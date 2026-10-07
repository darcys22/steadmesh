package readiness

import (
	"errors"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/darcys22/steadmesh/api/v1alpha1"
	"github.com/darcys22/steadmesh/pkg/runtimeapi"
	seatruntime "github.com/darcys22/steadmesh/runtime"
)

func allTrue(gen int64) []metav1.Condition {
	var out []metav1.Condition
	for _, t := range Order {
		out = append(out, Condition(t, true, "", "", gen))
	}
	return out
}

func TestAggregate(t *testing.T) {
	if c := Aggregate(allTrue(3), 3); c.Status != metav1.ConditionTrue || c.ObservedGeneration != 3 {
		t.Fatalf("all true: %+v", c)
	}
	// Stale generation blocks.
	if c := Aggregate(allTrue(2), 3); c.Status != metav1.ConditionFalse || c.Reason != v1alpha1.CondConfigured {
		t.Fatalf("stale: %+v", c)
	}
	// Missing condition blocks.
	conds := allTrue(3)
	meta.RemoveStatusCondition(&conds, v1alpha1.CondIngressReady)
	if c := Aggregate(conds, 3); c.Status != metav1.ConditionFalse || c.Reason != v1alpha1.CondIngressReady {
		t.Fatalf("missing: %+v", c)
	}
	// The first blocking condition names the reason and carries the detail.
	conds = allTrue(3)
	meta.SetStatusCondition(&conds, Condition(v1alpha1.CondConnectionsAuthenticated, false, "ConnectionUnauthenticated", "connection tracker: invalid api key", 3))
	meta.SetStatusCondition(&conds, Condition(v1alpha1.CondBindingsValid, false, "BindingInvalid", "binding sean: unknown user", 3))
	c := Aggregate(conds, 3)
	if c.Reason != v1alpha1.CondConnectionsAuthenticated || !strings.Contains(c.Message, "tracker") {
		t.Fatalf("blocking: %+v", c)
	}
	// Unknown is not ready.
	conds = allTrue(3)
	meta.SetStatusCondition(&conds, Unknown(v1alpha1.CondSandboxEnforced, "Probing", "", 3))
	if c := Aggregate(conds, 3); c.Status != metav1.ConditionFalse || c.Reason != v1alpha1.CondSandboxEnforced {
		t.Fatalf("unknown: %+v", c)
	}
}

func TestVerifyConditions(t *testing.T) {
	v := &runtimeapi.VerifyResponse{
		Connections: map[string]runtimeapi.CheckResult{"slack": {OK: true}, "tracker": {OK: false, Detail: "401"}, "optional": {OK: false}},
		Ingress:     map[string]runtimeapi.CheckResult{"slack": {OK: true}},
		Bindings:    map[string]runtimeapi.CheckResult{"sean": {OK: false, Detail: "user not found"}},
	}
	conds := VerifyConditions(v, map[string]bool{"slack": true, "tracker": true}, 1)
	byType := map[string]metav1.Condition{}
	for _, c := range conds {
		byType[c.Type] = c
	}
	cc := byType[v1alpha1.CondConnectionsAuthenticated]
	if cc.Status != metav1.ConditionFalse || !strings.Contains(cc.Message, "tracker: 401") || strings.Contains(cc.Message, "optional") {
		t.Fatalf("connections: %+v", cc)
	}
	if byType[v1alpha1.CondIngressReady].Status != metav1.ConditionTrue {
		t.Fatal("ingress should be ready")
	}
	if b := byType[v1alpha1.CondBindingsValid]; b.Status != metav1.ConditionFalse || !strings.Contains(b.Message, "sean") {
		t.Fatalf("bindings: %+v", b)
	}
}

func TestVerifyConditionsSurfaceDegradedCredentials(t *testing.T) {
	v := &runtimeapi.VerifyResponse{
		Connections: map[string]runtimeapi.CheckResult{"slack": {OK: true}, "optional": {OK: true}},
		Credentials: map[string]runtimeapi.CredentialStatus{
			"slack":    {State: runtimeapi.CredentialReplacementRejected, Error: "replacement rejected: invalid_auth"},
			"optional": {State: runtimeapi.CredentialRefreshFailing, Error: "vault sealed"},
		},
	}
	cc := VerifyConditions(v, map[string]bool{"slack": true}, 1)[0]
	if cc.Status != metav1.ConditionTrue || cc.Reason != "CredentialRefreshDegraded" ||
		!strings.Contains(cc.Message, "slack: replacement_rejected") || strings.Contains(cc.Message, "optional") {
		t.Fatalf("connections: %+v", cc)
	}
	v.Credentials["slack"] = runtimeapi.CredentialStatus{State: runtimeapi.CredentialCurrent}
	if cc := VerifyConditions(v, map[string]bool{"slack": true}, 1)[0]; cc.Reason != "Verified" {
		t.Fatalf("current credential: %+v", cc)
	}
}

