package secretref

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/darcys22/steadmesh/connectors"
)

func TestKubernetes(t *testing.T) {
	cs := fake.NewClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "slack-creds", Namespace: "steadmesh-system"},
		Data:       map[string][]byte{"bot_token": []byte("xoxb-1"), "app_token": []byte("xapp-1")},
	}, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "elsewhere", Namespace: "org-a"},
		Data:       map[string][]byte{"api_key": []byte("k")},
	})
	r := NewKubernetes(cs, "steadmesh-system")
	ctx := context.Background()

	got, err := r.Resolve(ctx, "k8s:slack-creds")
	if err != nil {
		t.Fatal(err)
	}
	if got["bot_token"] != "xoxb-1" || got["app_token"] != "xapp-1" || len(got) != 2 {
		t.Fatalf("got %v", got)
	}
	// Only the configured namespace is visible.
	if _, err := r.Resolve(ctx, "k8s:elsewhere"); !errors.Is(err, connectors.ErrPermanent) {
		t.Fatalf("other namespace: %v", err)
	}
	for _, ref := range []string{"vault:slack-creds", "k8s:", "slack-creds", "k8s:Bad_Name"} {
		if _, err := r.Resolve(ctx, ref); !errors.Is(err, connectors.ErrPermanent) {
			t.Errorf("%q: want ErrPermanent, got %v", ref, err)
		}
	}
}

type fakeVault struct {
	mu        sync.Mutex
	logins    int
	reads     int
	lease     int
	forbidNow bool
	tokenSeq  int
}

func (f *fakeVault) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/auth/kubernetes/login":
		var in struct{ Role, JWT string }
		_ = json.NewDecoder(r.Body).Decode(&in)
		if in.Role != "steadmesh-platform" || in.JWT != "sa-jwt" {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"errors":["permission denied"]}`))
			return
		}
		f.logins++
		f.tokenSeq++
		_ = json.NewEncoder(w).Encode(map[string]any{"auth": map[string]any{
			"client_token": "tok-" + string(rune('0'+f.tokenSeq)), "lease_duration": f.lease,
		}})
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/kv/data/"):
		f.reads++
		if f.forbidNow || !strings.HasPrefix(r.Header.Get("X-Vault-Token"), "tok-") {
			f.forbidNow = false
			w.WriteHeader(http.StatusForbidden)
			return
		}
		switch strings.TrimPrefix(r.URL.Path, "/v1/kv/data/") {
		case "steadmesh/linear":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
				"data": map[string]any{"api_key": "lin_api_secret", "n": 3}, "metadata": map[string]any{"version": 1},
			}})
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"errors":[]}`))
		}
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func newVault(t *testing.T, f *fakeVault, jwt string) *Vault {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	p := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(p, []byte(jwt+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := NewVault(srv.URL, "steadmesh-platform", "kv", p)
	if err != nil {
		t.Fatal(err)
	}
	return s.(*Vault)
}

func TestVaultResolveAndCache(t *testing.T) {
	f := &fakeVault{lease: 3600}
	v := newVault(t, f, "sa-jwt")
	ctx := context.Background()

	got, err := v.Resolve(ctx, "vault:steadmesh/linear")
	if err != nil {
		t.Fatal(err)
	}
	if got["api_key"] != "lin_api_secret" || got["n"] != "3" {
		t.Fatalf("got %v", got)
	}
	if _, err := v.Resolve(ctx, "vault:steadmesh/linear"); err != nil {
		t.Fatal(err)
	}
	if f.logins != 1 {
		t.Fatalf("token not cached: %d logins", f.logins)
	}

	// After the lease expires, the resolver logs in again.
	now := time.Now()
	v.now = func() time.Time { return now.Add(2 * time.Hour) }
	if _, err := v.Resolve(ctx, "vault:steadmesh/linear"); err != nil {
		t.Fatal(err)
	}
	if f.logins != 2 {
		t.Fatalf("want re-login after lease expiry, got %d logins", f.logins)
	}
}

func TestVaultReloginOnForbidden(t *testing.T) {
	f := &fakeVault{lease: 3600}
	v := newVault(t, f, "sa-jwt")
	ctx := context.Background()
	if _, err := v.Resolve(ctx, "vault:steadmesh/linear"); err != nil {
		t.Fatal(err)
	}
	f.forbidNow = true
	if _, err := v.Resolve(ctx, "vault:steadmesh/linear"); err != nil {
		t.Fatalf("should recover by logging in again: %v", err)
	}
	if f.logins != 2 {
		t.Fatalf("logins = %d", f.logins)
	}
}

func TestVaultErrors(t *testing.T) {
	ctx := context.Background()
	v := newVault(t, &fakeVault{}, "sa-jwt")
	if _, err := v.Resolve(ctx, "vault:steadmesh/missing"); !errors.Is(err, connectors.ErrPermanent) {
		t.Fatalf("missing: %v", err)
	}
	for _, ref := range []string{"vault:../sys/x", "vault:a//b", "k8s:x"} {
		if _, err := v.Resolve(ctx, ref); !errors.Is(err, connectors.ErrPermanent) {
			t.Errorf("%q: %v", ref, err)
		}
	}
	bad := newVault(t, &fakeVault{}, "wrong-jwt")
	_, err := bad.Resolve(ctx, "vault:steadmesh/linear")
	if !errors.Is(err, connectors.ErrUnauthorized) {
		t.Fatalf("bad jwt: %v", err)
	}
	if strings.Contains(err.Error(), "wrong-jwt") {
		t.Fatal("error leaks the service account token")
	}
	if _, err := NewVault("not a url", "r", "", ""); err == nil {
		t.Fatal("want error for bad address")
	}
	if _, err := NewVault("http://vault:8200", "", "", ""); err == nil {
		t.Fatal("want error for missing role")
	}
}

type staticResolver map[string]string

func (s staticResolver) Resolve(context.Context, string) (map[string]string, error) { return s, nil }

func TestMulti(t *testing.T) {
	m := NewMulti(map[string]connectors.Secrets{
		SchemeKubernetes: staticResolver{"from": "k8s"},
		SchemeVault:      staticResolver{"from": "vault"},
		"nil":            nil,
	})
	ctx := context.Background()
	for ref, want := range map[string]string{"k8s:a": "k8s", "vault:a/b": "vault"} {
		got, err := m.Resolve(ctx, ref)
		if err != nil || got["from"] != want {
			t.Errorf("%s: %v %v", ref, got, err)
		}
	}
	for _, ref := range []string{"aws:x", "nil:x", "plain"} {
		if _, err := m.Resolve(ctx, ref); !errors.Is(err, connectors.ErrPermanent) {
			t.Errorf("%s: %v", ref, err)
		}
	}
}
