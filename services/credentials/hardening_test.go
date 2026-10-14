package credentials

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/darcys22/steadmesh/connectors"
	"github.com/darcys22/steadmesh/pkg/access"
	"github.com/darcys22/steadmesh/services/store"
)

func issue(t *testing.T, r *Registry) string {
	t.Helper()
	c, err := r.Issue(context.Background(), "org", "engineer-identity", grant(access.DeliverySandbox), "github")
	if err != nil {
		t.Fatal(err)
	}
	return c.Token
}

func TestDurableRevocationScopeReductionAndRestart(t *testing.T) {
	for name, change := range map[string]func(*access.SeatAccess){
		"repository removed":  func(a *access.SeatAccess) { a.GitHub[0].Repos = []string{"acme/b"} },
		"write to read":       func(a *access.SeatAccess) { a.GitHub[0].Permissions["contents"] = "read" },
		"permission removed":  func(a *access.SeatAccess) { a.GitHub[0].Permissions = map[string]string{"pull_requests": "read"} },
		"sandbox to platform": func(a *access.SeatAccess) { a.GitHub[0].Delivery = access.DeliveryPlatform },
	} {
		t.Run(name, func(t *testing.T) {
			gh := &issuer{revocable: true}
			r, l := testRegistry(gh)
			token := issue(t, r)
			change(l.seats["engineer"].Manifest.Access)
			// A fresh replica has no in-memory issuance knowledge.
			restarted := &Registry{Connections: r.Connections, Ledger: l, EncryptionKey: r.EncryptionKey}
			out := restarted.Sweep(context.Background())
			if len(out) != 1 || out[0].Err != nil || len(gh.revoked) != 1 || gh.revoked[0] != token || restarted.Outstanding("org", "engineer") != 0 {
				t.Fatalf("revocation %+v %v", out, gh.revoked)
			}
		})
	}
}

func TestDurableFailedRevocationRemainsPendingAfterRegrant(t *testing.T) {
	gh := &issuer{revocable: true, revokeErr: errors.New("remote failed")}
	r, l := testRegistry(gh)
	token := issue(t, r)
	l.seats["engineer"].Manifest.Access = nil
	if out := r.Sweep(context.Background()); len(out) != 1 || out[0].Err == nil {
		t.Fatalf("first revoke %+v", out)
	}
	if r.Outstanding("org", "engineer") != 1 {
		t.Fatal("failed revoke was dropped")
	}
	l.seats["engineer"].Manifest.Access = grant(access.DeliverySandbox)
	gh.revokeErr = nil
	restarted := &Registry{Connections: r.Connections, Ledger: l, EncryptionKey: r.EncryptionKey}
	if out := restarted.Sweep(context.Background()); len(out) != 1 || out[0].Err != nil {
		t.Fatalf("retry %+v", out)
	}
	if len(gh.revoked) != 2 || gh.revoked[1] != token || r.Outstanding("org", "engineer") != 0 {
		t.Fatal("pending revoke did not survive regrant/restart")
	}
}

func TestIssuanceFailsClosedWithoutPersistenceOrKey(t *testing.T) {
	for _, setup := range []func(*Registry){func(r *Registry) { r.Ledger = nil }, func(r *Registry) { r.EncryptionKey = nil }, func(r *Registry) { r.EncryptionKey = []byte("short") }} {
		gh := &issuer{revocable: true}
		r, _ := testRegistry(gh)
		setup(r)
		if c, err := r.Issue(context.Background(), "org", "engineer-identity", grant(access.DeliverySandbox), "github"); err == nil || c.Token != "" || gh.n != 0 {
			t.Fatal("minted with unavailable ledger/key")
		}
	}
	gh := &issuer{revocable: true}
	r, l := testRegistry(gh)
	l.recordErr = errors.New("database unavailable")
	if c, err := r.Issue(context.Background(), "org", "engineer-identity", grant(access.DeliverySandbox), "github"); err == nil || c.Token != "" || len(gh.revoked) != 1 {
		t.Fatal("delivered untracked credential")
	}
}

func TestIssuanceRacePersistsPendingWithoutDelivering(t *testing.T) {
	gh := &issuer{revocable: true, revokeErr: errors.New("outage")}
	r, l := testRegistry(gh)
	l.beforeRecord = func() { l.seats["engineer"].Manifest.Access = nil }
	c, err := r.Issue(context.Background(), "org", "engineer-identity", grant(access.DeliverySandbox), "github")
	if !errors.Is(err, store.ErrConflict) || c.Token != "" || r.Outstanding("org", "engineer") != 1 {
		t.Fatalf("stale issue %+v %v", c, err)
	}
	for _, row := range l.rows {
		if !row.Pending {
			t.Fatal("stale minted token not durably pending")
		}
	}
	gh.revokeErr = nil
	if out := r.Sweep(context.Background()); len(out) != 1 || out[0].Err != nil {
		t.Fatalf("retry %+v", out)
	}
}

