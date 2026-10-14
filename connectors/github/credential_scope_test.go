package github

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/darcys22/steadmesh/connectors"
	fake "github.com/darcys22/steadmesh/tests/fakes/github"
)

func TestAppCredentialBindsInstallationOwner(t *testing.T) {
	for _, tt := range []struct {
		name  string
		owner string
		repos []string
		want  error
	}{
		{name: "matching owner", owner: "acme", repos: []string{"acme/sandbox"}},
		{name: "case insensitive owner", owner: "AcMe", repos: []string{"ACME/sandbox", "acme/other"}},
		{name: "wrong owner same repository name", owner: "otherowner", repos: []string{"acme/sandbox"}, want: connectors.ErrUnauthorized},
		{name: "mixed owners", owner: "acme", repos: []string{"acme/sandbox", "otherowner/sandbox"}, want: connectors.ErrUnauthorized},
		{name: "missing login", owner: "", repos: []string{"acme/sandbox"}, want: connectors.ErrPermanent},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f, a := setup(t, func(f *fake.Server) map[string]string { return appSecret(t, f) })
			f.SetInstallationOwner(tt.owner)
			c, err := a.IssueCredential(context.Background(), connectors.CredentialScope{
				Repos: tt.repos, Permissions: map[string]string{"contents": "read"},
			})
			if !errors.Is(err, tt.want) {
				t.Fatalf("credential error = %v, want %v", err, tt.want)
			}
			st := f.State()
			if len(st.Requests) == 0 || st.Requests[0].Method != http.MethodGet || st.Requests[0].Path != "/app/installations/77" || st.Requests[0].Auth != "app" {
				t.Fatalf("owner lookup did not use the app JWT: %+v", st.Requests)
			}
			if tt.want != nil {
				if c.Token != "" || len(st.Tokens) != 0 || len(st.Requests) != 1 {
					t.Fatalf("minted or attempted token before validating owner: credential=%+v state=%+v", c, st)
				}
				return
			}
			if len(st.Tokens) != 1 || len(st.Requests) != 2 || st.Requests[1].Method != http.MethodPost || st.Requests[1].Auth != "app" {
				t.Fatalf("expected one scoped mint after lookup: %+v", st)
			}
			wantNames := []string{"sandbox"}
			if len(tt.repos) == 2 {
				wantNames = append(wantNames, "other")
			}
			if !slices.Equal(st.Tokens[0].Repos, wantNames) || st.Tokens[0].Permissions["contents"] != "read" || st.Tokens[0].Permissions["metadata"] != "read" {
				t.Fatalf("unexpected scope: %+v", st.Tokens[0])
			}
			if !c.Revocable || c.Revocation == nil || c.Revocation.Kind != "github_installation" || c.Revocation.Endpoint != a.base {
				t.Fatalf("lost durable revocation route: %+v", c)
			}
		})
	}
}

func TestAppCredentialRejectsInvalidScopeBeforeRequests(t *testing.T) {
	f, a := setup(t, func(f *fake.Server) map[string]string { return appSecret(t, f) })
	validPermissions := map[string]string{"contents": "read"}
	cases := []struct {
		name  string
		scope connectors.CredentialScope
	}{
		{name: "empty scope"},
		{name: "missing repositories", scope: connectors.CredentialScope{Permissions: validPermissions}},
		{name: "missing permissions", scope: connectors.CredentialScope{Repos: []string{"acme/sandbox"}}},
		{name: "unknown permission", scope: connectors.CredentialScope{Repos: []string{"acme/sandbox"}, Permissions: map[string]string{"made_up": "write"}}},
		{name: "empty permission level", scope: connectors.CredentialScope{Repos: []string{"acme/sandbox"}, Permissions: map[string]string{"contents": ""}}},
		{name: "unknown permission level", scope: connectors.CredentialScope{Repos: []string{"acme/sandbox"}, Permissions: map[string]string{"contents": "admin"}}},
		{name: "metadata cannot be written", scope: connectors.CredentialScope{Repos: []string{"acme/sandbox"}, Permissions: map[string]string{"metadata": "write"}}},
	}
	for _, repo := range []string{"", "sandbox", "/sandbox", "acme/", "acme/a/b", "acme/..", "../sandbox", "acme/%2e%2e", "acme/sandbox?x=y", "acme/*", " acme/sandbox", "acme/sandbox\n"} {
		cases = append(cases, struct {
			name  string
			scope connectors.CredentialScope
		}{name: "repository " + repo, scope: connectors.CredentialScope{Repos: []string{repo}, Permissions: validPermissions}})
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			c, err := a.IssueCredential(context.Background(), tt.scope)
			if !errors.Is(err, connectors.ErrPermanent) || c.Token != "" {
				t.Fatalf("invalid scope returned credential=%+v err=%v", c, err)
			}
			if got := f.State(); len(got.Requests) != 0 || len(got.Tokens) != 0 {
				t.Fatalf("invalid scope reached GitHub: %+v", got)
			}
		})
	}
}

