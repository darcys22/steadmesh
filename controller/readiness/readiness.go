// Package readiness computes organisation readiness conditions (design §5.4,
// contracts.md "Readiness flow"). All functions are pure.
package readiness

import (
	"fmt"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/darcys22/steadmesh/api/v1alpha1"
	"github.com/darcys22/steadmesh/pkg/runtimeapi"
	seatruntime "github.com/darcys22/steadmesh/runtime"
)

// Order is the evaluation order of the conditions that OperationalReady
// requires; the first one that is not True names the blocking reason.
var Order = []string{
	v1alpha1.CondConfigured,
	v1alpha1.CondIdentitiesReady,
	v1alpha1.CondStorageReady,
	v1alpha1.CondHarnessCompatible,
	v1alpha1.CondSandboxEnforced,
	v1alpha1.CondConnectionsAuthenticated,
	v1alpha1.CondIngressReady,
	v1alpha1.CondBindingsValid,
	v1alpha1.CondRoutesExecutable,
}

// Seat condition types on AgentSeat.
const (
	SeatStorageReady = "StorageReady"
	SeatBackendValid = "BackendValid"
	SeatHarnessReady = "HarnessReady"
	// SeatAccessApplied is true when the seat runs the revision its access
	// profiles need (image tools, NetworkPolicy, proxy, browser).
	SeatAccessApplied = "AccessApplied"
	SeatReady         = "Ready"
)

// Probe states.
const (
	ProbePending = "pending"
	ProbePassed  = "passed"
	ProbeFailed  = "failed"
)

// Condition builds a condition for generation gen.
func Condition(t string, ok bool, reason, msg string, gen int64) metav1.Condition {
	s := metav1.ConditionFalse
	if ok {
		s = metav1.ConditionTrue
	}
	if reason == "" {
		if ok {
			reason = "Ready"
		} else {
			reason = "NotReady"
		}
	}
	return metav1.Condition{Type: t, Status: s, Reason: reason, Message: truncate(msg, 1024), ObservedGeneration: gen}
}

// Unknown builds an Unknown condition.
func Unknown(t, reason, msg string, gen int64) metav1.Condition {
	return metav1.Condition{Type: t, Status: metav1.ConditionUnknown, Reason: reason, Message: truncate(msg, 1024), ObservedGeneration: gen}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-3] + "..."
}

// Aggregate computes OperationalReady from the other conditions. It is True
// only when every condition in Order is True for generation gen; otherwise
// the reason is the first blocking condition type and the message carries
// that condition's reason and message (which name the seat, binding or
// connection involved, A02).
func Aggregate(conds []metav1.Condition, gen int64) metav1.Condition {
	for _, t := range Order {
		c := meta.FindStatusCondition(conds, t)
		switch {
		case c == nil:
			return Condition(v1alpha1.CondOperationalReady, false, t, t+" has not been evaluated", gen)
		case c.ObservedGeneration != gen:
			return Condition(v1alpha1.CondOperationalReady, false, t, fmt.Sprintf("%s has not been evaluated for generation %d", t, gen), gen)
		case c.Status != metav1.ConditionTrue:
			return Condition(v1alpha1.CondOperationalReady, false, t, fmt.Sprintf("%s: %s: %s", t, c.Reason, c.Message), gen)
		}
	}
	return Condition(v1alpha1.CondOperationalReady, true, "Ready", "all readiness conditions are true", gen)
}

// failures renders failing keys as "key: detail; key2: detail2".
func failures(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + ": " + m[k]
	}
	return strings.Join(parts, "; ")
}

func checkCondition(t, noun, failReason string, results map[string]runtimeapi.CheckResult, include func(string) bool, gen int64) metav1.Condition {
	bad := map[string]string{}
	n := 0
	for k, r := range results {
		if include != nil && !include(k) {
			continue
		}
		n++
		if !r.OK {
			d := r.Detail
			if d == "" {
				d = "check failed"
			}
			bad[k] = d
		}
	}
	if len(bad) > 0 {
		return Condition(t, false, failReason, fmt.Sprintf("%s %s", noun, failures(bad)), gen)
	}
	return Condition(t, true, "Verified", fmt.Sprintf("%d %s verified", n, noun), gen)
}

// VerifyConditions maps a platform verify response to the connection,
// ingress and binding conditions. Only required connections block readiness.
func VerifyConditions(v *runtimeapi.VerifyResponse, required map[string]bool, gen int64) []metav1.Condition {
	conns := checkCondition(v1alpha1.CondConnectionsAuthenticated, "connection", "ConnectionUnauthenticated", v.Connections, func(k string) bool { return required[k] }, gen)
	if conns.Status == metav1.ConditionTrue {
		// Working now, but a rotated credential could not be applied or the
		// secret store is failing: still ready, with the reason surfaced.
		degraded := map[string]string{}
		for k, c := range v.Credentials {
			if required[k] && c.Degraded() {
				degraded[k] = c.State + ": " + c.Error
			}
		}
		if len(degraded) > 0 {
			conns = Condition(v1alpha1.CondConnectionsAuthenticated, true, "CredentialRefreshDegraded",
				conns.Message+"; credential refresh degraded: "+failures(degraded), gen)
		}
	}
	return []metav1.Condition{
		conns,
		checkCondition(v1alpha1.CondIngressReady, "ingress", "IngressUnavailable", v.Ingress, func(k string) bool { return required[k] }, gen),
		checkCondition(v1alpha1.CondBindingsValid, "binding", "BindingInvalid", v.Bindings, nil, gen),
		integrations(v, required, gen),
	}
}

