package provider

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/darcys22/steadmesh/api/v1alpha1"
)

type metav1Condition = metav1.Condition

func sortConditions(c []metav1.Condition) {
	sort.SliceStable(c, func(i, j int) bool { return c[i].Type < c[j].Type })
}

// readiness reports whether the organisation's current generation is
// OperationalReady (§5.4). When it is not, it describes what blocks it: the
// OperationalReady reason and message plus every other False condition, so
// the operator sees the unresolved binding or connection (A02).
func readiness(org *v1alpha1.AgentOrganization) (bool, string) {
	if org.Status.ObservedGeneration != org.Generation {
		return false, fmt.Sprintf("the controller has not yet observed generation %d (observed %d)", org.Generation, org.Status.ObservedGeneration)
	}
	var ready *metav1.Condition
	for i := range org.Status.Conditions {
		if org.Status.Conditions[i].Type == v1alpha1.CondOperationalReady {
			ready = &org.Status.Conditions[i]
		}
	}
	if ready == nil {
		return false, "no OperationalReady condition has been reported"
	}
	if ready.ObservedGeneration != 0 && ready.ObservedGeneration != org.Generation {
		return false, fmt.Sprintf("OperationalReady describes generation %d, not the current generation %d", ready.ObservedGeneration, org.Generation)
	}
	if ready.Status == metav1.ConditionTrue {
		return true, ""
	}
	parts := []string{fmt.Sprintf("OperationalReady=%s (reason %s): %s", ready.Status, ready.Reason, ready.Message)}
	conds := append([]metav1.Condition(nil), org.Status.Conditions...)
	sortConditions(conds)
	for _, c := range conds {
		if c.Type != v1alpha1.CondOperationalReady && c.Status != metav1.ConditionTrue {
			parts = append(parts, fmt.Sprintf("%s=%s (reason %s): %s", c.Type, c.Status, c.Reason, c.Message))
		}
	}
	return false, strings.Join(parts, "; ")
}

// waitForReady polls until the organisation is ready or ctx expires. It
// returns the latest observed object in both cases. Transient read errors are
// retried; NotFound is fatal because the object was just written.
func waitForReady(ctx context.Context, c orgClient, ns, name string) (*v1alpha1.AgentOrganization, error) {
	var last *v1alpha1.AgentOrganization
	var lastErr error
	for {
		org, err := c.Get(ctx, ns, name)
		switch {
		case err == nil:
			last, lastErr = org, nil
			if ok, _ := readiness(org); ok {
				return org, nil
			}
		case apierrors.IsNotFound(err):
			return last, fmt.Errorf("AgentOrganization %s/%s disappeared while waiting for readiness", ns, name)
		case ctx.Err() == nil:
			lastErr = err
		}
		select {
		case <-ctx.Done():
			if last == nil {
				return nil, fmt.Errorf("timed out; last read error: %v", lastErr)
			}
			_, why := readiness(last)
			if lastErr != nil {
				why += fmt.Sprintf("; last read error: %v", lastErr)
			}
			return last, fmt.Errorf("timed out: %s", why)
		case <-time.After(pollInterval):
		}
	}
}
