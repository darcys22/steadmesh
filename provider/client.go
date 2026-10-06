package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/darcys22/steadmesh/api/v1alpha1"
)

// FieldManager is the server-side apply field manager of the provider (§5.5).
const FieldManager = "terraform-provider-steadmesh"

// Annotations written by the provider.
const (
	ManagedByTerraform = "terraform"
	// AnnotationManifestDigest records the digest of the manifest the provider wrote.
	AnnotationManifestDigest = "steadmesh.io/manifest-digest"
	// AnnotationDeclaration records the declared (unresolved) organisation, so
	// that an import reproduces the Terraform configuration exactly.
	AnnotationDeclaration = "steadmesh.io/terraform-declaration"
)

var orgGVR = schema.GroupVersionResource{Group: v1alpha1.GroupVersion.Group, Version: v1alpha1.GroupVersion.Version, Resource: "agentorganizations"}

// orgClient is the provider's access to AgentOrganization objects.
type orgClient interface {
	Get(ctx context.Context, namespace, name string) (*v1alpha1.AgentOrganization, error)
	// Apply performs a non-forced server-side apply of the given object.
	Apply(ctx context.Context, obj map[string]any) (*v1alpha1.AgentOrganization, error)
	Delete(ctx context.Context, namespace, name string) error
}

type dynamicOrgClient struct {
	dyn dynamic.Interface
}

func (c *dynamicOrgClient) Get(ctx context.Context, namespace, name string) (*v1alpha1.AgentOrganization, error) {
	u, err := c.dyn.Resource(orgGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	return toTyped(u)
}

func (c *dynamicOrgClient) Apply(ctx context.Context, obj map[string]any) (*v1alpha1.AgentOrganization, error) {
	u := &unstructured.Unstructured{Object: obj}
	// Never force: conflicts with other field managers are surfaced (§5.5).
	out, err := c.dyn.Resource(orgGVR).Namespace(u.GetNamespace()).Apply(ctx, u.GetName(), u, metav1.ApplyOptions{FieldManager: FieldManager, Force: false})
	if err != nil {
		return nil, err
	}
	return toTyped(out)
}

func (c *dynamicOrgClient) Delete(ctx context.Context, namespace, name string) error {
	bg := metav1.DeletePropagationBackground
	return c.dyn.Resource(orgGVR).Namespace(namespace).Delete(ctx, name, metav1.DeleteOptions{PropagationPolicy: &bg})
}

func toTyped(u *unstructured.Unstructured) (*v1alpha1.AgentOrganization, error) {
	var org v1alpha1.AgentOrganization
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &org); err != nil {
		return nil, fmt.Errorf("decode AgentOrganization: %w", err)
	}
	return &org, nil
}

// restConfig loads a kubeconfig with an explicit context. The ambient
// current-context is never used: an empty context is rejected by the schema
// and a context missing from the file is an error.
func restConfig(kubeconfigPath, kubeContext string) (*rest.Config, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfigPath != "" {
		if rest, ok := strings.CutPrefix(kubeconfigPath, "~/"); ok {
			home, err := os.UserHomeDir()
			if err != nil {
				return nil, err
			}
			kubeconfigPath = filepath.Join(home, rest)
		}
		rules.ExplicitPath = kubeconfigPath
	}
	raw, err := rules.Load()
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig: %w", err)
	}
	if _, ok := raw.Contexts[kubeContext]; !ok {
		return nil, fmt.Errorf("context %q not found in kubeconfig %s", kubeContext, describePath(rules))
	}
	cc := clientcmd.NewNonInteractiveClientConfig(*raw, kubeContext, &clientcmd.ConfigOverrides{CurrentContext: kubeContext}, rules)
	cfg, err := cc.ClientConfig()
	if err != nil {
		return nil, err
	}
	cfg.UserAgent = "terraform-provider-steadmesh"
	return cfg, nil
}

func describePath(rules *clientcmd.ClientConfigLoadingRules) string {
	if rules.ExplicitPath != "" {
		return rules.ExplicitPath
	}
	b, _ := json.Marshal(rules.GetLoadingPrecedence())
	return string(b)
}

// checkAPI fails early when steadmesh.io/v1alpha1 agentorganizations is not
// served (§13.3).
func checkAPI(cfg *rest.Config) error {
	dc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return err
	}
	list, err := dc.ServerResourcesForGroupVersion(v1alpha1.GroupVersion.String())
	if err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("the cluster does not serve %s; install the platform chart (CRDs) before applying an organisation", v1alpha1.GroupVersion)
		}
		return fmt.Errorf("discover %s: %w", v1alpha1.GroupVersion, err)
	}
	for _, r := range list.APIResources {
		if r.Name == orgGVR.Resource {
			return nil
		}
	}
	return fmt.Errorf("the cluster serves %s but not the %s resource; the installed platform version is unsupported by this provider", v1alpha1.GroupVersion, orgGVR.Resource)
}
