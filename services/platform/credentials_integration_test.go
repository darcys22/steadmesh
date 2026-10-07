//go:build integration

package platform_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/darcys22/steadmesh/connectors"
	"github.com/darcys22/steadmesh/pkg/runtimeapi"
	"github.com/darcys22/steadmesh/pkg/spec"
	"github.com/darcys22/steadmesh/services/internal/fakeconn"
	"github.com/darcys22/steadmesh/services/internal/orgfixture"
	"github.com/darcys22/steadmesh/services/platform"
)

// upstream is a model provider that accepts only its currently valid keys.
type upstream struct {
	mu    sync.Mutex
	valid map[string]bool
}

func (u *upstream) ok(key string) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.valid[key]
}

func (u *upstream) set(key string, ok bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.valid[key] = ok
}

// keyedModel echoes which key served a request and the body it received.
type keyedModel struct {
	key string
	up  *upstream
}

func (m keyedModel) Verify(context.Context) error {
	if !m.up.ok(m.key) {
		return fmt.Errorf("%w: 401 invalid x-api-key", connectors.ErrUnauthorized)
	}
	return nil
}

func (m keyedModel) Proxy() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !m.up.ok(m.key) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"type":"authentication_error"}}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"key": m.key, "body": string(body)})
	})
}

func credentialsEnv(t *testing.T) (*env, *fakeconn.Secrets, *upstream) {
	t.Helper()
	secrets := &fakeconn.Secrets{}
	secrets.Set("k8s:anthropic", map[string]string{"api_key": "k1"})
	up := &upstream{valid: map[string]bool{"k1": true}}
	e := newEnvWith(t, true, func(o *platform.Options) {
		o.Secrets = secrets
		o.CredentialRefresh = time.Hour // only use-time and verify refreshes
		o.Factories.Model = map[string]func(connectors.Config) (connectors.Model, error){
			"anthropic": func(cfg connectors.Config) (connectors.Model, error) {
				return keyedModel{key: cfg.Secret["api_key"], up: up}, nil
			}}
	})
	sp := orgfixture.Spec()
	sp.Connections["model"] = spec.Connection{Adapter: "anthropic", SecretRef: "k8s:anthropic"}
	sp.HarnessProfiles["claude"] = spec.HarnessProfile{Adapter: "claude-code", ImageDigest: "seat:dev", Model: &spec.ModelSelection{Connection: "model", ID: "claude"}}
	sp.Seats["lead"] = func(s spec.Seat) spec.Seat { s.HarnessProfile = "claude"; return s }(sp.Seats["lead"])
	e.sync(sp)
	return e, secrets, up
}

func (e *env) infer(body string) (int, map[string]string) {
	e.t.Helper()
	var payload any
	_ = json.Unmarshal([]byte(body), &payload)
	code, b := e.request("POST", "/v1/model/model/v1/messages", "", map[string]string{"X-Api-Key": "tok-lead"}, payload)
	out := map[string]string{}
	_ = json.Unmarshal(b, &out)
	return code, out
}

func (e *env) verify() runtimeapi.VerifyResponse {
	e.t.Helper()
	code, b := e.request("POST", runtimeapi.PathInternalOrgs+e.org+"/verify", controllerToken, nil, struct{}{})
	if code != http.StatusOK {
		e.t.Fatalf("verify: %d %s", code, b)
	}
	var v runtimeapi.VerifyResponse
	_ = json.Unmarshal(b, &v)
	return v
}

func TestModelProxyRetriesOnceWithRotatedCredential(t *testing.T) {
	e, secrets, up := credentialsEnv(t)
	if code, out := e.infer(`{"model":"claude","n":1}`); code != http.StatusOK || out["key"] != "k1" {
		t.Fatalf("before rotation: %d %v", code, out)
	}
	// The key is rotated in the secret and revoked upstream. No refresh has
	// run yet, so the first upstream attempt is rejected; the platform
	// reloads the secret and retries before anything reaches the seat.
	up.set("k2", true)
	up.set("k1", false)
	secrets.Set("k8s:anthropic", map[string]string{"api_key": "k2"})
	code, out := e.infer(`{"model":"claude","n":2}`)
	if code != http.StatusOK || out["key"] != "k2" || !strings.Contains(out["body"], `"n":2`) {
		t.Fatalf("after rotation: %d %v", code, out)
	}
	// Without a usable replacement the upstream 401 reaches the seat as is.
	up.set("k2", false)
	if code, _ := e.infer(`{"model":"claude"}`); code != http.StatusUnauthorized {
		t.Fatalf("revoked without replacement: %d", code)
	}
}

func TestVerifyReportsCredentialStatus(t *testing.T) {
	e, secrets, up := credentialsEnv(t)
	v := e.verify()
	if c := v.Credentials["model"]; c.State != runtimeapi.CredentialCurrent || c.SecretVersion == "" {
		t.Fatalf("initial credential %+v", c)
	}
	// An invalid replacement is never activated: the previous key keeps
	// serving and the reason is reported, and stored for the console.
	secrets.Set("k8s:anthropic", map[string]string{"api_key": "bad"})
	v = e.verify()
	c := v.Credentials["model"]
	if c.State != runtimeapi.CredentialReplacementRejected || c.PreviousUntil == nil || !v.Connections["model"].OK {
		t.Fatalf("rejected replacement: %+v %+v", c, v.Connections["model"])
	}
	if code, out := e.infer(`{"model":"claude"}`); code != http.StatusOK || out["key"] != "k1" {
		t.Fatalf("previous key not in use: %d %v", code, out)
	}
	var org runtimeapi.ConsoleOrganization
	e.console(runtimeapi.PathConsoleOrgs+"/"+e.org, &org)
	for _, conn := range org.Connections {
		if conn.Key == "model" && (conn.Check == nil || conn.Check.Credential.State != runtimeapi.CredentialReplacementRejected) {
			t.Fatalf("stored check %+v", conn.Check)
		}
	}
	// A valid replacement recovers without Terraform or a restart.
	up.set("k3", true)
	secrets.Set("k8s:anthropic", map[string]string{"api_key": "k3"})
	if c := e.verify().Credentials["model"]; c.State != runtimeapi.CredentialCurrent {
		t.Fatalf("after valid replacement %+v", c)
	}
	if code, out := e.infer(`{"model":"claude"}`); code != http.StatusOK || out["key"] != "k3" {
		t.Fatalf("new key not in use: %d %v", code, out)
	}
	// No secret value appears in any status.
	b, _ := json.Marshal(e.verify())
	for _, secret := range []string{"k1", "k3", "bad"} {
		if strings.Contains(string(b), `"`+secret+`"`) {
			t.Fatalf("verify response contains a secret value %q: %s", secret, b)
		}
	}
}