func TestAppCredentialInstallationLookupFailsClosed(t *testing.T) {
	for _, tt := range []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{name: "missing account", status: http.StatusOK, body: `{}`, want: connectors.ErrPermanent},
		{name: "null account", status: http.StatusOK, body: `{"account":null}`, want: connectors.ErrPermanent},
		{name: "empty login", status: http.StatusOK, body: `{"account":{"login":""}}`, want: connectors.ErrPermanent},
		{name: "invalid response", status: http.StatusOK, body: `{"account":{"login":123}}`, want: connectors.ErrPermanent},
		{name: "empty response", status: http.StatusOK, want: connectors.ErrPermanent},
		{name: "not found", status: http.StatusNotFound, want: connectors.ErrPermanent},
		{name: "unauthorized", status: http.StatusUnauthorized, want: connectors.ErrUnauthorized},
		{name: "unavailable", status: http.StatusServiceUnavailable, want: connectors.ErrRetryable},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := fake.New([]string{"acme/sandbox"})
			secret := appSecret(t, f)
			var requests atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.Method == http.MethodGet && r.URL.Path == "/app/installations/77" {
					w.WriteHeader(tt.status)
					_, _ = w.Write([]byte(tt.body))
					return
				}
				f.ServeHTTP(w, r)
			}))
			t.Cleanup(srv.Close)
			a, err := newAdapter(connectors.Config{Endpoint: srv.URL, Secret: secret})
			if err != nil {
				t.Fatal(err)
			}
			c, err := a.IssueCredential(context.Background(), connectors.CredentialScope{Repos: []string{"acme/sandbox"}, Permissions: map[string]string{"contents": "read"}})
			if !errors.Is(err, tt.want) || c.Token != "" {
				t.Fatalf("lookup failure returned credential=%+v err=%v, want %v", c, err, tt.want)
			}
			if requests.Load() != 1 || len(f.State().Tokens) != 0 {
				t.Fatalf("minted after failed lookup: requests=%d state=%+v", requests.Load(), f.State())
			}
		})
	}
}

func TestGitHubDoesNotFollowRepositoryRedirects(t *testing.T) {
	for _, tt := range []struct {
		name   string
		status int
		op     string
		params string
		path   string
	}{
		{name: "read moved", status: http.StatusMovedPermanently, op: "repo.read", params: `{"repo":"allowed/a"}`, path: "/repos/allowed/a"},
		{name: "read found", status: http.StatusFound, op: "repo.read", params: `{"repo":"allowed/a"}`, path: "/repos/allowed/a"},
		{name: "write temporary redirect", status: http.StatusTemporaryRedirect, op: "issue.comment", params: `{"repo":"allowed/a","number":1,"body":"hello"}`, path: "/repos/allowed/a/issues/1/comments"},
		{name: "write permanent redirect", status: http.StatusPermanentRedirect, op: "issue.comment", params: `{"repo":"allowed/a","number":1,"body":"hello"}`, path: "/repos/allowed/a/issues/1/comments"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var requests, deniedRequests, transportCalls, redirectCalls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.URL.Path == tt.path {
					http.Redirect(w, r, "/repos/denied/b/issues/1/comments", tt.status)
					return
				}
				deniedRequests.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":1}`))
			}))
			t.Cleanup(srv.Close)
			client := srv.Client()
			transport := client.Transport
			client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				transportCalls.Add(1)
				return transport.RoundTrip(r)
			})
			client.Timeout = 3 * time.Second
			client.CheckRedirect = func(*http.Request, []*http.Request) error {
				redirectCalls.Add(1)
				return nil
			}
			a, err := newAdapter(connectors.Config{Endpoint: srv.URL, HTTP: client, Secret: map[string]string{"token": "pat-1"}})
			if err != nil {
				t.Fatal(err)
			}
			if a.client == client || a.client.Timeout != client.Timeout {
				t.Fatal("adapter must clone the supplied client and preserve its timeout")
			}
			_, err = a.Invoke(context.Background(), tt.op, json.RawMessage(tt.params), "redirect-test")
			if !errors.Is(err, connectors.ErrPermanent) {
				t.Fatalf("redirect accepted: %v", err)
			}
			if requests.Load() != 1 || deniedRequests.Load() != 0 || transportCalls.Load() != 1 || redirectCalls.Load() != 0 {
				t.Fatalf("redirect followed: requests=%d denied=%d transport=%d redirects=%d", requests.Load(), deniedRequests.Load(), transportCalls.Load(), redirectCalls.Load())
			}
			if err := client.CheckRedirect(nil, nil); err != nil || redirectCalls.Load() != 1 {
				t.Fatal("adapter changed caller-owned redirect policy")
			}
		})
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
