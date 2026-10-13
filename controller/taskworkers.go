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
			t.State = "cancelled"
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
			// Confirmed no Pod: remove policy and identity, then record cleanup.
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
		if t.PodUID != "" {
			if err := api.TaskRunAction(ctx, s.SeatID, t.ID, "failed"); err != nil {
				return err
			}
			continue
		}
		if !time.Now().Before(t.Deadline) {
			continue
		}
		desired, sa, np, err := kube.RenderTaskWorker(s, t, backend.Options())
		if err != nil {
			return err
		}
		// Establish default-deny policy and separate identity before the Pod.
		for _, obj := range []client.Object{np, sa, desired} {
			if err := r.Client.Create(ctx, obj); err != nil && !apierrors.IsAlreadyExists(err) {
				return fmt.Errorf("task worker %s: %w", t.ID, err)
			}
		}
	}
	return nil
}