func seat(conds map[string]metav1.ConditionStatus, probe *v1alpha1.SeatProbeStatus) *v1alpha1.AgentSeat {
	s := &v1alpha1.AgentSeat{}
	for t, st := range conds {
		meta.SetStatusCondition(&s.Status.Conditions, metav1.Condition{Type: t, Status: st, Reason: "R", Message: "m"})
	}
	s.Status.Probe = probe
	return s
}

func TestSeatConditions(t *testing.T) {
	good := map[string]metav1.ConditionStatus{SeatStorageReady: metav1.ConditionTrue, SeatBackendValid: metav1.ConditionTrue}
	passed := &v1alpha1.SeatProbeStatus{ID: "p", Revision: "r1", Status: ProbePassed}
	enf := map[string]seatruntime.ProbeResult{"standard": {State: seatruntime.ProbePassed}}

	get := func(conds []metav1.Condition, t string) metav1.Condition { return *meta.FindStatusCondition(conds, t) }

	conds := SeatConditions([]SeatView{{Key: "a", ConfigRevision: "r1", Seat: seat(good, passed)}}, enf, 1)
	for _, c := range conds {
		if c.Status != metav1.ConditionTrue {
			t.Fatalf("%s: %+v", c.Type, c)
		}
	}

	// Probe for an old revision is not current.
	conds = SeatConditions([]SeatView{{Key: "a", ConfigRevision: "r2", Seat: seat(good, passed)}}, enf, 1)
	if c := get(conds, v1alpha1.CondRoutesExecutable); c.Status != metav1.ConditionFalse || c.Reason != "ProbePending" {
		t.Fatalf("old revision: %+v", c)
	}
	// Probe for another nonce is not current.
	conds = SeatConditions([]SeatView{{Key: "a", ConfigRevision: "r1", Nonce: "n2", Seat: seat(good, passed)}}, enf, 1)
	if c := get(conds, v1alpha1.CondRoutesExecutable); c.Status != metav1.ConditionFalse {
		t.Fatalf("old nonce: %+v", c)
	}
	// Failed probe.
	failed := &v1alpha1.SeatProbeStatus{ID: "p", Revision: "r1", Status: ProbeFailed, Detail: "model unreachable"}
	conds = SeatConditions([]SeatView{{Key: "a", ConfigRevision: "r1", Seat: seat(good, failed)}}, enf, 1)
	if c := get(conds, v1alpha1.CondRoutesExecutable); c.Reason != "ProbeFailed" || !strings.Contains(c.Message, "a: synthetic probe failed: model unreachable") {
		t.Fatalf("failed: %+v", c)
	}
	// Unenforced network policy blocks the sandbox condition.
	bad := map[string]seatruntime.ProbeResult{"standard": {State: seatruntime.ProbeFailed, Detail: "denied url reachable"}}
	conds = SeatConditions([]SeatView{{Key: "a", ConfigRevision: "r1", Seat: seat(good, passed)}}, bad, 1)
	if c := get(conds, v1alpha1.CondSandboxEnforced); c.Status != metav1.ConditionFalse || !strings.Contains(c.Message, "denied url reachable") {
		t.Fatalf("sandbox: %+v", c)
	}
	// Backend validation failure.
	invalid := map[string]metav1.ConditionStatus{SeatStorageReady: metav1.ConditionTrue, SeatBackendValid: metav1.ConditionFalse}
	conds = SeatConditions([]SeatView{{Key: "a", ConfigRevision: "r1", Seat: seat(invalid, passed)}}, enf, 1)
	if c := get(conds, v1alpha1.CondSandboxEnforced); c.Status != metav1.ConditionFalse || !strings.Contains(c.Message, "a: R: m") {
		t.Fatalf("backend invalid: %+v", c)
	}
	// Harness failure only when explicitly False.
	h := map[string]metav1.ConditionStatus{SeatStorageReady: metav1.ConditionTrue, SeatBackendValid: metav1.ConditionTrue, SeatHarnessReady: metav1.ConditionFalse}
	conds = SeatConditions([]SeatView{{Key: "a", ConfigRevision: "r1", Seat: seat(h, passed)}}, enf, 1)
	if c := get(conds, v1alpha1.CondHarnessCompatible); c.Status != metav1.ConditionFalse {
		t.Fatalf("harness: %+v", c)
	}
	// Missing seat and ensure errors.
	conds = SeatConditions([]SeatView{{Key: "a", ConfigRevision: "r1"}, {Key: "b", Ensure: errors.New("boom")}}, enf, 1)
	if c := get(conds, v1alpha1.CondStorageReady); c.Status != metav1.ConditionFalse || !strings.Contains(c.Message, "a: seat not created yet") || !strings.Contains(c.Message, "b: boom") {
		t.Fatalf("missing: %+v", c)
	}
}
