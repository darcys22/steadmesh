package controller

import (
	"context"
	"reflect"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/darcys22/steadmesh/api/v1alpha1"
	"github.com/darcys22/steadmesh/controller/platform"
	"github.com/darcys22/steadmesh/pkg/names"
	"github.com/darcys22/steadmesh/pkg/runtimeapi"
)

// Pollers runs one platform runtime poller per organisation (not per seat).
// Each poll fetches GET /internal/v1/organizations/{id}/runtime and enqueues
// the AgentSeats whose runtime view changed through Events, a channel source.
type Pollers struct {
	API      platform.API
	Interval time.Duration
	// Stale is the age after which a runtime view is no longer used.
	Stale  time.Duration
	Events chan event.GenericEvent

	mu      sync.Mutex
	ctx     context.Context
	orgs    map[string]*orgPoller
	data    map[string]map[string]runtimeapi.SeatRuntime
	fetched map[string]time.Time
	now     func() time.Time
}

type orgPoller struct {
	orgKey, namespace string
	cancel            context.CancelFunc
}

func NewPollers(api platform.API, interval time.Duration) *Pollers {
	return &Pollers{
		API: api, Interval: interval, Stale: 15 * time.Second,
		Events:  make(chan event.GenericEvent, 1024),
		orgs:    map[string]*orgPoller{},
		data:    map[string]map[string]runtimeapi.SeatRuntime{},
		fetched: map[string]time.Time{},
		now:     time.Now,
	}
}

// NeedLeaderElection: only the leader polls.
func (p *Pollers) NeedLeaderElection() bool { return true }

// Start implements manager.Runnable.
func (p *Pollers) Start(ctx context.Context) error {
	p.mu.Lock()
	p.ctx = ctx
	for id, op := range p.orgs {
		p.launch(id, op)
	}
	p.mu.Unlock()
	<-ctx.Done()
	return nil
}

func (p *Pollers) launch(orgID string, op *orgPoller) {
	ctx, cancel := context.WithCancel(p.ctx)
	op.cancel = cancel
	go p.run(ctx, orgID, op.orgKey, op.namespace)
}

// Ensure starts (once) the poller of an organisation.
func (p *Pollers) Ensure(orgID, orgKey, namespace string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if op, ok := p.orgs[orgID]; ok && op.orgKey == orgKey && op.namespace == namespace {
		return
	} else if ok && op.cancel != nil {
		op.cancel()
	}
	op := &orgPoller{orgKey: orgKey, namespace: namespace}
	p.orgs[orgID] = op
	if p.ctx != nil {
		p.launch(orgID, op)
	}
}

// Stop stops an organisation's poller and forgets its data.
func (p *Pollers) Stop(orgID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if op, ok := p.orgs[orgID]; ok && op.cancel != nil {
		op.cancel()
	}
	delete(p.orgs, orgID)
	delete(p.data, orgID)
	delete(p.fetched, orgID)
}

// Lookup returns the latest runtime view of a seat and whether the
// organisation's view is fresh; a fresh view without the seat means the
// platform has no active seat under that key.
func (p *Pollers) Lookup(orgID, seatKey string) (*runtimeapi.SeatRuntime, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if t, ok := p.fetched[orgID]; !ok || p.now().Sub(t) > p.Stale {
		return nil, false
	}
	r, ok := p.data[orgID][seatKey]
	if !ok {
		return nil, true
	}
	return &r, true
}

// Get returns the latest fresh runtime view of a seat.
func (p *Pollers) Get(orgID, seatKey string) (*runtimeapi.SeatRuntime, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if t, ok := p.fetched[orgID]; !ok || p.now().Sub(t) > p.Stale {
		return nil, false
	}
	r, ok := p.data[orgID][seatKey]
	if !ok {
		return nil, false
	}
	return &r, true
}

func (p *Pollers) run(ctx context.Context, orgID, orgKey, ns string) {
	log := logf.FromContext(ctx).WithValues("organizationID", orgID, "organization", orgKey, "namespace", ns)
	t := time.NewTicker(p.Interval)
	defer t.Stop()
	failures := 0
	for {
		p.poll(ctx, log, orgID, orgKey, ns, &failures)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (p *Pollers) poll(ctx context.Context, log interface {
	Info(string, ...any)
	Error(error, string, ...any)
}, orgID, orgKey, ns string, failures *int) {
	rctx, cancel := context.WithTimeout(ctx, p.Interval*5)
	defer cancel()
	resp, err := p.API.Runtime(rctx, orgID)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		*failures++
		if *failures == 1 || *failures%30 == 0 {
			log.Error(err, "poll platform runtime", "failures", *failures)
		}
		return
	}
	*failures = 0
	p.mu.Lock()
	if _, ok := p.orgs[orgID]; !ok {
		p.mu.Unlock()
		return
	}
	prev := p.data[orgID]
	p.data[orgID] = resp.Seats
	wasStale := p.now().Sub(p.fetched[orgID]) > p.Stale
	p.fetched[orgID] = p.now()
	p.mu.Unlock()
	for key, sr := range resp.Seats {
		if old, ok := prev[key]; ok && !wasStale && reflect.DeepEqual(old, sr) {
			continue
		}
		obj := &v1alpha1.AgentSeat{ObjectMeta: metav1.ObjectMeta{Name: names.Seat(orgKey, key), Namespace: ns}}
		select {
		case p.Events <- event.GenericEvent{Object: obj}:
		default:
			// The seat's periodic requeue picks the change up.
		}
	}
}
