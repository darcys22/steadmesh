//go:build integration

package publisher_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/darcys22/steadmesh/connectors"
	"github.com/darcys22/steadmesh/pkg/spec"
	"github.com/darcys22/steadmesh/services/internal/orgfixture"
	"github.com/darcys22/steadmesh/services/internal/pgtest"
	"github.com/darcys22/steadmesh/services/publisher"
	"github.com/darcys22/steadmesh/services/store"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

// recorder is a tracker that keeps the objects it holds, keyed by the
// operation marker it was created with, like Linear's read-back.
type recorder struct {
	mu        sync.Mutex
	objects   map[string]map[string]any // id -> fields
	byOp      map[string]string         // operation id -> object id
	creates   int
	updates   int
	ambiguous bool // next create lands but reports a lost response
}

func (r *recorder) Verify(context.Context) error { return nil }
func (r *recorder) ReadOnly(string) bool         { return false }

func (r *recorder) Invoke(_ context.Context, _ string, params json.RawMessage, opID string) (connectors.Result, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var p map[string]any
	_ = json.Unmarshal(params, &p)
	if id, ok := p["id"].(string); ok {
		r.updates++
		r.objects[id] = p
		return connectors.Result{Receipt: id, Data: json.RawMessage(`{"id":"` + id + `"}`)}, nil
	}
	r.creates++
	id := fmt.Sprintf("ISSUE-%d", len(r.objects)+1)
	r.objects[id], r.byOp[opID] = p, id
	if r.ambiguous {
		r.ambiguous = false
		return connectors.Result{}, fmt.Errorf("connection reset: %w", connectors.ErrAmbiguous)
	}
	return connectors.Result{Receipt: id, Data: json.RawMessage(`{"id":"` + id + `","url":"https://tracker/` + id + `"}`)}, nil
}

func (r *recorder) FindByOperation(_ context.Context, _ string, _ json.RawMessage, opID string) (*connectors.Result, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if id, ok := r.byOp[opID]; ok {
		return &connectors.Result{Receipt: id, Data: json.RawMessage(`{"id":"` + id + `"}`)}, nil
	}
	return nil, nil
}

type trackers struct {
	t    connectors.Tracker
	down bool
}

func (t *trackers) Tracker(string, string) (connectors.Tracker, error) {
	if t.down {
		return nil, errors.New("connection not available: linear unreachable")
	}
	return t.t, nil
}

type fixture struct {
	t     *testing.T
	ctx   context.Context
	s     *store.Store
	org   string
	store string
	fence store.Fence
	seat  string
}

