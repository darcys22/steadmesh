// Package publisher projects work items to a tracker for people to follow
// (work_publication). It is optional and outward only: agents coordinate in
// shared memory and messages, and a missing or failing tracker never blocks
// them; publications are retried in the background and their state is
// reported separately from the work itself.
//
// Each work item maps to one external object per connection. Creating it
// uses a stable idempotency key per (work item, connection, kind), and the
// external object carries the operation marker, so a crash between creating
// it and recording its id converges by read-back instead of creating a
// duplicate. Later revisions update the recorded object.
package publisher

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/darcys22/steadmesh/connectors"
	"github.com/darcys22/steadmesh/pkg/runtimeapi"
	"github.com/darcys22/steadmesh/services/store"
)

// ExternalKind is the kind of external object a work item becomes.
const ExternalKind = "issue"

// Store is the persistence the publisher needs.
type Store interface {
	ActiveOrganizations(ctx context.Context) ([]string, error)
	Organization(ctx context.Context, orgID string) (*store.Organization, error)
	ActiveStoreIDs(ctx context.Context, orgID string, keys []string) (map[string]string, error)
	EnqueuePublications(ctx context.Context, orgID string, storeIDs []string, connection, kind string) error
	ClaimPublications(ctx context.Context, orgID, connection string, limit int, lease time.Duration) ([]store.PublicationItem, error)
	SetPublicationExternal(ctx context.Context, it store.PublicationItem, externalID, url string) error
	ResolvePublication(ctx context.Context, it store.PublicationItem, state string, publishedRevision int, errText string, retryIn time.Duration) error
	BeginSystemOperation(ctx context.Context, in store.NewOperation) (*runtimeapi.Operation, bool, error)
	FinishOperation(ctx context.Context, id, status, receipt string, result json.RawMessage, errText string, attempts int) error
}

// Trackers resolves tracker adapters.
type Trackers interface {
	Tracker(orgID, key string) (connectors.Tracker, error)
}

// Publisher runs the publication loop.
type Publisher struct {
	Store    Store
	Trackers Trackers
	Log      *slog.Logger
	Interval time.Duration
	// Backoff is the delay before retrying after the given number of
	// attempts; nil uses 2s, 8s, 32s ... up to 10 minutes.
	Backoff func(attempts int) time.Duration
	// ClaimLease is how long a claimed publication is reserved; a publisher
	// that dies holding it is retried after it ends. Zero uses 5 minutes.
	ClaimLease time.Duration

	// afterCreate, when set, runs after an external object was created and
	// before its id is recorded; tests use it to simulate a crash.
	afterCreate func() error
}

const batch = 20

