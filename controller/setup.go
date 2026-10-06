package controller

import (
	"context"
	"net/http"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/darcys22/steadmesh/api/v1alpha1"
	"github.com/darcys22/steadmesh/controller/platform"
	"github.com/darcys22/steadmesh/pkg/compile"
	"github.com/darcys22/steadmesh/pkg/names"
	"github.com/darcys22/steadmesh/runtime/kube"
)

// Config configures the controllers.
type Config struct {
	// Platform is the internal API client.
	Platform platform.API
	// Backend options (platform URL, control-plane namespace, images).
	Backend kube.Options
	// PollInterval of the per-organisation runtime poller (default 2s).
	PollInterval time.Duration
	// InstructionHTTP fetches https instruction bundles (default 15s timeout).
	InstructionHTTP *http.Client
	// Now overrides the clock (tests).
	Now func() time.Time
}

// CacheOptions restricts the manager cache for seat-owned kinds to objects
// labelled steadmesh.io/component=seat, so the controller does not cache every
// Pod or ConfigMap in the cluster. Instruction ConfigMaps are read live.
func CacheOptions() cache.Options {
	sel := cache.ByObject{Label: labels.SelectorFromSet(labels.Set{v1alpha1.LabelComponent: kube.ComponentSeat})}
	return cache.Options{ByObject: map[client.Object]cache.ByObject{
		&corev1.Pod{}:                   sel,
		&corev1.ConfigMap{}:             sel,
		&corev1.ServiceAccount{}:        sel,
		&corev1.PersistentVolumeClaim{}: sel,
		&appsv1.StatefulSet{}:           sel,
		&networkingv1.NetworkPolicy{}:   sel,
	}}
}

// orgPredicate reconciles organisations on spec, annotation (verify
// requests), finalizer and deletion changes, but not on status-only updates.
var orgPredicate = predicate.Funcs{
	UpdateFunc: func(e event.UpdateEvent) bool {
		o, n := e.ObjectOld, e.ObjectNew
		if o.GetGeneration() != n.GetGeneration() || n.GetDeletionTimestamp() != nil {
			return true
		}
		return o.GetAnnotations()[v1alpha1.AnnotationVerifyRequest] != n.GetAnnotations()[v1alpha1.AnnotationVerifyRequest] ||
			len(o.GetFinalizers()) != len(n.GetFinalizers())
	},
}

// Reconcilers are the components registered by SetupReconcilers.
type Reconcilers struct {
	Organization *OrganizationReconciler
	Seat         *SeatReconciler
	Pollers      *Pollers
	Backend      *kube.Backend
}

// Setup registers the reconcilers, the runtime pollers and the backend with mgr.
func Setup(mgr ctrl.Manager, cfg Config) error {
	_, err := SetupReconcilers(mgr, cfg)
	return err
}

// SetupReconcilers is Setup, returning the registered components.
func SetupReconcilers(mgr ctrl.Manager, cfg Config) (*Reconcilers, error) {
	if cfg.PollInterval == 0 {
		cfg.PollInterval = 2 * time.Second
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	cfg.Backend.FieldOwner = FieldOwner
	c := client.WithFieldOwner(mgr.GetClient(), FieldOwner)
	backend := kube.New(c, mgr.GetAPIReader(), cfg.Backend)
	pollers := NewPollers(cfg.Platform, cfg.PollInterval)
	if err := mgr.Add(pollers); err != nil {
		return nil, err
	}
	rec := mgr.GetEventRecorder(FieldOwner)

	orgR := &OrganizationReconciler{
		Client: c, Platform: cfg.Platform, Backend: backend, Pollers: pollers,
		Instructions: NewInstructionResolver(mgr.GetAPIReader(), cfg.InstructionHTTP),
		Recorder:     rec, Catalog: compile.DefaultCatalog(), PlatformURL: backend.Options().PlatformURL, Now: cfg.Now,
		VerifyInterval: 5 * time.Minute, VerifyRetry: 30 * time.Second, SyncInterval: 5 * time.Minute,
		syncs: map[types.UID]syncEntry{}, verify: map[types.UID]verifyEntry{},
	}
	if err := ctrl.NewControllerManagedBy(mgr).Named("agentorganization").
		For(&v1alpha1.AgentOrganization{}, builder.WithPredicates(orgPredicate)).
		Owns(&v1alpha1.AgentSeat{}).
		Complete(orgR); err != nil {
		return nil, err
	}

	seatR := &SeatReconciler{
		Client: c, Backend: backend, Platform: cfg.Platform, Pollers: pollers, Recorder: rec, Now: cfg.Now,
		ProbeTimeout: 5 * time.Minute, ProbeRetry: time.Minute,
	}
	podToSeat := handler.EnqueueRequestsFromMapFunc(func(_ context.Context, o client.Object) []reconcile.Request {
		l := o.GetLabels()
		org, seat := l[v1alpha1.LabelOrganization], l[v1alpha1.LabelSeat]
		if org == "" || seat == "" {
			return nil
		}
		return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: o.GetNamespace(), Name: names.Seat(org, seat)}}}
	})
	err := ctrl.NewControllerManagedBy(mgr).Named("agentseat").
		For(&v1alpha1.AgentSeat{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&corev1.ConfigMap{}).
		Watches(&corev1.Pod{}, podToSeat).
		WatchesRawSource(source.Channel(pollers.Events, &handler.EnqueueRequestForObject{})).
		Complete(seatR)
	if err != nil {
		return nil, err
	}
	return &Reconcilers{Organization: orgR, Seat: seatR, Pollers: pollers, Backend: backend}, nil
}
