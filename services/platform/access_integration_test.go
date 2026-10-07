//go:build integration

package platform_test

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/darcys22/steadmesh/connectors/github"
	"github.com/darcys22/steadmesh/pkg/runtimeapi"
	"github.com/darcys22/steadmesh/pkg/spec"
	"github.com/darcys22/steadmesh/services/internal/fakeconn"
	"github.com/darcys22/steadmesh/services/internal/orgfixture"
	"github.com/darcys22/steadmesh/services/platform"
	fakegithub "github.com/darcys22/steadmesh/tests/fakes/github"
)

// TestSandboxAccessAndCredentials checks the access the egress gateway and
// the credential helper rely on: the seat's live egress rules, a scoped
// GitHub App token for a seat with sandbox delivery only, and revocation of
// that token at GitHub when a sync removes the grant.
func TestSandboxAccessAndCredentials(t *testing.T) {
	gh := fakegithub.New([]string{"acme/sandbox", "acme/other"})
	ghSrv := httptest.NewServer(gh)
	t.Cleanup(ghSrv.Close)
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	gh.SetApp("1", "2", &key.PublicKey)
	secrets := &fakeconn.Secrets{}
	secrets.Set("k8s:github", map[string]string{"app_id": "1", "installation_id": "2",
		"private_key": string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))})
	e := newEnvWith(t, true, func(o *platform.Options) {
		o.Secrets = secrets
		o.Factories.Tracker["github"] = github.New
	})
	sp := orgfixture.Spec()
	sp.Connections["github"] = spec.Connection{Adapter: "github", SecretRef: "k8s:github", EndpointRef: ghSrv.URL}
	sp.AccessProfiles = map[string]spec.AccessProfile{
		"gh": {
			Egress: &spec.EgressAccess{Hosts: []string{"pypi.org"}},
			GitHub: &spec.GitHubAccess{Connection: "github", Repos: []string{"acme/sandbox"}, Permissions: map[string]string{"pull_requests": "write"}, Delivery: "sandbox"},
		},
	}
	eng := sp.Seats["engineer"]
	eng.AccessProfiles = []string{"gh"}
	sp.Seats["engineer"] = eng
	e.sync(sp)

	var acc runtimeapi.AccessResponse
	code, b := e.request("GET", runtimeapi.PathAccess, "tok-engineer", nil, nil)
	_ = json.Unmarshal(b, &acc)
	hosts := []string{}
	for _, r := range acc.Egress {
		hosts = append(hosts, r.Host)
	}
	if code != http.StatusOK || acc.SeatKey != "engineer" || !strings.Contains(strings.Join(hosts, ","), "pypi.org") ||
		len(acc.GitHub) != 1 || acc.GitHub[0].Host != strings.TrimPrefix(ghSrv.URL, "http://") {
		t.Fatalf("access %d %s", code, b)
	}

	eng2 := e.seat("engineer")
	code, b = eng2.do("POST", runtimeapi.PathCredentials+"github", struct{}{})
	var cred runtimeapi.CredentialResponse
	_ = json.Unmarshal(b, &cred)
	if code != http.StatusOK || !strings.HasPrefix(cred.Token, "ghs_") || !cred.Revocable || cred.Username != "x-access-token" {
		t.Fatalf("credential %d %s", code, b)
	}
	if tok := gh.State().Tokens; strings.Join(tok[len(tok)-1].Repos, ",") != "sandbox" {
		t.Fatalf("token not scoped to the granted repository: %+v", tok)
	}
	// A seat without the grant is refused.
	if code, b := e.request("POST", runtimeapi.PathCredentials+"github", "tok-reviewer", nil, struct{}{}); code != http.StatusForbidden {
		t.Fatalf("ungranted seat: %d %s", code, b)
	}

	// Removing the grant revokes the delivered token at GitHub.
	eng.AccessProfiles = nil
	sp.Seats["engineer"] = eng
	e.sync(sp)
	revoked := false
	for _, tk := range gh.State().Tokens {
		revoked = revoked || (tk.Token == cred.Token && tk.Revoked)
	}
	if !revoked {
		t.Fatal("the delivered token was not revoked at GitHub")
	}
	if code, _ := eng2.do("POST", runtimeapi.PathCredentials+"github", struct{}{}); code != http.StatusForbidden {
		t.Fatalf("credential after revocation: %d", code)
	}
}
