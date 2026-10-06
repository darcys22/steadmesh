//go:build integration

package provider

// Acceptance tests: terraform-plugin-testing drives the real terraform binary
// against an envtest API server with the platform CRDs installed. No
// controller runs; status is simulated where a test needs readiness.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ktypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/darcys22/steadmesh/api/v1alpha1"
)

const accNS = "steadmesh-example"

var (
	accDyn            dynamic.Interface
	accAdminKubecfg   string
	accNobodyKubecfg  string
	accProtoFactories = map[string]func() (tfprotov6.ProviderServer, error){
		"steadmesh": providerserver.NewProtocol6WithError(New("acc")()),
	}
)

func TestMain(m *testing.M) {
	os.Exit(runWithEnvtest(m))
}

func runWithEnvtest(m *testing.M) int {
	pollInterval = 200 * time.Millisecond
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		out, err := exec.Command("../bin/setup-envtest", "use", "1.37.0", "-p", "path", "--bin-dir", "../bin/envtest").Output()
		if err != nil {
			fmt.Fprintf(os.Stderr, "setup-envtest: %v\n", err)
			return 1
		}
		os.Setenv("KUBEBUILDER_ASSETS", strings.TrimSpace(string(out)))
	}
	env := &envtest.Environment{CRDDirectoryPaths: []string{"../charts/platform/crds"}, ErrorIfCRDPathMissing: true}
	cfg, err := env.Start()
	if err != nil {
		fmt.Fprintf(os.Stderr, "envtest: %v\n", err)
		return 1
	}
	defer env.Stop()
	if accDyn, err = dynamic.NewForConfig(cfg); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	dir, err := os.MkdirTemp("", "steadmesh-acc")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer os.RemoveAll(dir)
	write := func(name string, user envtest.User) string {
		u, err := env.AddUser(user, nil)
		if err != nil {
			panic(err)
		}
		b, err := u.KubeConfig()
		if err != nil {
			panic(err)
		}
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, b, 0o600); err != nil {
			panic(err)
		}
		return p
	}
	accAdminKubecfg = write("admin.kubeconfig", envtest.User{Name: "tf-admin", Groups: []string{"system:masters"}})
	accNobodyKubecfg = write("nobody.kubeconfig", envtest.User{Name: "nobody"})

	ns := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": accNS}}}
	if _, err := accDyn.Resource(schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}).Create(context.Background(), ns, metav1.CreateOptions{}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	os.Setenv("TF_ACC", "1")
	return m.Run()
}

type accOpts struct {
	key, displayName string
	kubeconfig       string
	wait             *bool
	extraSeat        bool
	timeouts         string
}

