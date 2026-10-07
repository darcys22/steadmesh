package github

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/darcys22/steadmesh/connectors"
	fake "github.com/darcys22/steadmesh/tests/fakes/github"
)

func appSecret(t *testing.T, f *fake.Server) map[string]string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f.SetApp("4242", "77", &key.PublicKey)
	p := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	return map[string]string{"app_id": "4242", "installation_id": "77", "private_key": string(p)}
}

func setup(t *testing.T, secret func(*fake.Server) map[string]string) (*fake.Server, *Adapter) {
	t.Helper()
	f := fake.New([]string{"acme/sandbox", "acme/other"}, "pat-1")
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	a, err := newAdapter(connectors.Config{Adapter: "github", Endpoint: srv.URL, Secret: secret(f)})
	if err != nil {
		t.Fatal(err)
	}
	return f, a
}

func TestNewRequiresCredentials(t *testing.T) {
	for _, s := range []map[string]string{{}, {"app_id": "1"}, {"app_id": "1", "installation_id": "2", "private_key": "nope"}} {
		if _, err := New(connectors.Config{Secret: s}); !errors.Is(err, connectors.ErrUnauthorized) {
			t.Errorf("%v: %v", s, err)
		}
	}
}

func TestPATVerifyAndCredential(t *testing.T) {
	_, a := setup(t, func(*fake.Server) map[string]string { return map[string]string{"token": "pat-1"} })
	if err := a.Verify(context.Background()); err != nil {
		t.Fatal(err)
	}
	c, err := a.IssueCredential(context.Background(), connectors.CredentialScope{Repos: []string{"acme/sandbox"}})
	if err != nil || c.Token != "pat-1" || c.Revocable || !c.ExpiresAt.IsZero() {
		t.Fatalf("PAT credential %+v %v", c, err)
	}
	if err := a.RevokeCredential(context.Background(), "pat-1"); err == nil {
		t.Fatal("a PAT cannot be revoked by Steadmesh")
	}
	_, bad := setup(t, func(*fake.Server) map[string]string { return map[string]string{"token": "wrong"} })
	if err := bad.Verify(context.Background()); !errors.Is(err, connectors.ErrUnauthorized) {
		t.Fatalf("bad PAT: %v", err)
	}
}

func TestAppCredentialsAreScopedAndRevocable(t *testing.T) {
	f, a := setup(t, func(f *fake.Server) map[string]string { return appSecret(t, f) })
	ctx := context.Background()
	if err := a.Verify(ctx); err != nil {
		t.Fatal(err)
	}
	c, err := a.IssueCredential(ctx, connectors.CredentialScope{Repos: []string{"acme/sandbox"}, Permissions: map[string]string{"pull_requests": "write"}})
	if err != nil || !c.Revocable || c.ExpiresAt.IsZero() || !strings.HasPrefix(c.Token, "ghs_") {
		t.Fatalf("app credential %+v %v", c, err)
	}
	// The token works for the granted repository only.
	get := func(path string) int {
		req, _ := http.NewRequest(http.MethodGet, a.base+path, nil)
		req.Header.Set("Authorization", "Bearer "+c.Token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if got := get("/repos/acme/sandbox"); got != 200 {
		t.Fatalf("granted repo: %d", got)
	}
	if got := get("/repos/acme/other"); got == 200 {
		t.Fatal("token reaches an ungranted repository")
	}
	st := f.State()
	last := st.Tokens[len(st.Tokens)-1]
	if strings.Join(last.Repos, ",") != "sandbox" || last.Permissions["pull_requests"] != "write" || last.Permissions["metadata"] != "read" {
		t.Fatalf("minted scope %+v", last)
	}
	if err := a.RevokeCredential(ctx, c.Token); err != nil {
		t.Fatal(err)
	}
	if got := get("/repos/acme/sandbox"); got != http.StatusUnauthorized {
		t.Fatalf("revoked token still works: %d", got)
	}
	// Revoking again is not an error.
	if err := a.RevokeCredential(ctx, c.Token); err != nil {
		t.Fatal(err)
	}
}

func TestPullRequestCreateAndReadBack(t *testing.T) {
	f, a := setup(t, func(f *fake.Server) map[string]string { return appSecret(t, f) })
	ctx := context.Background()
	params, _ := json.Marshal(map[string]any{"repo": "acme/sandbox", "title": "Add login", "head": "login", "base": "main", "body": "Implements the form."})
	res, err := a.Invoke(ctx, "pull_request.create", params, "op-1")
	if err != nil || res.Receipt == "" {
		t.Fatalf("create %+v %v", res, err)
	}
	if b := f.State().Pulls[0].Body; !strings.Contains(b, "steadmesh-op:op-1") || !strings.HasPrefix(b, "Implements the form.") {
		t.Fatalf("body %q", b)
	}
	found, err := a.FindByOperation(ctx, "pull_request.create", params, "op-1")
	if err != nil || found == nil || found.Receipt != res.Receipt {
		t.Fatalf("read-back %+v %v", found, err)
	}
	if missing, _ := a.FindByOperation(ctx, "pull_request.create", params, "op-2"); missing != nil {
		t.Fatal("found an operation that never ran")
	}
	repo, err := a.Invoke(ctx, "repo.read", json.RawMessage(`{"repo":"acme/sandbox"}`), "op-3")
	if err != nil || !strings.Contains(string(repo.Data), `"default_branch":"main"`) {
		t.Fatalf("repo.read %s %v", repo.Data, err)
	}
	if _, err := a.Invoke(ctx, "pull_request.create", json.RawMessage(`{"repo":"acme"}`), "op-4"); !errors.Is(err, connectors.ErrPermanent) {
		t.Fatalf("bad repo: %v", err)
	}
	if !a.ReadOnly("repo.read") || a.ReadOnly("pull_request.create") {
		t.Fatal("ReadOnly")
	}
}
