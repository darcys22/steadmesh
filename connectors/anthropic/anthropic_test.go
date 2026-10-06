package anthropic

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/darcys22/steadmesh/connectors"
)

type upstream struct {
	once    sync.Once
	mu      sync.Mutex
	headers http.Header
	path    string
	body    string
	release chan struct{}
}

func (u *upstream) unblock() { u.once.Do(func() { close(u.release) }) }

func (u *upstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, err := io.ReadAll(r.Body)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	u.mu.Lock()
	u.headers, u.path, u.body = r.Header.Clone(), r.URL.Path, string(b)
	u.mu.Unlock()
	switch r.URL.Path {
	case "/v1/models":
		if r.Header.Get("X-Api-Key") != "sk-real" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		fmt.Fprint(w, `{"data":[]}`)
	case "/v1/messages":
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: message_start\ndata: {}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-u.release:
		case <-r.Context().Done():
			return
		}
		fmt.Fprint(w, "event: message_stop\ndata: {}\n\n")
	}
}

func setup(t *testing.T, key string, extra map[string]string) (*upstream, *httptest.Server, *Adapter) {
	t.Helper()
	up := &upstream{release: make(chan struct{})}
	us := httptest.NewServer(up)
	t.Cleanup(us.Close)
	t.Cleanup(up.unblock)
	a, err := newAdapter(connectors.Config{Endpoint: us.URL, Secret: map[string]string{"api_key": key}, Extra: extra})
	if err != nil {
		t.Fatal(err)
	}
	ps := httptest.NewServer(a.Proxy())
	t.Cleanup(ps.Close)
	return up, ps, a
}

func TestVerify(t *testing.T) {
	_, _, a := setup(t, "sk-real", nil)
	if err := a.Verify(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, _, bad := setup(t, "sk-wrong", nil)
	if err := bad.Verify(context.Background()); !errors.Is(err, connectors.ErrUnauthorized) {
		t.Fatalf("want ErrUnauthorized, got %v", err)
	}
}

func TestNewRequiresKey(t *testing.T) {
	if _, err := New(connectors.Config{}); !errors.Is(err, connectors.ErrUnauthorized) {
		t.Fatalf("got %v", err)
	}
}

func TestProxyStripsAndInjectsHeaders(t *testing.T) {
	up, ps, _ := setup(t, "sk-real", nil)
	up.unblock()
	req, _ := http.NewRequest(http.MethodPost, ps.URL+"/v1/messages", strings.NewReader(`{"model":"m"}`))
	req.Header.Set("Authorization", "Bearer seat-token")
	req.Header.Set("X-Api-Key", "sk-seat-supplied")
	req.Header.Set("X-Steadmesh-Generation", "7")
	req.Header.Set("Anthropic-Version", "2023-06-01")
	req.Header.Set("Anthropic-Beta", "tools-2024")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()

	up.mu.Lock()
	defer up.mu.Unlock()
	h := up.headers
	if h.Get("Authorization") != "" || h.Get("X-Steadmesh-Generation") != "" {
		t.Fatalf("seat headers leaked: %v", h)
	}
	if got := h.Values("X-Api-Key"); len(got) != 1 || got[0] != "sk-real" {
		t.Fatalf("x-api-key = %v", got)
	}
	if h.Get("Anthropic-Version") != "2023-06-01" || h.Get("Anthropic-Beta") != "tools-2024" {
		t.Fatalf("anthropic headers not passed: %v", h)
	}
	if up.path != "/v1/messages" || up.body != `{"model":"m"}` {
		t.Fatalf("path=%q body=%q", up.path, up.body)
	}
}

func TestProxyDefaultsAnthropicVersion(t *testing.T) {
	up, ps, _ := setup(t, "sk-real", nil)
	resp, err := http.Get(ps.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	up.mu.Lock()
	defer up.mu.Unlock()
	if up.headers.Get("Anthropic-Version") != DefaultAPIVersion {
		t.Fatalf("headers %v", up.headers)
	}
}

func TestProxyStreams(t *testing.T) {
	up, ps, _ := setup(t, "sk-real", nil)
	resp, err := http.Post(ps.URL+"/v1/messages", "application/json", strings.NewReader(`{"stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	r := bufio.NewReader(resp.Body)

	got := make(chan string, 1)
	go func() {
		line, _ := r.ReadString('\n')
		got <- line
	}()
	select {
	case line := <-got:
		if line != "event: message_start\n" {
			t.Fatalf("first line %q", line)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("first event was buffered instead of streamed")
	}
	up.unblock()
	rest, _ := io.ReadAll(r)
	if !strings.Contains(string(rest), "message_stop") {
		t.Fatalf("rest = %q", rest)
	}
}

func TestProxyBoundsRequestBody(t *testing.T) {
	_, ps, _ := setup(t, "sk-real", map[string]string{"max_request_bytes": "10"})

	resp, err := http.Post(ps.URL+"/v1/messages", "application/json", strings.NewReader(strings.Repeat("x", 100)))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("content-length: status %d", resp.StatusCode)
	}

	// Chunked body with no Content-Length.
	pr, pw := io.Pipe()
	go func() { _, _ = pw.Write([]byte(strings.Repeat("y", 100))); pw.Close() }()
	req, _ := http.NewRequest(http.MethodPost, ps.URL+"/v1/messages", pr)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("chunked: status %d", resp.StatusCode)
	}
}