func accConfig(o accOpts) string {
	if o.kubeconfig == "" {
		o.kubeconfig = accAdminKubecfg
	}
	if o.displayName == "" {
		o.displayName = "Acceptance Org"
	}
	wait := ""
	if o.wait != nil {
		wait = fmt.Sprintf("wait_for_ready = %t", *o.wait)
	}
	extra := ""
	if o.extraSeat {
		extra = `
      engineer = {
        role_ref          = "role:member"
        teams             = ["engineering"]
        harness_profile   = "fake"
        execution_profile = "interactive"
        sandbox_profile   = "standard"
      }`
	}
	tmo := ""
	if o.timeouts != "" {
		tmo = "timeouts {\n" + o.timeouts + "\n}"
	}
	ref := func(name string) string { return fmt.Sprintf("configmap:%s#sha256:%s", name, strings.Repeat("a", 64)) }
	return fmt.Sprintf(`
provider "steadmesh" {
  kubeconfig_path = %q
  kube_context    = "envtest"
  namespace       = %q
}

resource "steadmesh_organization" "test" {
  key          = %q
  display_name = %q
  %s
  spec = {
    culture_refs = [%q]
    team_templates = {
      base = {
        instruction_refs = [%q]
        roles            = { member = %q }
      }
      engineering = {
        extends          = "base"
        instruction_refs = [%q]
        roles            = { reviewer = %q }
        shared_memory    = { engineering = ["read", "search", "write", "revise"] }
      }
    }
    teams = { engineering = { template = "engineering" } }
    memory_stores = {
      organisation = {}
      engineering  = {}
      rep_sean     = {}
      reviewer     = {}
    }
    harness_profiles   = { fake = { adapter = "fake", image_digest = "steadmesh/seat-fake:dev" } }
    execution_profiles = { interactive = { backend = "kubernetes" } }
    sandbox_profiles   = { standard = {} }
    connections = {
      slack   = { adapter = "slack", account_id = "T0000", secret_ref = "k8s:slack-credentials" }
      tracker = { adapter = "linear", secret_ref = "k8s:linear-credentials", config = { team_id = "TEAM" } }
    }
    seats = {
      representative_sean = {
        role_ref          = %q
        harness_profile   = "fake"
        execution_profile = "interactive"
        sandbox_profile   = "standard"
        personal_memory   = "rep_sean"
      }
      reviewer = {
        role_ref          = "role:reviewer"
        teams             = ["engineering"]
        harness_profile   = "fake"
        execution_profile = "interactive"
        sandbox_profile   = "standard"
        personal_memory   = "reviewer"
        workspace         = { persistent = true }
      }%s
    }
    grants = {
      reviewer_tracker = { subject = "seat:reviewer", resource = "connection:tracker", operations = ["project.create", "task.write"] }
      org_memory       = { subject = "seat:representative_sean", resource = "memory:organisation", operations = ["read", "search"] }
    }
    message_routes = {
      rep_to_reviewer = { from = "seat:representative_sean", to = "seat:reviewer", reply = true }
    }
    channel_bindings = {
      sean = { connection = "slack", external_user_id = "U0SEAN", seat = "representative_sean" }
    }
  }
  %s
}
`, o.kubeconfig, accNS, o.key, o.displayName, wait,
		ref("culture/culture.md"), ref("team-base/base.md"), ref("roles/member.md"), ref("team-eng/engineering.md"),
		ref("roles/reviewer.md"), ref("roles/representative.md"), extra, tmo)
}

func boolPtr(b bool) *bool { return &b }

func orgs() dynamic.ResourceInterface { return accDyn.Resource(orgGVR).Namespace(accNS) }

// simulateController patches status as a controller would, once per
// generation, until the test ends.
func simulateController(t *testing.T, key string, ready bool) {
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	t.Cleanup(func() { cancel(); wg.Wait() })
	go func() {
		defer wg.Done()
		for ctx.Err() == nil {
			time.Sleep(100 * time.Millisecond)
			u, err := orgs().Get(ctx, key, metav1.GetOptions{})
			if err != nil {
				continue
			}
			observed, _, _ := unstructured.NestedInt64(u.Object, "status", "observedGeneration")
			if observed == u.GetGeneration() {
				continue
			}
			now := metav1.Now().UTC().Format(time.RFC3339)
			cond := func(typ, status, reason, msg string) map[string]any {
				return map[string]any{"type": typ, "status": status, "reason": reason, "message": msg, "lastTransitionTime": now, "observedGeneration": u.GetGeneration()}
			}
			conds := []any{cond(v1alpha1.CondOperationalReady, "True", "Ready", "organisation is ready"), cond(v1alpha1.CondConfigured, "True", "Accepted", "")}
			if !ready {
				conds = []any{
					cond(v1alpha1.CondOperationalReady, "False", "ConnectionsAuthenticatedFalse", "blocked by ConnectionsAuthenticated"),
					cond(v1alpha1.CondConnectionsAuthenticated, "False", "Unauthorized", "connection tracker: linear rejected api_key"),
				}
			}
			status := map[string]any{
				"observedGeneration": u.GetGeneration(),
				"organizationID":     "org-" + key,
				"effectiveRevision":  fmt.Sprintf("rev-%d", u.GetGeneration()),
				"conditions":         conds,
				"connectionDetails": map[string]any{
					"organizationID": "org-" + key,
					"namespace":      accNS,
					"representatives": map[string]any{
						"sean": map[string]any{"seat": "representative_sean", "seatID": "seat-1", "connection": "slack", "adapter": "slack", "externalUserID": "U0SEAN", "mode": "direct_message"},
					},
				},
			}
			body, _ := json.Marshal(map[string]any{"status": status})
			_, _ = orgs().Patch(ctx, key, ktypes.MergePatchType, body, metav1.PatchOptions{}, "status")
		}
	}()
}

