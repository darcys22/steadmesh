package secretref

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"

	"github.com/darcys22/steadmesh/connectors"
)

type kubernetesResolver struct {
	cs        kubernetes.Interface
	namespace string
}

// NewKubernetes resolves "k8s:<name>" to the data of Secret <name> in
// namespace. It also implements connectors.SecretWatcher, which needs list
// and watch on Secrets in that namespace.
func NewKubernetes(clientset kubernetes.Interface, namespace string) connectors.Secrets {
	return &kubernetesResolver{cs: clientset, namespace: namespace}
}

// Resolve reads the Secret directly rather than from the watch cache, so a
// refresh always sees the latest stored value.
func (k *kubernetesResolver) Resolve(ctx context.Context, ref string) (connectors.Resolved, error) {
	name, err := expect(ref, SchemeKubernetes)
	if err != nil {
		return connectors.Resolved{}, err
	}
	if errs := validation.IsDNS1123Subdomain(name); len(errs) > 0 {
		return connectors.Resolved{}, fmt.Errorf("%w: invalid secret name %q", connectors.ErrPermanent, name)
	}
	s, err := k.cs.CoreV1().Secrets(k.namespace).Get(ctx, name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		return connectors.Resolved{}, fmt.Errorf("%w: secret %s/%s not found", connectors.ErrPermanent, k.namespace, name)
	case apierrors.IsForbidden(err), apierrors.IsUnauthorized(err):
		return connectors.Resolved{}, fmt.Errorf("%w: cannot read secret %s/%s", connectors.ErrUnauthorized, k.namespace, name)
	case err != nil:
		return connectors.Resolved{}, fmt.Errorf("%w: reading secret %s/%s: %v", connectors.ErrRetryable, k.namespace, name, err)
	}
	out := make(map[string]string, len(s.Data))
	for key, v := range s.Data {
		out[key] = string(v)
	}
	return resolved(out, s.ResourceVersion), nil
}

// Watch reports every add, update or delete of a Secret in the namespace as
// "k8s:<name>". The caller decides whether the reference is in use.
func (k *kubernetesResolver) Watch(ctx context.Context, notify func(ref string)) error {
	f := informers.NewSharedInformerFactoryWithOptions(k.cs, 10*time.Minute, informers.WithNamespace(k.namespace))
	inf := f.Core().V1().Secrets().Informer()
	name := func(obj any) string {
		if d, ok := obj.(cache.DeletedFinalStateUnknown); ok {
			obj = d.Obj
		}
		if s, ok := obj.(*corev1.Secret); ok {
			return s.Name
		}
		return ""
	}
	report := func(obj any) {
		if n := name(obj); n != "" {
			notify(SchemeKubernetes + ":" + n)
		}
	}
	if _, err := inf.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    report,
		UpdateFunc: func(_, obj any) { report(obj) },
		DeleteFunc: report,
	}); err != nil {
		return err
	}
	f.Start(ctx.Done())
	<-ctx.Done()
	f.Shutdown()
	return nil
}