func setup(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	s, err := store.Open(ctx, pgtest.URL(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	sp := orgfixture.Spec()
	sp.WorkPublication = &spec.WorkPublication{Connection: "tracker", Stores: []string{"engineering"}}
	res, err := s.SyncOrganization(ctx, store.SyncInput{Namespace: "acme", Key: "acme", SourceUID: "u", Manifest: orgfixture.Compile(t, sp)})
	if err != nil {
		t.Fatal(err)
	}
	lead := res.Seats["lead"].SeatID
	l, err := s.AcquireLease(ctx, lead, "pod")
	if err != nil {
		t.Fatal(err)
	}
	ids, _ := s.ActiveStoreIDs(ctx, res.OrganizationID, []string{"engineering"})
	return &fixture{t: t, ctx: ctx, s: s, org: res.OrganizationID, store: ids["engineering"],
		fence: store.Fence{SeatID: lead, Generation: l.Generation}, seat: lead}
}

func (f *fixture) work(objective string) *store.Record {
	f.t.Helper()
	rec, err := f.s.CreateStructured(f.ctx, f.fence, f.org, f.store, f.seat, store.KindWork, "work", "W-",
		func(n int64) (json.RawMessage, string, error) {
			b, _ := json.Marshal(map[string]any{"id": fmt.Sprintf("engineering/W-%d", n), "objective": objective, "status": "ready"})
			return b, "# " + objective, nil
		})
	if err != nil {
		f.t.Fatal(err)
	}
	return rec
}

func (f *fixture) revise(rec *store.Record, status string) *store.Record {
	f.t.Helper()
	out, err := f.s.ReviseStructured(f.ctx, f.fence, f.org, rec.ID, f.seat, store.KindWork, rec.Revision, []string{f.store},
		func(data json.RawMessage) (json.RawMessage, string, error) {
			var m map[string]any
			_ = json.Unmarshal(data, &m)
			m["status"] = status
			b, _ := json.Marshal(m)
			return b, "status: " + status, nil
		})
	if err != nil {
		f.t.Fatal(err)
	}
	return out
}

func (f *fixture) publication(recordID string) store.Publication {
	f.t.Helper()
	pubs, err := f.s.Publications(f.ctx, f.org, 50)
	if err != nil {
		f.t.Fatal(err)
	}
	for _, p := range pubs {
		if p.RecordID == recordID {
			return p
		}
	}
	f.t.Fatalf("no publication for %s", recordID)
	return store.Publication{}
}

// due is a no-op marker: retries have no backoff in these tests.
func (f *fixture) due() {}

func newPublisher(tr *trackers) *publisher.Publisher {
	return &publisher.Publisher{Trackers: tr, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Interval: time.Hour,
		Backoff: func(int) time.Duration { return 0 }, ClaimLease: time.Nanosecond}
}

func TestPublishBlockedThenPublishedThenUpdated(t *testing.T) {
	f := setup(t)
	rec := &recorder{objects: map[string]map[string]any{}, byOp: map[string]string{}}
	tr := &trackers{t: rec, down: true}
	p := newPublisher(tr)
	p.Store = f.s
	w := f.work("Build the login page")

	// Tracker down: the publication is blocked; the work item is untouched.
	if err := p.Once(f.ctx); err != nil {
		t.Fatal(err)
	}
	if pub := f.publication(w.ID); pub.State != store.PubBlocked || !strings.Contains(pub.LastError, "unreachable") {
		t.Fatalf("while down: %+v", pub)
	}
	// Tracker back: published once.
	tr.down = false
	f.due()
	if err := p.Once(f.ctx); err != nil {
		t.Fatal(err)
	}
	pub := f.publication(w.ID)
	if pub.State != store.PubPublished || pub.ExternalID != "ISSUE-1" || pub.PublishedRevision != w.Revision || rec.creates != 1 {
		t.Fatalf("published: %+v creates=%d", pub, rec.creates)
	}
	// Nothing changes, nothing is sent.
	f.due()
	_ = p.Once(f.ctx)
	if rec.creates != 1 || rec.updates != 0 {
		t.Fatalf("idle pass sent requests: creates=%d updates=%d", rec.creates, rec.updates)
	}
	// A new revision updates the same object.
	w = f.revise(w, "in_progress")
	f.due()
	_ = p.Once(f.ctx)
	pub = f.publication(w.ID)
	if rec.creates != 1 || rec.updates != 1 || pub.PublishedRevision != w.Revision || !strings.Contains(rec.objects["ISSUE-1"]["description"].(string), "in_progress") {
		t.Fatalf("update: %+v creates=%d updates=%d", pub, rec.creates, rec.updates)
	}
}

func TestCrashBetweenCreateAndSavingIDConverges(t *testing.T) {
	f := setup(t)
	rec := &recorder{objects: map[string]map[string]any{}, byOp: map[string]string{}}
	tr := &trackers{t: rec}
	p := newPublisher(tr)
	p.Store = f.s
	w := f.work("Write the runbook")

	crashed := false
	p.SetAfterCreate(func() error {
		if !crashed {
			crashed = true
			return errors.New("crash")
		}
		return nil
	})
	_ = p.Once(f.ctx) // created externally, id never saved
	if pub := f.publication(w.ID); pub.ExternalID != "" || rec.creates != 1 {
		t.Fatalf("after crash: %+v creates=%d", pub, rec.creates)
	}
	f.due()
	_ = p.Once(f.ctx)
	if pub := f.publication(w.ID); pub.ExternalID != "ISSUE-1" || pub.State != store.PubPublished || rec.creates != 1 {
		t.Fatalf("after restart: %+v creates=%d", pub, rec.creates)
	}
}

func TestLostResponseIsReadBackNotDuplicated(t *testing.T) {
	f := setup(t)
	rec := &recorder{objects: map[string]map[string]any{}, byOp: map[string]string{}, ambiguous: true}
	tr := &trackers{t: rec}
	p := newPublisher(tr)
	p.Store = f.s
	w := f.work("Rotate the keys")
	_ = p.Once(f.ctx) // lands, but the response is lost
	if pub := f.publication(w.ID); pub.ExternalID != "" {
		t.Fatalf("after lost response: %+v", pub)
	}
	f.due()
	_ = p.Once(f.ctx)
	if pub := f.publication(w.ID); pub.ExternalID != "ISSUE-1" || rec.creates != 1 {
		t.Fatalf("after read-back: %+v creates=%d", pub, rec.creates)
	}
}
