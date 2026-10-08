package console

import (
	"math"
	"slices"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/darcys22/steadmesh/api/v1alpha1"
	"github.com/darcys22/steadmesh/pkg/runtimeapi"
)

// Seat states shown on the organisation view. They summarise the
// controller's ExecutionState, the platform lease and the inbox.
const (
	StateWorking  = "working"
	StateWaiting  = "waiting"
	StateQueued   = "queued"
	StateStarting = "starting"
	StateBlocked  = "blocked"
	StateOffline  = "offline"
	// StateRetiring is a seat removed from the organisation that is winding
	// down before it retires.
	StateRetiring = "retiring"
	StateUnknown  = "unknown"
)

// seatView joins one seat's configuration, platform state and cluster state.
type seatView struct {
	Config runtimeapi.ConsoleSeatConfig
	Status runtimeapi.ConsoleSeatStatus
	CRD    *v1alpha1.AgentSeat
	Pod    *corev1.Pod
	State  string
	// Alive reports whether a runner holds the seat's lease; it says nothing
	// about whether the seat is making progress.
	Alive     bool
	Attention []string
}

func newSeatView(cfg runtimeapi.ConsoleSeatConfig, st runtimeapi.ConsoleSeatStatus, crd *v1alpha1.AgentSeat, pod *corev1.Pod, now time.Time) seatView {
	v := seatView{Config: cfg, Status: st, CRD: crd, Pod: pod}
	v.Alive = st.LeaseHolder != "" && st.LeaseExpiresAt.After(now)
	v.State = seatState(st, crd, v.Alive)
	if st.DeadDeliveries > 0 {
		v.Attention = append(v.Attention, plural(st.DeadDeliveries, "dead-lettered message"))
	}
	if st.UnknownOperations > 0 {
		v.Attention = append(v.Attention, plural(st.UnknownOperations, "operation with unknown outcome"))
	}
	if crd != nil && crd.Status.LastError != "" {
		v.Attention = append(v.Attention, crd.Status.LastError)
	}
	if crd != nil && crd.Spec.ConfigRevision != "" && crd.Status.AdoptedRevision != "" && crd.Spec.ConfigRevision != crd.Status.AdoptedRevision {
		v.Attention = append(v.Attention, "running an older configuration revision")
	}
	return v
}

func seatState(st runtimeapi.ConsoleSeatStatus, crd *v1alpha1.AgentSeat, alive bool) string {
	if st.SeatID == "" && crd == nil {
		return StateUnknown
	}
	state := v1alpha1.ExecutionState(st.State)
	if crd != nil && crd.Status.ExecutionState != "" {
		state = crd.Status.ExecutionState
	}
	if crd != nil && crd.Spec.AdminSuspended {
		return StateOffline
	}
	if st.RetireBy != nil || state == v1alpha1.StateRetiring {
		return StateRetiring
	}
	switch {
	case st.Current != nil || state == v1alpha1.StateExecuting:
		return StateWorking
	case state == v1alpha1.StateBlocked:
		return StateBlocked
	case state == v1alpha1.StateProvisioning || state == v1alpha1.StateRecovering:
		return StateStarting
	case state == v1alpha1.StateWarm && alive:
		if st.PendingDeliveries > 0 {
			return StateQueued
		}
		return StateWaiting
	case state == v1alpha1.StateStopped || state == v1alpha1.StateSuspended || state == v1alpha1.StateQuiescing || !alive:
		if st.PendingDeliveries > 0 {
			return StateQueued
		}
		return StateOffline
	}
	return StateUnknown
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return strconv.Itoa(n) + " " + noun + "s"
}

// teamView is one group on the organisation view.
type teamView struct {
	Name  string
	Seats []seatView
}

// groupByTeam groups seats by team in the order of teams; a seat in several
// teams appears in each. Representatives and seats without a team follow.
func groupByTeam(teams []string, seats []seatView) []teamView {
	var out []teamView
	for _, t := range teams {
		tv := teamView{Name: t}
		for _, s := range seats {
			if slices.Contains(s.Config.Teams, t) {
				tv.Seats = append(tv.Seats, s)
			}
		}
		if len(tv.Seats) > 0 {
			out = append(out, tv)
		}
	}
	reps, rest := teamView{Name: "Representatives"}, teamView{Name: "No team"}
	for _, s := range seats {
		switch {
		case len(s.Config.Teams) > 0:
		case s.Config.IsRepresentative:
			reps.Seats = append(reps.Seats, s)
		default:
			rest.Seats = append(rest.Seats, s)
		}
	}
	for _, tv := range []teamView{reps, rest} {
		if len(tv.Seats) > 0 {
			out = append(out, tv)
		}
	}
	return out
}

