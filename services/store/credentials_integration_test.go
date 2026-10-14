//go:build integration

package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/darcys22/steadmesh/connectors"
	"github.com/darcys22/steadmesh/pkg/access"
	"github.com/darcys22/steadmesh/pkg/compile"
	"github.com/darcys22/steadmesh/services/internal/orgfixture"
	"github.com/darcys22/steadmesh/services/internal/pgtest"
)

func credentialManifest(t *testing.T, repos ...string) *compile.Manifest {
	t.Helper()
	m := orgfixture.Manifest(t)
	seat := m.Seats["lead"]
	seat.Access = &access.SeatAccess{GitHub: []access.GitHubGrant{{
		Connection: "github", Delivery: access.DeliverySandbox,
		Repos: repos, Permissions: map[string]string{"contents": "write"},
	}}}
	m.Seats["lead"] = seat
	return m
}

func credentialRecord(t *testing.T, s *Store, m *compile.Manifest) IssuedCredential {
	t.Helper()
	res := syncOrg(t, s, m)
	grant := m.Seats["lead"].Access.GitHub[0]
	return IssuedCredential{
		ID: uuid.NewString(), OrganizationID: res.OrganizationID, SeatID: res.Seats["lead"].SeatID,
		SeatKey: "lead", Connection: grant.Connection,
		Scope: connectors.CredentialScope{Repos: grant.Repos, Permissions: grant.Permissions},
		// The encryption envelope is opaque to the store. The registry's
		// cipher tests verify encryption; this test checks byte-for-byte storage.
		Ciphertext: []byte{1, 0, 2, 255, 0, 4, 3},
		ExpiresAt:  time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond),
	}
}

func TestIssuedCredentialPersistsAcrossRestartAndPurge(t *testing.T) {
	ctx := context.Background()
	url := pgtest.URL(t)
	s, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	m := credentialManifest(t, "acme/core", "acme/tools")
	record := credentialRecord(t, s, m)
	if err := s.RecordIssuedCredential(ctx, record, m.Seats["lead"].Access); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordIssuedCredential(ctx, record, m.Seats["lead"].Access); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate credential = %v, want conflict", err)
	}
	var raw []byte
	if err := s.pool.QueryRow(ctx, `SELECT ciphertext FROM issued_credentials WHERE id = $1`, record.ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, record.Ciphertext) {
		t.Fatal("store changed the opaque encryption envelope")
	}
	s.Close()
	s, err = Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	rows, err := s.ListIssuedCredentials(ctx)
	if err != nil || len(rows) != 1 {
		t.Fatalf("reopened ledger rows = %d, err = %v", len(rows), err)
	}
	got := rows[0]
	if got.ID != record.ID || got.SeatID != record.SeatID || got.OrganizationID != record.OrganizationID || got.Pending ||
		!got.ExpiresAt.Equal(record.ExpiresAt) || !bytes.Equal(got.Ciphertext, record.Ciphertext) {
		t.Fatalf("reopened record lost identity, expiry, state, or ciphertext")
	}
	wantScope, _ := json.Marshal(record.Scope)
	gotScope, _ := json.Marshal(got.Scope)
	if !bytes.Equal(wantScope, gotScope) {
		t.Fatal("reopened record lost the exact issued scope")
	}
	if count, err := s.CountIssuedCredentials(ctx, record.OrganizationID, record.SeatKey); err != nil || count != 1 {
		t.Fatalf("outstanding = %d, %v", count, err)
	}
	if err := s.DeleteOrganization(ctx, record.OrganizationID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Seat(ctx, record.SeatID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("purged seat = %v", err)
	}
	rows, err = s.ListIssuedCredentials(ctx)
	if err != nil || len(rows) != 1 || !rows[0].Pending {
		t.Fatalf("purge lost pending revocation: rows = %d, err = %v", len(rows), err)
	}
}

