// Command orgctl reports organisation status, runs a fresh readiness
// verification (design §5.4) and opens the optional Steadmesh Console. It is
// used by the deployment workflow after every apply, including applies with
// no Terraform changes (A25).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/google/uuid"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/darcys22/steadmesh/api/v1alpha1"
)

const (
	annotationVerifyRequest  = "steadmesh.io/verify-request"
	annotationVerifyObserved = "steadmesh.io/verify-observed"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "orgctl:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: orgctl status|verify|console [--context ctx] [--namespace ns] [--name org]")
	}
	cmd := args[0]
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	kubeconfig := fs.String("kubeconfig", "", "kubeconfig path (default: standard loading rules)")
	kctx := fs.String("context", os.Getenv("STEADMESH_KUBE_CONTEXT"), "kubeconfig context (required)")
	ns := fs.String("namespace", "", "organisation namespace (default: all)")
	name := fs.String("name", "", "organisation name (default: all in namespace)")
	timeout := fs.Duration("timeout", 10*time.Minute, "verify timeout")
	systemNS := fs.String("system-namespace", "steadmesh-system", "control-plane namespace (console)")
	port := fs.Int("port", 8090, "local port for the console")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *kctx == "" {
		return errors.New("--context is required; orgctl never uses the ambient context")
	}
	c, err := newClient(*kubeconfig, *kctx)
	if err != nil {
		return err
	}
	ctx := context.Background()
	if cmd == "console" {
		return openConsole(ctx, c, *kubeconfig, *kctx, *systemNS, *port)
	}
	orgs, err := listOrgs(ctx, c, *ns, *name)
	if err != nil {
		return err
	}
	if len(orgs) == 0 {
		return errors.New("no AgentOrganization found")
	}
	switch cmd {
	case "status":
		for i := range orgs {
			printStatus(&orgs[i])
		}
		return nil
	case "verify":
		var failed []string
		for i := range orgs {
			if err := verify(ctx, c, &orgs[i], *timeout); err != nil {
				failed = append(failed, fmt.Sprintf("%s/%s: %v", orgs[i].Namespace, orgs[i].Name, err))
			}
		}
		if len(failed) > 0 {
			for _, f := range failed {
				fmt.Fprintln(os.Stderr, "NOT READY", f)
			}
			return errors.New("readiness verification failed")
		}
		return nil
	default:
		return fmt.Errorf("unknown command %q", cmd)
	}
}

func newClient(kubeconfig, kctx string) (client.Client, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfig != "" {
		rules.ExplicitPath = kubeconfig
	}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{CurrentContext: kctx}).ClientConfig()
	if err != nil {
		return nil, err
	}
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	if err := appsv1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	return client.New(cfg, client.Options{Scheme: scheme})
}

func listOrgs(ctx context.Context, c client.Client, ns, name string) ([]v1alpha1.AgentOrganization, error) {
	if name != "" {
		if ns == "" {
			return nil, errors.New("--name requires --namespace")
		}
		var o v1alpha1.AgentOrganization
		if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &o); err != nil {
			return nil, err
		}
		return []v1alpha1.AgentOrganization{o}, nil
	}
	var list v1alpha1.AgentOrganizationList
	if err := c.List(ctx, &list, client.InNamespace(ns)); err != nil {
		return nil, err
	}
	return list.Items, nil
}

// verify requests a fresh verification pass and waits for the controller to
// report it for the current generation.
func verify(ctx context.Context, c client.Client, org *v1alpha1.AgentOrganization, timeout time.Duration) error {
	nonce := uuid.NewString()
	patch := fmt.Appendf(nil, `{"metadata":{"annotations":{%q:%q}}}`, annotationVerifyRequest, nonce)
	if err := c.Patch(ctx, org, client.RawPatch(types.MergePatchType, patch)); err != nil {
		return fmt.Errorf("request verification: %w", err)
	}
	fmt.Printf("verifying %s/%s (request %s)\n", org.Namespace, org.Name, nonce)
	deadline := time.Now().Add(timeout)
	for {
		var cur v1alpha1.AgentOrganization
		if err := c.Get(ctx, client.ObjectKeyFromObject(org), &cur); err != nil {
			return err
		}
		cond := meta.FindStatusCondition(cur.Status.Conditions, v1alpha1.CondOperationalReady)
		observed := cur.Annotations[annotationVerifyObserved] == nonce && cur.Status.ObservedGeneration == cur.Generation
		if observed && cond != nil {
			printStatus(&cur)
			if cond.Status == metav1.ConditionTrue {
				return nil
			}
			return fmt.Errorf("%s: %s", cond.Reason, cond.Message)
		}
		if time.Now().After(deadline) {
			printStatus(&cur)
			return fmt.Errorf("timed out after %s waiting for verification", timeout)
		}
		time.Sleep(2 * time.Second)
	}
}

func printStatus(o *v1alpha1.AgentOrganization) {
	fmt.Printf("\nOrganisation %s/%s  id=%s  generation=%d observed=%d  revision=%s\n",
		o.Namespace, o.Name, o.Status.OrganizationID, o.Generation, o.Status.ObservedGeneration, o.Status.EffectiveRevision)
	w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	fmt.Fprintln(w, "CONDITION\tSTATUS\tREASON\tMESSAGE")
	for _, c := range o.Status.Conditions {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", c.Type, c.Status, c.Reason, c.Message)
	}
	w.Flush()
	if len(o.Status.Seats) > 0 {
		keys := make([]string, 0, len(o.Status.Seats))
		for k := range o.Status.Seats {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		fmt.Fprintln(w, "\nSEAT\tSTATE\tREADY\tREVISION\tREASON")
		for _, k := range keys {
			s := o.Status.Seats[k]
			fmt.Fprintf(w, "%s\t%s\t%t\t%s\t%s\n", k, s.State, s.Ready, short(s.AdoptedRevision), s.Reason)
		}
		w.Flush()
	}
	if cd := o.Status.ConnectionDetails; cd != nil && len(cd.Representatives) > 0 {
		fmt.Fprintln(w, "\nHUMAN\tREPRESENTATIVE\tCHANNEL\tUSER")
		for _, k := range sortedKeys(cd.Representatives) {
			r := cd.Representatives[k]
			fmt.Fprintf(w, "%s\t%s\t%s (%s)\t%s\n", k, r.Seat, r.Connection, r.Adapter, r.ExternalUserID)
		}
		w.Flush()
	}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func short(rev string) string {
	if len(rev) > 19 {
		return rev[:19]
	}
	return rev
}