// Run publishes until ctx is cancelled.
func (p *Publisher) Run(ctx context.Context) {
	t := time.NewTicker(p.Interval)
	defer t.Stop()
	for {
		if err := p.Once(ctx); err != nil && ctx.Err() == nil {
			p.Log.Error("work publication", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Once runs one publication pass over every organisation that publishes.
func (p *Publisher) Once(ctx context.Context) error {
	ids, err := p.Store.ActiveOrganizations(ctx)
	if err != nil {
		return err
	}
	for _, orgID := range ids {
		org, err := p.Store.Organization(ctx, orgID)
		if err != nil {
			return err
		}
		pub := org.Manifest.Spec.WorkPublication
		if pub == nil {
			continue
		}
		stores, err := p.Store.ActiveStoreIDs(ctx, orgID, pub.Stores)
		if err != nil {
			return err
		}
		storeIDs := make([]string, 0, len(stores))
		for _, id := range stores {
			storeIDs = append(storeIDs, id)
		}
		if err := p.Store.EnqueuePublications(ctx, orgID, storeIDs, pub.Connection, ExternalKind); err != nil {
			return err
		}
		lease := p.ClaimLease
		if lease == 0 {
			lease = 5 * time.Minute
		}
		items, err := p.Store.ClaimPublications(ctx, orgID, pub.Connection, batch, lease)
		if err != nil {
			return err
		}
		for _, it := range items {
			p.publish(ctx, it)
		}
	}
	return nil
}

// retryIn backs off 2s, 8s, 32s ... up to 10 minutes.
func retryIn(attempts int) time.Duration {
	d := 2 * time.Second
	for range attempts - 1 {
		d *= 4
		if d > 10*time.Minute {
			return 10 * time.Minute
		}
	}
	return d
}

type workItem struct {
	ID        string `json:"id"`
	Objective string `json:"objective"`
	Status    string `json:"status"`
}

func (p *Publisher) publish(ctx context.Context, it store.PublicationItem) {
	log := p.Log.With("organization_id", it.OrganizationID, "record_id", it.RecordID, "connection", it.Connection)
	resolve := func(state, errText string, published int) {
		wait := time.Duration(0)
		if state != store.PubPublished {
			wait = retryIn(it.Attempts)
			if state == store.PubBlocked {
				// The tracker is unreachable or rejecting us: check back
				// often enough that a reconnect is picked up quickly.
				wait = min(wait, time.Minute)
			}
			if p.Backoff != nil {
				wait = p.Backoff(it.Attempts)
			}
		}
		if err := p.Store.ResolvePublication(context.WithoutCancel(ctx), it, state, published, errText, wait); err != nil {
			log.Error("record publication outcome", "error", err)
		}
	}
	tracker, err := p.Trackers.Tracker(it.OrganizationID, it.Connection)
	if err != nil {
		// The connection is unavailable: report it here only; agents carry on.
		resolve(store.PubBlocked, err.Error(), 0)
		return
	}
	var w workItem
	if err := json.Unmarshal(it.Data, &w); err != nil {
		resolve(store.PubFailed, "work item data: "+err.Error(), 0)
		return
	}
	params := map[string]any{"title": fmt.Sprintf("%s: %s", w.ID, w.Objective), "description": it.Body}
	key := fmt.Sprintf("pub-create:%s:%s:%s", it.RecordID, it.Connection, it.ExternalKind)
	if it.ExternalID != "" {
		params["id"] = it.ExternalID
		key = fmt.Sprintf("pub-update:%s:%s:%d", it.RecordID, it.Connection, it.Revision)
	}
	raw, _ := json.Marshal(params)
	op, created, err := p.Store.BeginSystemOperation(ctx, store.NewOperation{OrganizationID: it.OrganizationID,
		Connection: it.Connection, Operation: "task.write", Target: it.ExternalID, RequestHash: digest(raw), IdempotencyKey: key})
	if err != nil {
		resolve(store.PubPending, err.Error(), 0)
		return
	}
	var res *connectors.Result
	if !created && op.Status == store.OpSucceeded {
		res = &connectors.Result{Receipt: op.ExternalReceipt, Data: op.Result}
	} else if !created {
		// A previous attempt may have landed (crash, lost response): read back
		// by the operation marker before trying again.
		found, ferr := tracker.FindByOperation(ctx, "task.write", raw, op.ID)
		if ferr != nil {
			resolve(classify(ferr), "read-back: "+ferr.Error(), 0)
			return
		}
		if found != nil {
			log.Info("publication resolved by read-back", "operation_id", op.ID)
			res = found
		}
	}
	if res == nil {
		r, err := tracker.Invoke(ctx, "task.write", raw, op.ID)
		if err != nil {
			status := store.OpFailed
			if errors.Is(err, connectors.ErrAmbiguous) {
				status = store.OpUnknown
			}
			_ = p.Store.FinishOperation(context.WithoutCancel(ctx), op.ID, status, "", nil, err.Error(), 1)
			resolve(classify(err), err.Error(), 0)
			return
		}
		res = &r
	}
	if created || op.Status != store.OpSucceeded {
		if err := p.Store.FinishOperation(context.WithoutCancel(ctx), op.ID, store.OpSucceeded, res.Receipt, res.Data, "", 1); err != nil {
			log.Error("record publication operation", "error", err)
		}
	}
	if it.ExternalID == "" {
		if p.afterCreate != nil {
			if err := p.afterCreate(); err != nil {
				return
			}
		}
		var issue struct {
			ID  string `json:"id"`
			URL string `json:"url"`
		}
		_ = json.Unmarshal(res.Data, &issue)
		if issue.ID == "" {
			issue.ID = res.Receipt
		}
		if err := p.Store.SetPublicationExternal(context.WithoutCancel(ctx), it, issue.ID, issue.URL); err != nil {
			log.Error("record published object", "error", err)
			return
		}
		// The object reflects this revision already.
	}
	resolve(store.PubPublished, "", it.Revision)
	log.Info("work item published", "revision", it.Revision, "receipt", res.Receipt)
}

// classify maps a connector error to a publication state: blocked while the
// tracker is unreachable or rejects the credential (retried), failed when the
// request itself is wrong (retried with backoff, reported).
func classify(err error) string {
	switch {
	case errors.Is(err, connectors.ErrPermanent):
		return store.PubFailed
	case errors.Is(err, connectors.ErrUnauthorized), errors.Is(err, connectors.ErrRetryable):
		return store.PubBlocked
	}
	return store.PubPending
}

func digest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
