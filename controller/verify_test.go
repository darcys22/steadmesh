package controller

import (
	"errors"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/darcys22/steadmesh/api/v1alpha1"
	"github.com/darcys22/steadmesh/controller/readiness"
	seatruntime "github.com/darcys22/steadmesh/runtime"
)

func TestVerifyFresh(t *testing.T) {
	const nonce, rev = "n1", "rev"
	enf := map[string]seatruntime.ProbeResult{"standard": {State: seatruntime.ProbePassed, Nonce: nonce}}
	seat := func(probe string, backend *metav1.Condition) *v1alpha1.AgentSeat {
		s := &v1alpha1.AgentSeat{}
		s.Status.Probe = &v1alpha1.SeatProbeStatus{Revision: rev, Nonce: nonce, Status: probe}
		if backend != nil {
			s.Status.Conditions = []metav1.Condition{*backend}
		}
		return s
	}
	view := func(s *v1alpha1.AgentSeat, err error) readiness.SeatView {
		return readiness.SeatView{Key: "a", SandboxProfile: "standard", ConfigRevision: rev, Nonce: nonce, Seat: s, Ensure: err}
	}
	conflict := apierrors.NewConflict(schema.GroupResource{Resource: "agentseats"}, "a", errors.New("modified"))
	unavailable := &metav1.Condition{Type: readiness.SeatBackendValid, Status: metav1.ConditionFalse, Reason: string(seatruntime.Unavailable)}
	unsupported := &metav1.Condition{Type: readiness.SeatBackendValid, Status: metav1.ConditionFalse, Reason: string(seatruntime.Unsupported)}

	cases := []struct {
		name string
		view readiness.SeatView
		want bool
	}{
		{"probe passed", view(seat(readiness.ProbePassed, nil), nil), true},
		{"probe failed is terminal", view(seat(readiness.ProbeFailed, nil), nil), true},
		{"probe pending", view(seat(readiness.ProbePending, nil), nil), false},
		{"ensure conflict is transient", view(nil, conflict), false},
		{"permanent ensure error is reported, not awaited", view(nil, errors.New("invalid")), true},
		{"backend unavailable is transient", view(seat(readiness.ProbePending, unavailable), nil), false},
		{"backend unsupported is terminal", view(seat(readiness.ProbePending, unsupported), nil), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := verifyFresh([]readiness.SeatView{tc.view}, enf, nonce, nonce); got != tc.want {
				t.Fatalf("verifyFresh = %v, want %v", got, tc.want)
			}
		})
	}
}