func TestCredentialCiphertextAuthenticatesScopeAndIdentity(t *testing.T) {
	gh := &issuer{revocable: true}
	r, l := testRegistry(gh)
	token := issue(t, r)
	var row store.IssuedCredential
	for _, v := range l.rows {
		row = v
	}
	if bytes.Contains(row.Ciphertext, []byte(token)) {
		t.Fatal("plaintext stored")
	}
	payload, err := r.unseal(row)
	if err != nil || payload.Token != token {
		t.Fatal("cannot decrypt roundtrip", err)
	}
	for _, mutate := range []func(*store.IssuedCredential){
		func(i *store.IssuedCredential) { i.SeatID = "other" },
		func(i *store.IssuedCredential) { i.Connection = "other" },
		func(i *store.IssuedCredential) { i.Scope = connectors.CredentialScope{Repos: []string{"acme/other"}} },
		func(i *store.IssuedCredential) { i.ExpiresAt = i.ExpiresAt.Add(time.Second) },
		func(i *store.IssuedCredential) {
			i.Ciphertext = append([]byte(nil), i.Ciphertext...)
			i.Ciphertext[len(i.Ciphertext)-1] ^= 1
		},
	} {
		bad := row
		mutate(&bad)
		if _, err := r.unseal(bad); err == nil {
			t.Fatal("tampered row decrypted")
		}
	}
	other := *r
	other.EncryptionKey = bytes.Repeat([]byte{1}, 32)
	if _, err := other.unseal(row); err == nil {
		t.Fatal("wrong key accepted")
	}
}

func TestPersistedGitHubRevokeSurvivesConnectionRemoval(t *testing.T) {
	var seen string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodDelete || req.URL.Path != "/api/v3/installation/token" {
			t.Errorf("unexpected revoke %s %s", req.Method, req.URL.Path)
		}
		seen = req.Header.Get("Authorization")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	gh := &issuer{revocable: true, revocation: &connectors.CredentialRevocation{Kind: "github_installation", Endpoint: upstream.URL + "/api/v3"}}
	r, l := testRegistry(gh)
	token := issue(t, r)
	r.Connections = conns{}
	delete(l.seats, "engineer")
	if out := r.Sweep(context.Background()); len(out) != 1 || out[0].Err != nil {
		t.Fatalf("revoke %+v", out)
	}
	if seen != "Bearer "+token {
		t.Fatal("did not revoke with persisted credential")
	}
}

func TestRevocationDoesNotForwardTokenOnRedirectOrExposeProviderBody(t *testing.T) {
	var leaked bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked = true }))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	err := revokePersisted(context.Background(), connectors.CredentialRevocation{Kind: "github_installation", Endpoint: source.URL}, "secret-token")
	if err == nil || strings.Contains(err.Error(), "secret-token") || leaked {
		t.Fatal("redirect/token leak", err)
	}
}

func TestCredentialScopeNeverCombinesPlatformAuthority(t *testing.T) {
	a := grant(access.DeliverySandbox)
	a.GitHub[0].Permissions = map[string]string{"contents": "read"}
	a.GitHub = append(a.GitHub, access.GitHubGrant{Connection: "github", Repos: []string{"acme/b"}, Permissions: map[string]string{"contents": "write"}, Delivery: access.DeliveryPlatform})
	s, err := Scope(a, "github")
	if err != nil || len(s.Repos) != 1 || s.Repos[0] != "acme/a" || s.Permissions["contents"] != "read" {
		t.Fatalf("scope %+v %v", s, err)
	}
	a.GitHub[1].Delivery = access.DeliverySandbox
	if _, err := Scope(a, "github"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("heterogeneous scope allowed: %v", err)
	}
}

func TestCredentialIssuanceUsesAuthenticatedStableSeatIdentity(t *testing.T) {
	gh := &issuer{revocable: true}
	r, l := testRegistry(gh)
	// The HTTP layer authenticated engineer-identity, then that seat was
	// retired and replaced under the same human-facing key before issuance.
	l.seats["engineer"].ID = "replacement-identity"
	c, err := r.Issue(context.Background(), "org", "engineer-identity", grant(access.DeliverySandbox), "github")
	if !errors.Is(err, store.ErrNotFound) || c.Token != "" || gh.n != 0 {
		t.Fatalf("retired identity rebound to replacement: %+v %v", c, err)
	}
	l.seats["engineer"].OrganizationID = "other-org"
	c, err = r.Issue(context.Background(), "org", "replacement-identity", grant(access.DeliverySandbox), "github")
	if !errors.Is(err, ErrForbidden) || c.Token != "" || gh.n != 0 {
		t.Fatal("cross-organization seat accepted")
	}
}

func TestRevokeSeatDoesNotRevokeReusedKey(t *testing.T) {
	gh := &issuer{revocable: true}
	r, l := testRegistry(gh)
	oldToken := issue(t, r)
	l.seats["engineer"].ID = "replacement-identity"
	c, err := r.Issue(context.Background(), "org", "replacement-identity", grant(access.DeliverySandbox), "github")
	if err != nil {
		t.Fatal(err)
	}
	out := r.RevokeSeat(context.Background(), "org", "engineer-identity")
	if len(out) != 1 || len(gh.revoked) != 1 || gh.revoked[0] != oldToken || gh.revoked[0] == c.Token || r.Outstanding("org", "engineer") != 1 {
		t.Fatalf("reused key revoked: %+v %v", out, gh.revoked)
	}
}
