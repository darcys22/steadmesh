package console

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/darcys22/steadmesh/api/v1alpha1"
	"github.com/darcys22/steadmesh/runtime/kube"
)

// Cluster is the console's read-only view of the Kubernetes resources: what
// Terraform requested (AgentOrganization, AgentSeat specs), what the
// controller observed (their status) and what is running (Pods, claims).
type Cluster interface {
	// Organization returns the AgentOrganization with the given platform
	// organisation id, or nil.
	Organization(ctx context.Context, orgID string) (*v1alpha1.AgentOrganization, error)
	// Seats returns the organisation's AgentSeats keyed by seat id.
	Seats(ctx context.Context, org *v1alpha1.AgentOrganization) (map[string]v1alpha1.AgentSeat, error)
	// Pods returns the organisation's seat runner Pods keyed by seat key.
	Pods(ctx context.Context, org *v1alpha1.AgentOrganization) (map[string]corev1.Pod, error)
	// Claims returns the phases of the namespace's seat volume claims by name.
	Claims(ctx context.Context, namespace string) (map[string]corev1.PersistentVolumeClaimPhase, error)
}

// KubeCluster implements Cluster from an informer cache limited to Steadmesh
// resources and seat-labelled Pods and claims.
type KubeCluster struct {
	c client.Reader
}

// NewKubeCluster starts an informer cache and waits up to syncTimeout for it
// to sync. It stops when ctx is cancelled.
func NewKubeCluster(ctx context.Context, cfg *rest.Config, syncTimeout time.Duration) (*KubeCluster, error) {
	scheme := k8sruntime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return nil, err
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	seatObjects := cache.ByObject{Label: labels.SelectorFromSet(labels.Set{v1alpha1.LabelComponent: kube.ComponentSeat})}
	c, err := cache.New(cfg, cache.Options{Scheme: scheme, ByObject: map[client.Object]cache.ByObject{
		&corev1.Pod{}:                   seatObjects,
		&corev1.PersistentVolumeClaim{}: seatObjects,
	}})
	if err != nil {
		return nil, err
	}
	// Register the informers before starting so the sync covers them.
	for _, obj := range []client.Object{&v1alpha1.AgentOrganization{}, &v1alpha1.AgentSeat{}, &corev1.Pod{}, &corev1.PersistentVolumeClaim{}} {
		if _, err := c.GetInformer(ctx, obj); err != nil {
			return nil, fmt.Errorf("informer %T: %w", obj, err)
		}
	}
	go func() { _ = c.Start(ctx) }()
	syncCtx, cancel := context.WithTimeout(ctx, syncTimeout)
	defer cancel()
	if !c.WaitForCacheSync(syncCtx) {
		return nil, fmt.Errorf("cluster cache did not sync within %s; check the console's RBAC", syncTimeout)
	}
	return &KubeCluster{c: c}, nil
}

func (k *KubeCluster) Organization(ctx context.Context, orgID string) (*v1alpha1.AgentOrganization, error) {
	var list v1alpha1.AgentOrganizationList
	if err := k.c.List(ctx, &list); err != nil {
		return nil, err
	}
	for i := range list.Items {
		if list.Items[i].Status.OrganizationID == orgID {
			return &list.Items[i], nil
		}
	}
	return nil, nil
}

func (k *KubeCluster) Seats(ctx context.Context, org *v1alpha1.AgentOrganization) (map[string]v1alpha1.AgentSeat, error) {
	var list v1alpha1.AgentSeatList
	if err := k.c.List(ctx, &list, client.InNamespace(org.Namespace)); err != nil {
		return nil, err
	}
	out := map[string]v1alpha1.AgentSeat{}
	for _, s := range list.Items {
		if s.Spec.OrganizationName == org.Name && s.Spec.SeatID != "" {
			out[s.Spec.SeatID] = s
		}
	}
	return out, nil
}

func (k *KubeCluster) Pods(ctx context.Context, org *v1alpha1.AgentOrganization) (map[string]corev1.Pod, error) {
	var list corev1.PodList
	if err := k.c.List(ctx, &list, client.InNamespace(org.Namespace), client.MatchingLabels{
		v1alpha1.LabelOrganization: org.Spec.Key, kube.LabelWorkload: kube.WorkloadRunner}); err != nil {
		return nil, err
	}
	out := map[string]corev1.Pod{}
	for _, p := range list.Items {
		out[p.Labels[v1alpha1.LabelSeat]] = p
	}
	return out, nil
}

func (k *KubeCluster) Claims(ctx context.Context, namespace string) (map[string]corev1.PersistentVolumeClaimPhase, error) {
	var list corev1.PersistentVolumeClaimList
	if err := k.c.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	out := map[string]corev1.PersistentVolumeClaimPhase{}
	for _, c := range list.Items {
		out[c.Name] = c.Status.Phase
	}
	return out, nil
}
