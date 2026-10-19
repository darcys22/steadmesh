package main

import (
	"context"
	"github.com/darcys22/steadmesh/harnesses"
	"github.com/darcys22/steadmesh/pkg/runtimeapi"
	"testing"
	"time"
)

type contextObservingAdapter struct {
	harnesses.Adapter
	p    *fakePlatform
	seen chan runtimeapi.ParticipantContext
}

func (a *contextObservingAdapter) Deliver(ctx context.Context, d harnesses.Delivery) (harnesses.TurnResult, error) {
	if d.Context != nil {
		a.seen <- *d.Context
	}
	a.p.mu.Lock()
	a.p.workingContext = runtimeapi.ParticipantContext{Revision: 2, Instructions: "new instructions", Summary: "new summary"}
	a.p.mu.Unlock()
	return a.Adapter.Deliver(ctx, d)
}
func TestRunnerRefreshesWorkingContextForRetainedSession(t *testing.T) {
	p := newFakePlatform()
	p.workingContext = runtimeapi.ParticipantContext{Revision: 1, Instructions: "old instructions"}
	seen := make(chan runtimeapi.ParticipantContext, 2)
	tr := newTestRunnerWith(t, p, nil, func(name string) (harnesses.Adapter, error) {
		a, err := DefaultAdapters(name)
		if err != nil {
			return nil, err
		}
		return &contextObservingAdapter{Adapter: a, p: p, seen: seen}, nil
	})
	p.waitFor(t, "state:Warm", 10*time.Second)
	p.enqueue("human", "first turn")
	p.enqueue("human", "second turn")
	for _, rev := range []int{1, 2} {
		select {
		case got := <-seen:
			if got.Revision != rev {
				t.Fatalf("revision=%d want=%d", got.Revision, rev)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("no delivery")
		}
	}
	tr.stop()
	if err := tr.wait(t, 15*time.Second); err != nil {
		t.Fatal(err)
	}
}
