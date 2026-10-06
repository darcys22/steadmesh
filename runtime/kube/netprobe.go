package kube

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/darcys22/steadmesh/pkg/spec"
	seatruntime "github.com/darcys22/steadmesh/runtime"
)

// ProfileDigest identifies a sandbox profile's contents for probe caching.
func ProfileDigest(p spec.SandboxProfile) string {
	b, _ := json.Marshal(p)
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func probeKey(ns, digest string) string { return ns + "/" + digest }

// CachedEnforcement returns the cached enforcement result without starting a probe.
func (b *Backend) CachedEnforcement(ns, digest string) (seatruntime.ProbeResult, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	r, ok := b.probes[probeKey(ns, digest)]
	return r, ok
}

// VerifyEnforcement runs a short-lived Pod under the seat NetworkPolicy that
// must reach the platform and must not reach the denied URL. Results are
// cached per namespace and sandbox profile digest; a passed result is reused
// until a new verify nonce is requested, a failed one for ProbeFailureTTL.
func (b *Backend) VerifyEnforcement(ctx context.Context, p seatruntime.EnforcementProbe) (seatruntime.ProbeResult, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	key := probeKey(p.Namespace, p.ProfileDigest)
	now := b.now()
	cached, ok := b.probes[key]
	if ok && cached.Nonce == p.Nonce {
		if cached.State == seatruntime.ProbePassed || (cached.State == seatruntime.ProbeFailed && now.Sub(cached.Time) < b.ProbeFailureTTL) {
			return cached, nil
		}
	}
	pending := func(detail string) (seatruntime.ProbeResult, error) {
		return seatruntime.ProbeResult{State: seatruntime.ProbePending, Detail: detail, Nonce: p.Nonce, Time: now}, nil
	}
	if p.Seat == nil {
		return pending("no seat uses this sandbox profile yet")
	}
	// The probe is only meaningful once the seat policy it tests exists.
	var np networkingv1.NetworkPolicy
	if err := b.c.Get(ctx, types.NamespacedName{Namespace: p.Namespace, Name: p.Seat.Name}, &np); err != nil {
		if apierrors.IsNotFound(err) {
			return pending("waiting for seat network policy " + p.Seat.Name)
		}
		return seatruntime.ProbeResult{}, err
	}
	want := RenderProbePod(p, b.o)
	var pod corev1.Pod
	err := b.c.Get(ctx, client.ObjectKeyFromObject(want), &pod)
	if apierrors.IsNotFound(err) {
		if err := b.c.Create(ctx, want, client.FieldOwner(b.o.FieldOwner)); err != nil && !apierrors.IsAlreadyExists(err) {
			return seatruntime.ProbeResult{}, err
		}
		return pending("probe pod started")
	}
	if err != nil {
		return seatruntime.ProbeResult{}, err
	}
	if pod.DeletionTimestamp != nil {
		return pending("previous probe pod terminating")
	}
	if pod.Annotations["steadmesh.io/probe-nonce"] != p.Nonce {
		_ = b.c.Delete(ctx, &pod)
		return pending("replacing stale probe pod")
	}
	var res seatruntime.ProbeResult
	switch pod.Status.Phase {
	case corev1.PodSucceeded:
		res = seatruntime.ProbeResult{State: seatruntime.ProbePassed, Detail: "allowed platform route reachable; denied route blocked"}
	case corev1.PodFailed:
		msg := terminationMessage(&pod)
		if msg == "" {
			msg = pod.Status.Reason + " " + pod.Status.Message
		}
		res = seatruntime.ProbeResult{State: seatruntime.ProbeFailed, Detail: "network policy is not enforced: " + msg}
	default:
		if now.Sub(pod.CreationTimestamp.Time) < b.ProbeTimeout {
			return pending(fmt.Sprintf("probe pod %s", pod.Status.Phase))
		}
		res = seatruntime.ProbeResult{State: seatruntime.ProbeFailed, Detail: fmt.Sprintf("probe pod did not complete within %s (phase %s)", b.ProbeTimeout, pod.Status.Phase)}
	}
	res.Nonce, res.Time = p.Nonce, now
	b.probes[key] = res
	if err := b.c.Delete(ctx, &pod); err != nil && !apierrors.IsNotFound(err) {
		return res, nil
	}
	return res, nil
}

func terminationMessage(p *corev1.Pod) string {
	for _, cs := range p.Status.ContainerStatuses {
		if t := cs.State.Terminated; t != nil {
			if t.Message != "" {
				return t.Message
			}
			return fmt.Sprintf("exit code %d (%s)", t.ExitCode, t.Reason)
		}
	}
	return ""
}