func TestIssuedCredentialAuthorityChangesAreSticky(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	broad := credentialManifest(t, "acme/core", "acme/tools")
	record := credentialRecord(t, s, broad)
	if err := s.RecordIssuedCredential(ctx, record, broad.Seats["lead"].Access); err != nil {
		t.Fatal(err)
	}
	// An idempotent sync must neither bump policy nor revoke anything.
	unchanged := syncOrg(t, s, broad)
	if unchanged.Seats["lead"].PolicyRevision != 1 {
		t.Fatal("unchanged access bumped policy")
	}
	rows, _ := s.ListIssuedCredentials(ctx)
	if len(rows) != 1 || rows[0].Pending {
		t.Fatal("unchanged access marked credential pending")
	}
	narrow := credentialManifest(t, "acme/core")
	changed := syncOrg(t, s, narrow)
	if changed.Seats["lead"].PolicyRevision != 2 {
		t.Fatal("sandbox-only access reduction did not bump policy")
	}
	// A mint that started from an old snapshot must never be delivered, but
	// the ciphertext still needs a durable pending entry if revocation fails.
	late := record
	late.ID = uuid.NewString()
	if err := s.RecordIssuedCredential(ctx, late, broad.Seats["lead"].Access); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale issuance = %v, want conflict", err)
	}
	// Broadening conservatively rotates credentials too. A token minted under
	// the narrower snapshot becomes pending in that same broadening commit.
	narrowToken := record
	narrowToken.ID = uuid.NewString()
	narrowToken.Scope.Repos = []string{"acme/core"}
	if err := s.RecordIssuedCredential(ctx, narrowToken, narrow.Seats["lead"].Access); err != nil {
		t.Fatal(err)
	}
	// There is deliberately no reconcile pass between tightening and broadening.
	broadened := syncOrg(t, s, broad)
	if broadened.Seats["lead"].PolicyRevision != 3 {
		t.Fatal("sandbox-only access broadening did not bump policy")
	}
	rows, err := s.ListIssuedCredentials(ctx)
	if err != nil || len(rows) != 3 {
		t.Fatalf("ledger rows = %d, err = %v", len(rows), err)
	}
	for _, row := range rows {
		if !row.Pending {
			t.Fatal("broader later access cleared a required revocation")
		}
	}
}

func TestIssuedCredentialConcurrentSyncCannotMissMint(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	broad := credentialManifest(t, "acme/core", "acme/tools")
	record := credentialRecord(t, s, broad)
	start := make(chan struct{})
	errs := make(chan error, 13)
	var wg sync.WaitGroup
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			row := record
			row.ID = uuid.NewString()
			err := s.RecordIssuedCredential(ctx, row, broad.Seats["lead"].Access)
			if errors.Is(err, ErrConflict) {
				err = nil // Stale mints are durably pending, never deliverable.
			}
			errs <- err
		}()
	}
	narrow := credentialManifest(t, "acme/core")
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		_, err := s.SyncOrganization(ctx, SyncInput{Namespace: "acme", Key: "acme", SourceUID: "uid-1", Manifest: narrow})
		errs <- err
	}()
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	rows, err := s.ListIssuedCredentials(ctx)
	if err != nil || len(rows) != 12 {
		t.Fatalf("concurrent ledger rows = %d, err = %v", len(rows), err)
	}
	for _, row := range rows {
		if !row.Pending {
			t.Fatal("a concurrent mint escaped the committed access reduction")
		}
	}
}

