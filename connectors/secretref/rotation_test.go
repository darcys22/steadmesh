package secretref

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/darcys22/steadmesh/connectors"
)

func TestDigestTracksContentOnly(t *testing.T) {
	a := Digest(map[string]string{"api_key": "k1", "team": "x"})
	if a != Digest(map[string]string{"team": "x", "api_key": "k1"}) {
		t.Fatal("digest depends on map order")
	}
	if a == Digest(map[string]string{"api_key": "k2", "team": "x"}) {
		t.Fatal("digest ignores a value change")
	}
	// Key/value boundaries are unambiguous.
	if Digest(map[string]string{"ab": "c"}) == Digest(map[string]string{"a": "bc"}) {
		t.Fatal("digest is ambiguous across key/value boundaries")
	}
}

func TestKubernetesVersionAndWatch(t *testing.T) {
	cs := fake.NewClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "linear", Namespace: "sys", ResourceVersion: "7"},
		Data:       map[string][]byte{"api_key": []byte("k1")},
	})
	r := NewKubernetes(cs, "sys")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	got, err := r.Resolve(ctx, "k8s:linear")
	if err != nil || got.Version != "7" {
		t.Fatalf("resolve = %+v, %v", got, err)
	}

	refs := make(chan string, 8)
	go func() { _ = r.(connectors.SecretWatcher).Watch(ctx, func(ref string) { refs <- ref }) }()
	waitRef := func(want string) {
		t.Helper()
		select {
		case ref := <-refs:
			if ref != want {
				t.Fatalf("notified %q, want %q", ref, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("no notification for %s", want)
		}
	}
	waitRef("k8s:linear") // initial list
	s, _ := cs.CoreV1().Secrets("sys").Get(ctx, "linear", metav1.GetOptions{})
	s.Data["api_key"] = []byte("k2")
	if _, err := cs.CoreV1().Secrets("sys").Update(ctx, s, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	waitRef("k8s:linear")
	if err := cs.CoreV1().Secrets("sys").Delete(ctx, "linear", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	waitRef("k8s:linear")
}

// rotatingVault serves one KV v2 secret whose value and version can change,
// and a renewable login.
type rotatingVault struct {
	mu      sync.Mutex
	value   string
	version int
	logins  int
	renews  int
	lease   int
	noRenew bool
}

func (f *rotatingVault) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.URL.Path {
	case "/v1/auth/kubernetes/login":
		f.logins++
		_ = json.NewEncoder(w).Encode(map[string]any{"auth": map[string]any{
			"client_token": "tok-" + strconv.Itoa(f.logins), "lease_duration": f.lease, "renewable": true}})
	case "/v1/auth/token/renew-self":
		if f.noRenew {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		f.renews++
		_ = json.NewEncoder(w).Encode(map[string]any{"auth": map[string]any{"lease_duration": f.lease, "renewable": true}})
	case "/v1/kv/data/app/linear":
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
			"data": map[string]any{"api_key": f.value}, "metadata": map[string]any{"version": f.version}}})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func newRotatingVault(t *testing.T, f *rotatingVault) *Vault {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	p := filepath.Join(t.TempDir(), "jwt")
	if err := os.WriteFile(p, []byte("sa-jwt"), 0o600); err != nil {
		t.Fatal(err)
	}
	v, err := NewVault(srv.URL, "steadmesh-platform", "kv", p)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestVaultVersionsAndDigest(t *testing.T) {
	f := &rotatingVault{value: "k1", version: 1, lease: 3600}
	v := newRotatingVault(t, f)
	ctx := context.Background()
	a, err := v.Resolve(ctx, "vault:app/linear")
	if err != nil || a.Version != "1" {
		t.Fatalf("%+v %v", a, err)
	}
	// A new version with the same content changes Version but not Digest.
	f.version = 2
	b, _ := v.Resolve(ctx, "vault:app/linear")
	if b.Version != "2" || b.Digest != a.Digest {
		t.Fatalf("metadata-only version: %+v vs %+v", b, a)
	}
	f.value, f.version = "k2", 3
	c, _ := v.Resolve(ctx, "vault:app/linear")
	if c.Digest == a.Digest || c.Values["api_key"] != "k2" {
		t.Fatalf("rotated value not seen: %+v", c)
	}
}

func TestVaultRenewsLoginBeforeExpiry(t *testing.T) {
	f := &rotatingVault{value: "k1", version: 1, lease: 600}
	v := newRotatingVault(t, f)
	ctx := context.Background()
	now := time.Now()
	v.now = func() time.Time { return now }
	if _, err := v.Resolve(ctx, "vault:app/linear"); err != nil {
		t.Fatal(err)
	}
	st := v.LoginStatus()
	if !st.LoggedIn || st.ExpiresAt.IsZero() || f.logins != 1 {
		t.Fatalf("status %+v logins %d", st, f.logins)
	}
	// Inside the renewal window: renew, do not log in again.
	v.now = func() time.Time { return now.Add(590 * time.Second) }
	if _, err := v.Resolve(ctx, "vault:app/linear"); err != nil {
		t.Fatal(err)
	}
	if f.renews != 1 || f.logins != 1 || v.LoginStatus().LastRenew.IsZero() {
		t.Fatalf("renews %d logins %d", f.renews, f.logins)
	}
	// When renewal is refused, fall back to a fresh login.
	f.noRenew = true
	v.now = func() time.Time { return now.Add(20 * time.Minute) }
	if _, err := v.Resolve(ctx, "vault:app/linear"); err != nil {
		t.Fatal(err)
	}
	if f.logins != 2 || v.LoginStatus().LastError != "" {
		t.Fatalf("logins %d status %+v", f.logins, v.LoginStatus())
	}
}