func checkDestroyed(key string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		_, err := orgs().Get(context.Background(), key, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("organisation %s still exists (err=%v)", key, err)
	}
}

func checkObject(key string, f func(*v1alpha1.AgentOrganization) error) resource.TestCheckFunc {
	return func(*terraform.State) error {
		u, err := orgs().Get(context.Background(), key, metav1.GetOptions{})
		if err != nil {
			return err
		}
		org, err := toTyped(u)
		if err != nil {
			return err
		}
		return f(org)
	}
}

const addr = "steadmesh_organization.test"

func TestAccOrganizationLifecycle(t *testing.T) {
	key := "acc-life"
	simulateController(t, key, true)
	base := accOpts{key: key}
	updated := accOpts{key: key, displayName: "Acceptance Org v2", extraSeat: true}
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: accProtoFactories,
		CheckDestroy:             checkDestroyed(key),
		Steps: []resource.TestStep{
			{ // Create and wait for readiness.
				Config: accConfig(base),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(addr, "id", "org-"+key),
					resource.TestMatchResourceAttr(addr, "manifest_digest", regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)),
					resource.TestCheckResourceAttr(addr, "resolved_seats.%", "2"),
					resource.TestMatchResourceAttr(addr, "resolved_seats.reviewer.role_ref", regexp.MustCompile(`roles/reviewer\.md`)),
					resource.TestCheckResourceAttr(addr, "resolved_seats.reviewer.teams.0", "engineering"),
					resource.TestCheckResourceAttr(addr, "connection_details.representatives.sean.external_user_id", "U0SEAN"),
					resource.TestCheckResourceAttr(addr, "effective_revision", "rev-1"),
					checkObject(key, func(o *v1alpha1.AgentOrganization) error {
						if o.Annotations[v1alpha1.AnnotationManagedBy] != "terraform" {
							return fmt.Errorf("managed-by = %q", o.Annotations[v1alpha1.AnnotationManagedBy])
						}
						if o.Spec.TeamTemplates != nil || len(o.Spec.Teams["engineering"].InstructionRefs) != 2 {
							return fmt.Errorf("spec is not resolved: %+v", o.Spec.Teams)
						}
						for _, mf := range o.ManagedFields {
							if mf.Manager == FieldManager && mf.Operation == metav1.ManagedFieldsOperationApply {
								return nil
							}
						}
						return fmt.Errorf("no apply entry for field manager %s", FieldManager)
					}),
				),
			},
			{ // A03: repeating the same configuration plans nothing.
				Config:            accConfig(base),
				ConfigPlanChecks:  resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()}},
				ConfigStateChecks: nil,
			},
			{ // In-place update; the plan shows the affected seats.
				Config: accConfig(updated),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(addr, plancheck.ResourceActionUpdate),
				}},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(addr, "id", "org-"+key),
					resource.TestCheckResourceAttr(addr, "resolved_seats.%", "3"),
					resource.TestMatchResourceAttr(addr, "resolved_seats.engineer.role_ref", regexp.MustCompile(`roles/member\.md`)),
					resource.TestCheckResourceAttr(addr, "effective_revision", "rev-2"),
					checkObject(key, func(o *v1alpha1.AgentOrganization) error {
						if o.Spec.DisplayName != "Acceptance Org v2" || o.Generation != 2 {
							return fmt.Errorf("display_name=%q generation=%d", o.Spec.DisplayName, o.Generation)
						}
						return nil
					}),
				),
			},
			{ // Import by namespace/key reproduces the state.
				ResourceName:      addr,
				ImportState:       true,
				ImportStateId:     accNS + "/" + key,
				ImportStateVerify: true,
			},
			{ // A read error (forbidden) is an error and does not drop state.
				Config:      accConfig(accOpts{key: key, displayName: "Acceptance Org v2", extraSeat: true, kubeconfig: accNobodyKubecfg}),
				PlanOnly:    true,
				ExpectError: regexp.MustCompile(`Cannot read AgentOrganization`),
			},
			{ // State survived the failed read.
				Config:           accConfig(updated),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()}},
			},
			{ // A confirmed NotFound removes the resource, so the plan recreates it.
				PreConfig: func() {
					if err := orgs().Delete(context.Background(), key, metav1.DeleteOptions{}); err != nil {
						t.Fatal(err)
					}
				},
				Config:             accConfig(updated),
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
			{ // Re-create after out-of-band deletion.
				Config: accConfig(updated),
				Check:  resource.TestCheckResourceAttr(addr, "resolved_seats.%", "3"),
			},
		},
	})
}