func TestIssuedCredentialClaimsRetryAndFenceAcrossReplicas(t *testing.T) {
	ctx := context.Background()
	url := pgtest.URL(t)
	first, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(first.Close)
	second, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(second.Close)
	m := credentialManifest(t, "acme/core")
	record := credentialRecord(t, first, m)
	if err := first.RecordIssuedCredential(ctx, record, m.Seats["lead"].Access); err != nil {
		t.Fatal(err)
	}
	if claim, err := second.ClaimCredentialRevocation(ctx, record.ID); err != nil || claim != "" {
		t.Fatalf("nonpending credential claimed: %q, %v", claim, err)
	}
	if err := first.SetCredentialPending(ctx, record.ID); err != nil {
		t.Fatal(err)
	}
	type result struct {
		claim string
		err   error
	}
	results := make(chan result, 12)
	for i := range 12 {
		replica := first
		if i%2 == 0 {
			replica = second
		}
		go func() {
			claim, err := replica.ClaimCredentialRevocation(ctx, record.ID)
			results <- result{claim, err}
		}()
	}
	var owner string
	for range 12 {
		got := <-results
		if got.err != nil {
			t.Fatal(got.err)
		}
		if got.claim != "" {
			if owner != "" {
				t.Fatal("multiple replicas claimed the same live lease")
			}
			owner = got.claim
		}
	}
	if owner == "" {
		t.Fatal("no replica claimed pending revocation")
	}
	if err := first.FinishCredentialRevocation(ctx, record.ID, owner, false); err != nil {
		t.Fatal(err)
	}
	if err := second.SetCredentialPending(ctx, record.ID); err != nil {
		t.Fatal(err)
	}
	if claim, err := second.ClaimCredentialRevocation(ctx, record.ID); err != nil || claim != "" {
		t.Fatalf("failure backoff was bypassed: %q, %v", claim, err)
	}
	rows, err := second.ListIssuedCredentials(ctx)
	if err != nil || len(rows) != 1 || !rows[0].Pending || !bytes.Equal(rows[0].Ciphertext, record.Ciphertext) {
		t.Fatal("failed revocation forgot the pending encrypted credential")
	}
	if _, err := first.pool.Exec(ctx, `UPDATE issued_credentials SET next_attempt_at = now() - interval '1 second' WHERE id = $1`, record.ID); err != nil {
		t.Fatal(err)
	}
	claim, err := second.ClaimCredentialRevocation(ctx, record.ID)
	if err != nil || claim == "" {
		t.Fatalf("retry claim = %q, %v", claim, err)
	}
	// Simulate a worker crash and takeover after its lease. Fencing protects
	// the new claim against both failed and successful late completions.
	if _, err := first.pool.Exec(ctx, `UPDATE issued_credentials SET leased_until = now() - interval '1 second' WHERE id = $1`, record.ID); err != nil {
		t.Fatal(err)
	}
	newClaim, err := first.ClaimCredentialRevocation(ctx, record.ID)
	if err != nil || newClaim == "" || newClaim == claim {
		t.Fatalf("takeover claim = %q, %v", newClaim, err)
	}
	for _, success := range []bool{false, true} {
		if err := second.FinishCredentialRevocation(ctx, record.ID, claim, success); !errors.Is(err, ErrFenced) {
			t.Fatalf("late completion (success=%v) = %v", success, err)
		}
	}
	if err := first.FinishCredentialRevocation(ctx, record.ID, newClaim, true); err != nil {
		t.Fatal(err)
	}
	if count, err := second.CountIssuedCredentials(ctx, record.OrganizationID, record.SeatKey); err != nil || count != 0 {
		t.Fatalf("successful remote revoke retained ciphertext: count=%d err=%v", count, err)
	}
}

func TestIssuedCredentialRetiredIdentityAndExpiry(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	m := credentialManifest(t, "acme/core")
	record := credentialRecord(t, s, m)
	if err := s.RecordIssuedCredential(ctx, record, m.Seats["lead"].Access); err != nil {
		t.Fatal(err)
	}
	removed := credentialManifest(t, "acme/core")
	delete(removed.Seats, "lead")
	syncOrg(t, s, removed)
	// Retiring seats retain their current grant during the grace period.
	windDown := record
	windDown.ID = uuid.NewString()
	if err := s.RecordIssuedCredential(ctx, windDown, m.Seats["lead"].Access); err != nil {
		t.Fatalf("retiring seat issuance = %v", err)
	}
	if _, err := s.FinishRetirement(ctx, record.SeatID, nil); err != nil {
		t.Fatal(err)
	}
	fresh := syncOrg(t, s, m)
	if fresh.Seats["lead"].SeatID == record.SeatID {
		t.Fatal("seat recreation reused old identity")
	}
	late := record
	late.ID = uuid.NewString()
	if err := s.RecordIssuedCredential(ctx, late, m.Seats["lead"].Access); !errors.Is(err, ErrConflict) {
		t.Fatalf("retired identity issuance = %v", err)
	}
	rows, err := s.ListIssuedCredentials(ctx)
	if err != nil || len(rows) != 3 {
		t.Fatalf("retired credential rows = %d, %v", len(rows), err)
	}
	for _, row := range rows {
		if !row.Pending || row.SeatID != record.SeatID {
			t.Fatal("retirement or key reuse lost old identity revocation")
		}
	}
	if _, err := s.pool.Exec(ctx, `UPDATE issued_credentials SET expires_at = now() - interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	if claim, err := s.ClaimCredentialRevocation(ctx, record.ID); err != nil || claim != "" {
		t.Fatalf("expired credential claimed: %q, %v", claim, err)
	}
	rows, err = s.ListIssuedCredentials(ctx)
	if err != nil || len(rows) != 0 {
		t.Fatalf("expired credentials listed: %d, %v", len(rows), err)
	}
	var retained int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM issued_credentials`).Scan(&retained); err != nil || retained != 0 {
		t.Fatalf("expired ciphertext retained: %d, %v", retained, err)
	}
}