// commMap is the communication map: seats on a circle, declared routes as
// edges. Edges are drawn once per unordered pair.
type commMap struct {
	Size  int
	Nodes []mapNode
	Edges []mapEdge
}

type mapNode struct {
	Key, Label, State string
	X, Y              float64
	Representative    bool
}

type mapEdge struct {
	ID             string
	X1, Y1, X2, Y2 float64
}

func edgeID(a, b string) string {
	if b < a {
		a, b = b, a
	}
	return "edge-" + a + "--" + b
}

func layoutMap(seats []seatView) commMap {
	const size = 520
	m := commMap{Size: size}
	n := len(seats)
	if n == 0 {
		return m
	}
	r := float64(size)/2 - 60
	pos := map[string][2]float64{}
	for i, s := range seats {
		a := 2*math.Pi*float64(i)/float64(n) - math.Pi/2
		x, y := float64(size)/2+r*math.Cos(a), float64(size)/2+r*math.Sin(a)
		if n == 1 {
			x, y = float64(size)/2, float64(size)/2
		}
		pos[s.Config.Key] = [2]float64{x, y}
		label := s.Config.DisplayName
		if label == "" {
			label = s.Config.Key
		}
		m.Nodes = append(m.Nodes, mapNode{Key: s.Config.Key, Label: label, State: s.State, X: x, Y: y,
			Representative: s.Config.IsRepresentative})
	}
	seen := map[string]bool{}
	for _, s := range seats {
		for _, to := range s.Config.SendTo {
			id := edgeID(s.Config.Key, to)
			p, q := pos[s.Config.Key], pos[to]
			if seen[id] || (q == [2]float64{}) {
				continue
			}
			seen[id] = true
			m.Edges = append(m.Edges, mapEdge{ID: id, X1: p[0], Y1: p[1], X2: q[0], Y2: q[1]})
		}
	}
	return m
}

// conditionView is a condition row with how long it has held.
type conditionView struct {
	Type, Status, Reason, Message string
	Since                         time.Time
}

func conditions(cs []metav1.Condition) []conditionView {
	out := make([]conditionView, 0, len(cs))
	for _, c := range cs {
		out = append(out, conditionView{Type: c.Type, Status: string(c.Status), Reason: c.Reason, Message: c.Message,
			Since: c.LastTransitionTime.Time})
	}
	return out
}

func operationalReady(org *v1alpha1.AgentOrganization) string {
	if org == nil {
		return "Unknown"
	}
	if c := meta.FindStatusCondition(org.Status.Conditions, v1alpha1.CondOperationalReady); c != nil {
		return string(c.Status)
	}
	return "Unknown"
}

// readinessRow compares one seat's requested and observed configuration.
type readinessRow struct {
	Seat       seatView
	Requested  string
	Adopted    string
	Probe      string
	ProbeError string
	Claim      string
	ClaimPhase string
	Tools      int
	Failing    []conditionView
}

func newReadinessRow(s seatView, claims map[string]corev1.PersistentVolumeClaimPhase) readinessRow {
	r := readinessRow{Seat: s, Requested: s.Config.ConfigRevision, Adopted: s.Status.AdoptedRevision}
	if s.CRD == nil {
		return r
	}
	if s.CRD.Spec.ConfigRevision != "" {
		r.Requested = s.CRD.Spec.ConfigRevision
	}
	if s.CRD.Status.AdoptedRevision != "" {
		r.Adopted = s.CRD.Status.AdoptedRevision
	}
	if p := s.CRD.Status.Probe; p != nil {
		r.Probe, r.ProbeError = p.Status, p.Detail
	}
	r.Claim = s.CRD.Spec.Workspace.ClaimName
	if r.Claim != "" {
		r.ClaimPhase = string(claims[r.Claim])
		if r.ClaimPhase == "" {
			r.ClaimPhase = "Missing"
		}
	}
	r.Tools = len(s.CRD.Spec.Tools)
	for _, c := range conditions(s.CRD.Status.Conditions) {
		if c.Status != string(metav1.ConditionTrue) {
			r.Failing = append(r.Failing, c)
		}
	}
	return r
}