func TestAccReadinessTimeoutPreservesState(t *testing.T) {
	key := "acc-timeout"
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: accProtoFactories,
		CheckDestroy:             checkDestroyed(key),
		Steps: []resource.TestStep{
			{ // No controller: create times out with an actionable error.
				Config:      accConfig(accOpts{key: key, timeouts: `create = "3s"`}),
				ExpectError: regexp.MustCompile(`(?s)did\s+not\s+become\s+OperationalReady.*(not\s+yet\s+observed\s+generation|no\s+OperationalReady).*recorded\s+in\s+state.*untaint`),
			},
			{ // The identity is preserved and nothing was cleaned up. Terraform core
				// taints a resource whose create reported an error, so a replacement
				// is planned until the operator runs `terraform untaint`.
				RefreshState:       true,
				ExpectNonEmptyPlan: true,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet(addr, "id"),
					resource.TestMatchResourceAttr(addr, "manifest_digest", regexp.MustCompile(`^sha256:`)),
					checkObject(key, func(*v1alpha1.AgentOrganization) error { return nil }),
				),
			},
		},
	})
}

func TestAccUpdateBlockedReportsCondition(t *testing.T) {
	key := "acc-blocked"
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: accProtoFactories,
		CheckDestroy:             checkDestroyed(key),
		Steps: []resource.TestStep{
			{Config: accConfig(accOpts{key: key, wait: boolPtr(false)})},
			{ // A02: a blocked connection fails the apply with its reason.
				PreConfig:   func() { simulateController(t, key, false) },
				Config:      accConfig(accOpts{key: key, displayName: "Blocked", timeouts: `update = "4s"`}),
				ExpectError: regexp.MustCompile(`(?s)ConnectionsAuthenticated=False.*linear\s+rejected\s+api_key`),
			},
			{ // Update errors keep the written state: the next plan is clean.
				Config:           accConfig(accOpts{key: key, displayName: "Blocked", timeouts: `update = "4s"`}),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()}},
				Check:            resource.TestCheckResourceAttr(addr, "conditions.0.reason", "Unauthorized"),
			},
		},
	})
}

