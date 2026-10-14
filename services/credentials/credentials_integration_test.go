//go:build integration

package credentials

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/darcys22/steadmesh/connectors"
	"github.com/darcys22/steadmesh/pkg/access"
	"github.com/darcys22/steadmesh/pkg/compile"
	"github.com/darcys22/steadmesh/pkg/runtimeapi"
	"github.com/darcys22/steadmesh/pkg/spec"
	"github.com/darcys22/steadmesh/services/internal/orgfixture"
	"github.com/darcys22/steadmesh/services/internal/pgtest"
	"github.com/darcys22/steadmesh/services/store"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

var integrationLedgerKey = []byte("0123456789abcdef0123456789abcdef")

func openCredentialStore(t *testing.T, url string) *store.Store {
	t.Helper()
	s, err := store.Open(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func credentialSpec() spec.OrganizationSpec {
	sp := orgfixture.Spec()
	sp.Connections["github"] = spec.Connection{Adapter: "github", SecretRef: "k8s:github"}
	sp.AccessProfiles = map[string]spec.AccessProfile{
		"github": {GitHub: &spec.GitHubAccess{
			Connection: "github", Delivery: access.DeliverySandbox,
			Repos:       []string{"acme/a", "acme/b"},
			Permissions: map[string]string{"contents": "write", "pull_requests": "write", "issues": "read"},
		}},
	}
	seat := sp.Seats["engineer"]
	seat.AccessProfiles = []string{"github"}
	sp.Seats["engineer"] = seat
	return sp
}

func syncCredentialSpec(t *testing.T, s *store.Store, sp spec.OrganizationSpec) (*compile.Manifest, *runtimeapi.SyncResponse) {
	t.Helper()
	m := orgfixture.Compile(t, sp)
	out, err := s.SyncOrganization(context.Background(), store.SyncInput{
		Namespace: "acme", Key: "acme", SourceUID: "uid-1", Manifest: m,
	})
	if err != nil {
		t.Fatal(err)
	}
	return m, out
}

// Use a non-UTC, sub-microsecond expiry to exercise the database-normalized
// authenticated data, rather than merely checking an in-memory ciphertext.
type preciseExpiryIssuer struct {
	*issuer
	expires time.Time
}

func (i *preciseExpiryIssuer) IssueCredential(ctx context.Context, scope connectors.CredentialScope) (connectors.Credential, error) {
	c, err := i.issuer.IssueCredential(ctx, scope)
	c.ExpiresAt = i.expires
	return c, err
}

func TestRegistryPostgresRestartRevokesReducedScopes(t *testing.T) {
	cases := map[string]func(*spec.OrganizationSpec){
		"repository removed": func(sp *spec.OrganizationSpec) {
			sp.AccessProfiles["github"].GitHub.Repos = []string{"acme/a"}
		},
		"write becomes read": func(sp *spec.OrganizationSpec) {
			sp.AccessProfiles["github"].GitHub.Permissions["contents"] = "read"
		},
		"permission removed": func(sp *spec.OrganizationSpec) {
			delete(sp.AccessProfiles["github"].GitHub.Permissions, "pull_requests")
		},
		"sandbox becomes platform": func(sp *spec.OrganizationSpec) {
			sp.AccessProfiles["github"].GitHub.Delivery = access.DeliveryPlatform
		},
		"grant removed": func(sp *spec.OrganizationSpec) {
			seat := sp.Seats["engineer"]
			seat.AccessProfiles = nil
			sp.Seats["engineer"] = seat
		},
	}
	for name, reduce := range cases {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			url := pgtest.URL(t)
			s := openCredentialStore(t, url)
			sp := credentialSpec()
			m, org := syncCredentialSpec(t, s, sp)
			expires := time.Now().Add(time.Hour).Truncate(time.Second).Add(123456789 * time.Nanosecond).
				In(time.FixedZone("non-UTC issuer", 5*3600+45*60))
			gh := &preciseExpiryIssuer{issuer: &issuer{revocable: true}, expires: expires}
			r := &Registry{Ledger: s, EncryptionKey: integrationLedgerKey, Connections: conns{"github": gh}}
			cred, err := r.Issue(ctx, org.OrganizationID, org.Seats["engineer"].SeatID, m.Seats["engineer"].Access, "github")
			if err != nil || cred.Token == "" {
				t.Fatalf("issue = %v", err)
			}
			rows, err := s.ListIssuedCredentials(ctx)
			if err != nil || len(rows) != 1 || bytes.Contains(rows[0].Ciphertext, []byte(cred.Token)) {
				t.Fatal("issued token was not persisted as opaque ciphertext")
			}
			if !rows[0].ExpiresAt.Equal(expires.UTC().Truncate(time.Microsecond)) {
				t.Fatalf("unexpected database expiry: %v", rows[0].ExpiresAt)
			}
			oldSeat := rows[0].SeatID
			s.Close()
			s = openCredentialStore(t, url)
			restartedIssuer := &issuer{revocable: true}
			r = &Registry{Ledger: s, EncryptionKey: integrationLedgerKey, Connections: conns{"github": restartedIssuer}}
			rows, err = s.ListIssuedCredentials(ctx)
			if err != nil || len(rows) != 1 || rows[0].SeatID != oldSeat {
				t.Fatal("restart lost the issued credential's stable seat identity")
			}
			payload, err := r.unseal(rows[0])
			if err != nil || payload.Token != cred.Token {
				t.Fatalf("PostgreSQL round trip broke encryption authentication: %v", err)
			}
			if out := r.Sweep(ctx); len(out) != 0 || r.Outstanding(org.OrganizationID, "engineer") != 1 {
				t.Fatal("restart revoked a still-authorized token")
			}
			reduce(&sp)
			syncCredentialSpec(t, s, sp)
			// Even a stale caller's broader snapshot cannot undo the durable
			// pending decision made in the policy-update transaction.
			out := r.Reconcile(ctx, org.OrganizationID, map[string]*access.SeatAccess{"engineer": m.Seats["engineer"].Access})
			if len(out) != 1 || out[0].Err != nil || len(restartedIssuer.revoked) != 1 || restartedIssuer.revoked[0] != cred.Token {
				t.Fatalf("restart revocation = %+v, attempts = %d", out, len(restartedIssuer.revoked))
			}
			if r.Outstanding(org.OrganizationID, "engineer") != 0 {
				t.Fatal("successful provider revocation left ciphertext in the ledger")
			}
		})
	}
}

