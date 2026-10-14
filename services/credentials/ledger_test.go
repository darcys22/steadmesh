package credentials

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/darcys22/steadmesh/pkg/access"
	"github.com/darcys22/steadmesh/pkg/compile"
	"github.com/darcys22/steadmesh/services/store"
)

// memoryLedger exists only in tests. Production always requires PostgreSQL.
type memoryLedger struct {
	mu           sync.Mutex
	seats        map[string]*store.Seat
	rows         map[string]store.IssuedCredential
	claims       map[string]string
	recordErr    error
	beforeRecord func()
}

func newLedger() *memoryLedger {
	l := &memoryLedger{seats: map[string]*store.Seat{}, rows: map[string]store.IssuedCredential{}, claims: map[string]string{}}
	for _, key := range []string{"engineer", "reviewer"} {
		l.seats[key] = &store.Seat{ID: key + "-identity", OrganizationID: "org", Key: key, Manifest: compile.SeatManifest{Access: grant(access.DeliverySandbox)}}
	}
	return l
}

func (l *memoryLedger) SeatByKey(_ context.Context, org, key string) (*store.Seat, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.seats[key]
	if s == nil || s.OrganizationID != org {
		return nil, store.ErrNotFound
	}
	cp := *s
	return &cp, nil
}
func (l *memoryLedger) Seat(_ context.Context, id string) (*store.Seat, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, s := range l.seats {
		if s.ID == id {
			cp := *s
			return &cp, nil
		}
	}
	return nil, store.ErrNotFound
}
func (l *memoryLedger) RecordIssuedCredential(_ context.Context, i store.IssuedCredential, expected *access.SeatAccess) error {
	if l.beforeRecord != nil {
		l.beforeRecord()
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.recordErr != nil {
		return l.recordErr
	}
	s := l.seats[i.SeatKey]
	if s == nil || s.ID != i.SeatID || !sameAccess(s.Manifest.Access, expected) {
		i.Pending = true
		l.rows[i.ID] = i
		return store.ErrConflict
	}
	l.rows[i.ID] = i
	return nil
}
func (l *memoryLedger) ListIssuedCredentials(context.Context) ([]store.IssuedCredential, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []store.IssuedCredential
	for id, i := range l.rows {
		if !i.ExpiresAt.IsZero() && !i.ExpiresAt.After(time.Now()) {
			delete(l.rows, id)
		} else {
			out = append(out, i)
		}
	}
	return out, nil
}
func (l *memoryLedger) SetCredentialPending(_ context.Context, id string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	i, ok := l.rows[id]
	if !ok {
		return nil
	}
	i.Pending = true
	l.rows[id] = i
	return nil
}
func (l *memoryLedger) ClaimCredentialRevocation(_ context.Context, id string) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.rows[id].Pending || l.claims[id] != "" {
		return "", nil
	}
	claim := id + "-claim"
	l.claims[id] = claim
	return claim, nil
}
func (l *memoryLedger) FinishCredentialRevocation(_ context.Context, id, claim string, success bool) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.claims[id] != claim {
		return errors.New("stale claim")
	}
	delete(l.claims, id)
	if success {
		delete(l.rows, id)
	}
	return nil
}
func (l *memoryLedger) CountIssuedCredentials(_ context.Context, org, seat string) (int, error) {
	rows, _ := l.ListIssuedCredentials(context.Background())
	n := 0
	for _, i := range rows {
		if i.OrganizationID == org && i.SeatKey == seat {
			n++
		}
	}
	return n, nil
}
func testRegistry(i *issuer) (*Registry, *memoryLedger) {
	l := newLedger()
	return &Registry{Connections: conns{"github": i}, Ledger: l, EncryptionKey: []byte("0123456789abcdef0123456789abcdef")}, l
}