func TestAccDriftAndConflict(t *testing.T) {
	key := "acc-drift"
	cfg := accConfig(accOpts{key: key, wait: boolPtr(false)})
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: accProtoFactories,
		CheckDestroy:             checkDestroyed(key),
		Steps: []resource.TestStep{
			{Config: cfg},
			{ // An out-of-band edit of a declared field shows as a change.
				PreConfig: func() {
					u, err := orgs().Get(context.Background(), key, metav1.GetOptions{})
					if err != nil {
						t.Fatal(err)
					}
					_ = unstructured.SetNestedField(u.Object, "Edited elsewhere", "spec", "display_name")
					if _, err := orgs().Update(context.Background(), u, metav1.UpdateOptions{FieldManager: "intruder"}); err != nil {
						t.Fatal(err)
					}
				},
				Config:             cfg,
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
			{ // The provider does not force ownership; the conflict is explicit.
				Config:      cfg,
				ExpectError: regexp.MustCompile(`(?s)Field ownership conflict.*intruder`),
			},
		},
	})
}

func TestAccImportRejectsHelmOwned(t *testing.T) {
	key := "acc-helm"
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": v1alpha1.GroupVersion.String(), "kind": "AgentOrganization",
		"metadata": map[string]any{"name": key, "namespace": accNS, "annotations": map[string]any{v1alpha1.AnnotationManagedBy: "helm"}},
		"spec":     map[string]any{"key": key, "display_name": "Helm Org"},
	}}
	if _, err := orgs().Create(context.Background(), obj, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = orgs().Delete(context.Background(), key, metav1.DeleteOptions{}) })
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: accProtoFactories,
		Steps: []resource.TestStep{{
			Config:        accConfig(accOpts{key: key, wait: boolPtr(false)}),
			ResourceName:  addr,
			ImportState:   true,
			ImportStateId: accNS + "/" + key,
			ExpectError:   regexp.MustCompile(`owned\s+by\s+helm`),
		}},
	})
}

// TestAccImportPlansClean seeds the cluster with exactly the object the
// provider writes, imports it into an empty state, and checks that the
// matching configuration then plans nothing.
func TestAccImportPlansClean(t *testing.T) {
	key := "acc-import"
	cfg := accConfig(accOpts{key: key, wait: boolPtr(false)})
	cfgDefaults := accConfig(accOpts{key: key})
	var seeded map[string]any
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: accProtoFactories,
		Steps: []resource.TestStep{{
			Config: cfg,
			Check: checkObject(key, func(o *v1alpha1.AgentOrganization) error {
				b, err := json.Marshal(o.Spec)
				if err != nil {
					return err
				}
				var spec map[string]any
				if err := json.Unmarshal(b, &spec); err != nil {
					return err
				}
				seeded = map[string]any{
					"apiVersion": v1alpha1.GroupVersion.String(), "kind": "AgentOrganization",
					"metadata": map[string]any{"name": key, "namespace": accNS, "annotations": map[string]any{
						v1alpha1.AnnotationManagedBy: o.Annotations[v1alpha1.AnnotationManagedBy],
						AnnotationManifestDigest:     o.Annotations[AnnotationManifestDigest],
						AnnotationDeclaration:        o.Annotations[AnnotationDeclaration],
					}},
					"spec": spec,
				}
				return nil
			}),
		}},
	})
	if seeded == nil {
		t.Fatal("seed object not captured")
	}
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: accProtoFactories,
		CheckDestroy:             checkDestroyed(key),
		Steps: []resource.TestStep{
			{
				PreConfig: func() {
					if _, err := (&dynamicOrgClient{dyn: accDyn}).Apply(context.Background(), seeded); err != nil {
						t.Fatal(err)
					}
				},
				// wait_for_ready and timeouts are provider behaviour, not part of
				// the object; an import assumes their defaults.
				Config:             cfgDefaults,
				ResourceName:       addr,
				ImportState:        true,
				ImportStateId:      accNS + "/" + key,
				ImportStatePersist: true,
				ImportStateCheck: func(s []*terraform.InstanceState) error {
					if len(s) != 1 || s[0].Attributes["key"] != key || s[0].Attributes["spec.seats.reviewer.role_ref"] != "role:reviewer" {
						return fmt.Errorf("unexpected import state: %v", s)
					}
					return nil
				},
			},
			{
				Config:           cfgDefaults,
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()}},
			},
		},
	})
}