// SeatView is what the organisation needs to know about one seat.
type SeatView struct {
	Key            string
	SandboxProfile string
	ConfigRevision string
	// Nonce is the verify nonce the seat probe must answer ("" when none).
	Nonce  string
	Seat   *v1alpha1.AgentSeat // nil when not yet created
	Ensure error               // error while materialising the seat
}

func seatCond(s SeatView, t string) *metav1.Condition {
	if s.Seat == nil {
		return nil
	}
	return meta.FindStatusCondition(s.Seat.Status.Conditions, t)
}

// ProbeCurrent reports whether a seat probe answers the revision and nonce.
func ProbeCurrent(p *v1alpha1.SeatProbeStatus, revision, nonce string) bool {
	return p != nil && p.Revision == revision && p.Nonce == nonce
}

// SeatConditions derives StorageReady, HarnessCompatible, SandboxEnforced and
// RoutesExecutable from the seats and the per-profile enforcement probes.
func SeatConditions(seats []SeatView, enforcement map[string]seatruntime.ProbeResult, gen int64) []metav1.Condition {
	storage, harness, sandbox, routes := map[string]string{}, map[string]string{}, map[string]string{}, map[string]string{}
	pendingRoutes := map[string]string{}
	for _, s := range seats {
		if s.Ensure != nil {
			storage[s.Key] = s.Ensure.Error()
			sandbox[s.Key] = s.Ensure.Error()
			routes[s.Key] = s.Ensure.Error()
			continue
		}
		if s.Seat == nil {
			storage[s.Key], routes[s.Key] = "seat not created yet", "seat not created yet"
			continue
		}
		if c := seatCond(s, SeatStorageReady); c == nil || c.Status != metav1.ConditionTrue {
			storage[s.Key] = condDetail(c, "workspace not provisioned")
		}
		if c := seatCond(s, SeatHarnessReady); c != nil && c.Status == metav1.ConditionFalse {
			harness[s.Key] = condDetail(c, "")
		}
		if c := seatCond(s, SeatAccessApplied); c != nil && c.Status == metav1.ConditionFalse {
			harness[s.Key] = "access: " + condDetail(c, "")
		}
		if c := seatCond(s, SeatBackendValid); c == nil || c.Status != metav1.ConditionTrue {
			sandbox[s.Key] = condDetail(c, "sandbox not validated")
		}
		p := s.Seat.Status.Probe
		switch {
		case !ProbeCurrent(p, s.ConfigRevision, s.Nonce):
			pendingRoutes[s.Key] = "synthetic probe for revision " + short(s.ConfigRevision) + " not run yet"
		case p.Status == ProbePassed:
		case p.Status == ProbeFailed:
			routes[s.Key] = "synthetic probe failed: " + p.Detail
		default:
			pendingRoutes[s.Key] = "synthetic probe " + p.ID + " pending"
		}
	}
	profiles := make([]string, 0, len(enforcement))
	for k := range enforcement {
		profiles = append(profiles, k)
	}
	sort.Strings(profiles)
	for _, k := range profiles {
		if r := enforcement[k]; r.State != seatruntime.ProbePassed {
			sandbox["sandbox_profile "+k] = "network policy enforcement " + string(r.State) + ": " + r.Detail
		}
	}

	out := []metav1.Condition{}
	add := func(t string, bad map[string]string, failReason, okMsg string) {
		if len(bad) > 0 {
			out = append(out, Condition(t, false, failReason, failures(bad), gen))
		} else {
			out = append(out, Condition(t, true, "Ready", okMsg, gen))
		}
	}
	add(v1alpha1.CondStorageReady, storage, "StorageNotReady", fmt.Sprintf("%d seat workspaces provisioned", len(seats)))
	add(v1alpha1.CondHarnessCompatible, harness, "HarnessUnavailable", "harness profiles compatible")
	add(v1alpha1.CondSandboxEnforced, sandbox, "SandboxNotEnforced", "sandbox restrictions verified")
	switch {
	case len(routes) > 0:
		add(v1alpha1.CondRoutesExecutable, routes, "ProbeFailed", "")
	case len(pendingRoutes) > 0:
		out = append(out, Condition(v1alpha1.CondRoutesExecutable, false, "ProbePending", failures(pendingRoutes), gen))
	default:
		out = append(out, Condition(v1alpha1.CondRoutesExecutable, true, "Verified", fmt.Sprintf("%d seats passed the synthetic execution probe", len(seats)), gen))
	}
	return out
}

func condDetail(c *metav1.Condition, fallback string) string {
	if c == nil {
		return fallback
	}
	if c.Message != "" {
		return c.Reason + ": " + c.Message
	}
	return c.Reason
}

func short(rev string) string {
	if len(rev) > 19 {
		return rev[:19]
	}
	return rev
}

// integrations reports failing optional connections. It is informational:
// it is not in Order, so it never blocks OperationalReady, and agents keep
// working while an optional integration such as a work tracker is down.
func integrations(v *runtimeapi.VerifyResponse, required map[string]bool, gen int64) metav1.Condition {
	bad := map[string]string{}
	for k, r := range v.Connections {
		if required[k] || r.OK {
			continue
		}
		bad[k] = r.Detail
		if bad[k] == "" {
			bad[k] = "check failed"
		}
	}
	for k, r := range v.Ingress {
		if !required[k] && !r.OK {
			bad[k] = "ingress: " + r.Detail
		}
	}
	if len(bad) > 0 {
		return Condition(v1alpha1.CondIntegrationsDegraded, true, "OptionalConnectionUnavailable",
			"optional connection "+failures(bad)+"; agents keep working without it", gen)
	}
	return Condition(v1alpha1.CondIntegrationsDegraded, false, "Healthy", "optional connections are healthy", gen)
}