func TestRegistryPostgresFailedRevocationSurvivesRestartAndRegrant(t *testing.T) {
	ctx := context.Background()
	url := pgtest.URL(t)
	s := openCredentialStore(t, url)
	m, org := syncCredentialSpec(t, s, credentialSpec())
	gh := &issuer{revocable: true}
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	r := &Registry{Ledger: s, EncryptionKey: integrationLedgerKey, Connections: conns{"github": gh}, Log: logger}
	cred, err := r.Issue(ctx, org.OrganizationID, org.Seats["engineer"].SeatID, m.Seats["engineer"].Access, "github")
	if err != nil {
		t.Fatal(err)
	}
	narrow := credentialSpec()
	narrow.AccessProfiles["github"].GitHub.Repos = []string{"acme/a"}
	syncCredentialSpec(t, s, narrow)
	gh.revokeErr = errors.New("provider response contained secret " + cred.Token)
	out := r.Sweep(ctx)
	if len(out) != 1 || out[0].Err == nil || len(gh.revoked) != 1 {
		t.Fatal("expected failed provider revocation attempt")
	}
	if strings.Contains(out[0].Err.Error(), cred.Token) || strings.Contains(logs.String(), cred.Token) {
		t.Fatal("provider error leaked credential in an event or log")
	}
	rows, err := s.ListIssuedCredentials(ctx)
	if err != nil || len(rows) != 1 || !rows[0].Pending {
		t.Fatal("failed revocation lost its durable pending row")
	}
	id := rows[0].ID
	ciphertext := bytes.Clone(rows[0].Ciphertext)
	if out := r.Sweep(ctx); len(out) != 0 || len(gh.revoked) != 1 {
		t.Fatal("immediate second sweep bypassed the retry backoff")
	}
	// Regrant and restart cannot erase the failed revocation obligation.
	syncCredentialSpec(t, s, credentialSpec())
	s.Close()
	s = openCredentialStore(t, url)
	restartedIssuer := &issuer{revocable: true}
	r = &Registry{Ledger: s, EncryptionKey: integrationLedgerKey, Connections: conns{"github": restartedIssuer}, Log: logger}
	rows, err = s.ListIssuedCredentials(ctx)
	if err != nil || len(rows) != 1 || !rows[0].Pending || !bytes.Equal(rows[0].Ciphertext, ciphertext) {
		t.Fatal("restart/regrant changed the pending encrypted token")
	}
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, `UPDATE issued_credentials SET next_attempt_at = now() - interval '1 second' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	out = r.Sweep(ctx)
	if len(out) != 1 || out[0].Err != nil || len(restartedIssuer.revoked) != 1 || restartedIssuer.revoked[0] != cred.Token {
		t.Fatalf("durable retry after regrant = %+v", out)
	}
	if r.Outstanding(org.OrganizationID, "engineer") != 0 {
		t.Fatal("successful retry retained the pending row")
	}
}

func TestRegistryPostgresPurgeRevokesWithoutOriginalConnection(t *testing.T) {
	ctx := context.Background()
	url := pgtest.URL(t)
	s := openCredentialStore(t, url)
	m, org := syncCredentialSpec(t, s, credentialSpec())
	authorizations := make(chan string, 1)
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodDelete || req.URL.Path != "/installation/token" {
			t.Errorf("unexpected revocation request: %s %s", req.Method, req.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		authorizations <- req.Header.Get("Authorization")
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(remote.Close)
	gh := &issuer{revocable: true, revocation: &connectors.CredentialRevocation{Kind: "github_installation", Endpoint: remote.URL}}
	r := &Registry{Ledger: s, EncryptionKey: integrationLedgerKey, Connections: conns{"github": gh}}
	cred, err := r.Issue(ctx, org.OrganizationID, org.Seats["engineer"].SeatID, m.Seats["engineer"].Access, "github")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteOrganization(ctx, org.OrganizationID, true); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s = openCredentialStore(t, url)
	// No original tracker, connection config or App private key exists now.
	r = &Registry{Ledger: s, EncryptionKey: integrationLedgerKey, Connections: conns{}}
	out := r.Sweep(ctx)
	if len(out) != 1 || out[0].Err != nil {
		t.Fatalf("revocation from persisted provider route after purge = %+v", out)
	}
	select {
	case auth := <-authorizations:
		if auth != "Bearer "+cred.Token {
			t.Fatal("revocation request used the wrong decrypted token")
		}
	default:
		t.Fatal("provider never received the token revocation")
	}
	if len(gh.revoked) != 0 || r.Outstanding(org.OrganizationID, "engineer") != 0 {
		t.Fatal("revocation used original connection or retained token after success")
	}
}

func TestRegistryPostgresMidMintReductionFailsClosedAndRetries(t *testing.T) {
	ctx := context.Background()
	url := pgtest.URL(t)
	s := openCredentialStore(t, url)
	m, org := syncCredentialSpec(t, s, credentialSpec())
	gh := &issuer{revocable: true, revokeErr: errors.New("temporary provider failure")}
	gh.afterIssue = func() {
		narrow := credentialSpec()
		narrow.AccessProfiles["github"].GitHub.Repos = []string{"acme/a"}
		syncCredentialSpec(t, s, narrow)
	}
	r := &Registry{Ledger: s, EncryptionKey: integrationLedgerKey, Connections: conns{"github": gh}}
	cred, err := r.Issue(ctx, org.OrganizationID, org.Seats["engineer"].SeatID, m.Seats["engineer"].Access, "github")
	if !errors.Is(err, store.ErrConflict) || cred.Token != "" {
		t.Fatalf("mid-mint access change did not fail closed: %v", err)
	}
	rows, err := s.ListIssuedCredentials(ctx)
	if err != nil || len(rows) != 1 || !rows[0].Pending || len(gh.revoked) != 1 {
		t.Fatal("undelivered token cleanup failure was not durably retryable")
	}
	gh.revokeErr = nil
	gh.afterIssue = nil
	out := r.Sweep(ctx)
	if len(out) != 1 || out[0].Err != nil || len(gh.revoked) != 2 || gh.revoked[0] != gh.revoked[1] {
		t.Fatalf("undelivered token retry = %+v", out)
	}
	if r.Outstanding(org.OrganizationID, "engineer") != 0 {
		t.Fatal("undelivered token retained after successful retry")
	}
}
