package controller

import (
	"context"
	"fmt"
	"time"

	"github.com/darcys22/steadmesh/controller/platform"
	seatruntime "github.com/darcys22/steadmesh/runtime"
	"github.com/darcys22/steadmesh/runtime/kube"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// reconcileTaskWorkers does not change the persistent seat's replica count,
// lease or workspace. Its Pod resources use the controller's existing RBAC.
func (r *SeatReconciler) reconcileTaskWorkers(ctx context.Context, s *seatruntime.Seat, eligible bool) error {
	api, ok := r.Platform.(platform.TaskRunsAPI)
	if !ok {
		return nil
	}
	backend, ok := r.Backend.(*kube.Backend)
	if !ok {
		return nil
	}
	runs, err := api.TaskRuns(ctx, s.SeatID)
	if err != nil {
		return err
	}
	for _, t := range runs {
		key := types.NamespacedName{Namespace: s.Namespace, Name: t.ServiceAccount}
		var pod corev1.Pod
		reader := r.Live
		if reader == nil {
			reader = r.Client
		}
		err := reader.Get(ctx, key, &pod)
		if err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		exists := err == nil
		if !eligible && !t.Terminal() {
			if err := api.TaskRunAction(ctx, s.SeatID, t.ID, "cancel"); err != nil {
				return err
			}
			// Refetch on the next pass. A launch may have acquired its fence
			// after our list snapshot but before this cancellation committed.
			continue
		}
		if t.Terminal() {
			if exists {
				if pod.DeletionTimestamp == nil {
					if err := r.Client.Delete(ctx, &pod); client.IgnoreNotFound(err) != nil {
						return err
					}
				}
				continue
			}
			// A missing Pod does not prove that a delayed Create cannot arrive.
			// Only the launch owner can resolve its durable one-shot fence. Crash
			// or ambiguous API errors intentionally retain the capacity reservation.
			if t.LaunchStartedAt != nil && t.LaunchResolvedAt == nil {
				continue
			}
			// Confirmed no Pod and no outstanding launch: remove policy and identity.
			for _, obj := range []client.Object{&corev1.ServiceAccount{}, &networkingv1.NetworkPolicy{}} {
				obj.SetNamespace(key.Namespace)
				obj.SetName(key.Name)
				if err := r.Client.Delete(ctx, obj); client.IgnoreNotFound(err) != nil {
					return err
				}
			}
			if err := api.TaskRunAction(ctx, s.SeatID, t.ID, "cleaned"); err != nil {
				return err
			}
			continue
		}
		if exists {
			if pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded || pod.DeletionTimestamp != nil {
				if err := api.TaskRunAction(ctx, s.SeatID, t.ID, "failed"); err != nil {
					return err
				}
			}
			continue
		}
		// Never restart a claimed worker: a replacement must be a new explicit run.
		if t.PodUID != "" || t.LaunchResolvedAt != nil {
			if err := api.TaskRunAction(ctx, s.SeatID, t.ID, "failed"); err != nil {
				return err
			}
			continue
		}
		if t.LaunchStartedAt != nil {
			// Never retry an unresolved create, even after a controller restart.
			continue
		}
		if !time.Now().Before(t.Deadline) {
			continue
		}
		// Platform sync can commit a new revision before this reconciler observes
		// the matching manifest. Never start a worker under an older image/runtime.
		if t.ConfigRevision != s.ConfigRevision {
			continue
		}
		desired, sa, np, err := kube.RenderTaskWorker(s, t, backend.Options())
		if err != nil {
			return err
		}
		// Fence before *any* runtime write so cancellation cannot clean up policy
		// or identity while an old reconciliation is still creating resources.
		if err := api.TaskRunAction(ctx, s.SeatID, t.ID, "launch"); err != nil {
			return fmt.Errorf("task worker %s launch fence: %w", t.ID, err)
		}
		// Establish default-deny policy and separate identity before the Pod.
		for _, obj := range []client.Object{np, sa, desired} {
			if err := r.Client.Create(ctx, obj); err != nil && !apierrors.IsAlreadyExists(err) {
				return fmt.Errorf("task worker %s: %w", t.ID, err)
			}
		}
		// Do not resolve any failed/ambiguous Create: it could commit later.
		// No observer may resolve this fence merely because it sees a Pod.
		if err := api.TaskRunAction(ctx, s.SeatID, t.ID, "launched"); err != nil {
			return fmt.Errorf("task worker %s launch resolution: %w", t.ID, err)
		}
	}
	return nil
}
