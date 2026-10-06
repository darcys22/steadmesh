package secretref

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"

	"github.com/darcys22/steadmesh/connectors"
)

type kubernetesResolver struct {
	cs        kubernetes.Interface
	namespace string
}

// NewKubernetes resolves "k8s:<name>" to the data of Secret <name> in namespace.
func NewKubernetes(clientset kubernetes.Interface, namespace string) connectors.Secrets {
	return &kubernetesResolver{cs: clientset, namespace: namespace}
}

func (k *kubernetesResolver) Resolve(ctx context.Context, ref string) (map[string]string, error) {
	name, err := expect(ref, SchemeKubernetes)
	if err != nil {
		return nil, err
	}
	if errs := validation.IsDNS1123Subdomain(name); len(errs) > 0 {
		return nil, fmt.Errorf("%w: invalid secret name %q", connectors.ErrPermanent, name)
	}
	s, err := k.cs.CoreV1().Secrets(k.namespace).Get(ctx, name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		return nil, fmt.Errorf("%w: secret %s/%s not found", connectors.ErrPermanent, k.namespace, name)
	case apierrors.IsForbidden(err), apierrors.IsUnauthorized(err):
		return nil, fmt.Errorf("%w: cannot read secret %s/%s", connectors.ErrUnauthorized, k.namespace, name)
	case err != nil:
		return nil, fmt.Errorf("%w: reading secret %s/%s: %v", connectors.ErrRetryable, k.namespace, name, err)
	}
	out := make(map[string]string, len(s.Data))
	for key, v := range s.Data {
		out[key] = string(v)
	}
	return out, nil
}
