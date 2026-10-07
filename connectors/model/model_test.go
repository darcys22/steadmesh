package model

import (
	"bufio"
	"context"
	"encoding/json"
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
	"github.com/darcys22/steadmesh/harnesses"
	"github.com/darcys22/steadmesh/pkg/spec"
)

// upstream records requests and answers by path.
type upstream struct {
	once    sync.Once
	mu      sync.Mutex
	reqs    []seen
	release chan struct{}
	// chatOldStyle rejects max_completion_tokens like an older server.
	chatOldStyle bool
	// models the endpoint knows; others get 404.
	models map[string]bool
}

type seen struct {
	headers http.Header
	path    string
	query   string
	body    string
}

func (u *upstream) unblock() { u.once.Do(func() { close(u.release) }) }

func (u *upstream) last() seen {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.reqs[len(u.reqs)-1]
}

func (u *upstream) count() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.reqs)
}

func (u *upstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	u.mu.Lock()
	u.reqs = append(u.reqs, seen{headers: r.Header.Clone(), path: r.URL.Path, query: r.URL.RawQuery, body: string(b)})
	u.mu.Unlock()
	key := r.Header.Get("X-Api-Key")
	if key == "" {
		key = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	}
	if key == "" {
		key = r.Header.Get("X-Custom-Key")
	}
	if key != "sk-real" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	var body struct {
		Model               string `json:"model"`
		MaxCompletionTokens *int   `json:"max_completion_tokens"`
	}
	_ = json.Unmarshal(b, &body)
	if body.Model != "" && u.models != nil && !u.models[body.Model] {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprintf(w, `{"error":{"message":"model %s not found"}}`, body.Model)
		return
	}
	switch r.URL.Path {
	case "/api/v1/models":
		fmt.Fprint(w, `{"data":[]}`)
	case "/api/v1/chat/completions":
		if u.chatOldStyle && body.MaxCompletionTokens != nil {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":{"message":"Unrecognized request argument supplied: max_completion_tokens"}}`)
			return
		}
		fmt.Fprint(w, `{"choices":[]}`)
	case "/api/v1/responses":
		fmt.Fprint(w, `{"output":[]}`)
	case "/api/v1/messages":
		if !strings.Contains(string(b), `"stream":true`) {
			fmt.Fprint(w, `{"content":[]}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: message_start\ndata: {}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-u.release:
		case <-r.Context().Done():
			return
		}
		fmt.Fprint(w, "event: message_stop\ndata: {}\n\n")
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func setup(t *testing.T, cfg connectors.Config) (*upstream, *httptest.Server, *Adapter) {
	t.Helper()
	up := &upstream{release: make(chan struct{})}
	us := httptest.NewServer(up)
	t.Cleanup(us.Close)
	t.Cleanup(up.unblock)
	if cfg.Adapter == "" {
		cfg.Adapter = "anthropic"
	}
	cfg.Endpoint = us.URL + "/api/v1"
	if cfg.Secret == nil {
		cfg.Secret = map[string]string{"api_key": "sk-real"}
	}
	a, err := newAdapter(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ps := httptest.NewServer(a.Proxy())
	t.Cleanup(ps.Close)
	return up, ps, a
}

func TestNewRequiresKeyAndKnownAdapter(t *testing.T) {
	if _, err := New(connectors.Config{Adapter: "openai"}); !errors.Is(err, connectors.ErrUnauthorized) {
		t.Fatalf("missing key: %v", err)
	}
	if _, err := New(connectors.Config{Adapter: "slack", Secret: map[string]string{"api_key": "k"}}); !errors.Is(err, connectors.ErrPermanent) {
		t.Fatalf("foreign adapter: %v", err)
	}
	a, err := newAdapter(connectors.Config{Adapter: "openai", Secret: map[string]string{"api_key": "k"}})
	if err != nil || a.base.String() != OpenAIEndpoint || !a.Serves(harnesses.APIOpenAIChat) || a.Serves(harnesses.APIAnthropicMessages) {
		t.Fatalf("openai preset: %+v %v", a, err)
	}
}

func TestProxyJoinsPathsAndInjectsCredential(t *testing.T) {
	for _, tc := range []struct {
		adapter, auth, path string
		check               func(h http.Header) bool
	}{
		{"anthropic", "", "/v1/messages", func(h http.Header) bool { return h.Get("X-Api-Key") == "sk-real" && h.Get("Authorization") == "" }},
		{"openai", "", "/v1/responses", func(h http.Header) bool {
			return h.Get("Authorization") == "Bearer sk-real" && h.Get("X-Api-Key") == ""
		}},
		{"model", "header:X-Custom-Key", "/v1/chat/completions", func(h http.Header) bool {
			return h.Get("X-Custom-Key") == "sk-real" && h.Get("Authorization") == "" && h.Get("X-Api-Key") == ""
		}},
	} {
		t.Run(tc.adapter, func(t *testing.T) {
			cfg := connectors.Config{Adapter: tc.adapter}
			if tc.auth != "" {
				cfg.Model = &spec.ModelEndpoint{APIs: []string{harnesses.APIOpenAIChat}, Auth: tc.auth}
			}
			up, ps, _ := setup(t, cfg)
			req, _ := http.NewRequest(http.MethodPost, ps.URL+tc.path+"?beta=true", strings.NewReader(`{"model":"m"}`))
			req.Header.Set("Authorization", "Bearer seat-token")
			req.Header.Set("X-Api-Key", "steadmesh-local")
			req.Header.Set("X-Steadmesh-Generation", "7")
			req.Header.Set("Anthropic-Beta", "tools-2024")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.ReadAll(resp.Body)
			resp.Body.Close()
			got := up.last()
			// The base path (/api/v1) replaces the request's /v1.
			if want := "/api" + tc.path; got.path != want || got.query != "beta=true" || got.body != `{"model":"m"}` {
				t.Fatalf("upstream got %s?%s %q, want %s", got.path, got.query, got.body, want)
			}
			if !tc.check(got.headers) || got.headers.Get("X-Steadmesh-Generation") != "" || got.headers.Get("Anthropic-Beta") != "tools-2024" {
				t.Fatalf("headers %v", got.headers)
			}
			if tc.adapter == "anthropic" && got.headers.Get("Anthropic-Version") != DefaultAnthropicVersion {
				t.Fatalf("anthropic-version not defaulted: %v", got.headers)
			}
		})
	}
}

func TestProxyRejectsNonAPIPaths(t *testing.T) {
	_, ps, _ := setup(t, connectors.Config{})
	resp, err := http.Post(ps.URL+"/admin", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestProxyStreams(t *testing.T) {
	up, ps, _ := setup(t, connectors.Config{})
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
	_, ps, _ := setup(t, connectors.Config{Extra: map[string]string{"max_request_bytes": "10"}})
	resp, err := http.Post(ps.URL+"/v1/messages", "application/json", strings.NewReader(strings.Repeat("x", 100)))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("content-length: status %d", resp.StatusCode)
	}
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

func TestVerifyProbesEachAPIAndModel(t *testing.T) {
	cfg := connectors.Config{Adapter: "model",
		Model: &spec.ModelEndpoint{APIs: []string{harnesses.APIOpenAIResponses, harnesses.APIOpenAIChat},
			Models: []spec.ModelEntry{{ID: "a"}, {ID: "b", APIs: []string{harnesses.APIOpenAIChat}}}},
		ModelUses: []connectors.ModelUse{{ID: "c", API: harnesses.APIOpenAIResponses}, {ID: "a", API: harnesses.APIOpenAIChat}}}
	up, _, a := setup(t, cfg)
	now := time.Now()
	a.now = func() time.Time { return now }
	if err := a.Verify(context.Background()); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range up.reqs {
		var b struct{ Model string }
		_ = json.Unmarshal([]byte(r.body), &b)
		got = append(got, strings.TrimPrefix(r.path, "/api/v1/")+":"+b.Model)
	}
	// a over both APIs, b over chat only, c (in use) over responses; the
	// declared pair a/chat is not probed twice.
	want := "responses:a chat/completions:a chat/completions:b responses:c"
	if strings.Join(got, " ") != want {
		t.Fatalf("probes %v, want %s", got, want)
	}
	// Successful probes are trusted until ProbeTTL passes.
	n := up.count()
	if err := a.Verify(context.Background()); err != nil || up.count() != n {
		t.Fatalf("re-probed within the TTL: %d -> %d (%v)", n, up.count(), err)
	}
	now = now.Add(ProbeTTL + time.Minute)
	if err := a.Verify(context.Background()); err != nil || up.count() != 2*n {
		t.Fatalf("not re-probed after the TTL: %d -> %d (%v)", n, up.count(), err)
	}
}

func TestVerifyFailures(t *testing.T) {
	t.Run("unknown model names the API and model", func(t *testing.T) {
		up, _, a := setup(t, connectors.Config{Adapter: "openai", ModelUses: []connectors.ModelUse{{ID: "gpt-missing", API: harnesses.APIOpenAIResponses}}})
		up.models = map[string]bool{"gpt-5.5": true}
		err := a.Verify(context.Background())
		if !errors.Is(err, connectors.ErrPermanent) || !strings.Contains(err.Error(), `openai_responses model "gpt-missing"`) || !strings.Contains(err.Error(), "404") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("rejected key is unauthorized", func(t *testing.T) {
		_, _, a := setup(t, connectors.Config{Adapter: "openai", Secret: map[string]string{"api_key": "sk-wrong"}})
		if err := a.Verify(context.Background()); !errors.Is(err, connectors.ErrUnauthorized) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("old chat servers fall back to max_tokens", func(t *testing.T) {
		up, _, a := setup(t, connectors.Config{Adapter: "openai", ModelUses: []connectors.ModelUse{{ID: "m", API: harnesses.APIOpenAIChat}}})
		up.chatOldStyle = true
		if err := a.Verify(context.Background()); err != nil {
			t.Fatal(err)
		}
		if b := up.last().body; !strings.Contains(b, `"max_tokens":1`) {
			t.Fatalf("fallback body %s", b)
		}
	})
}

func TestVerifyModes(t *testing.T) {
	up, _, a := setup(t, connectors.Config{Adapter: "openai", Model: &spec.ModelEndpoint{Verify: "models"},
		ModelUses: []connectors.ModelUse{{ID: "m", API: harnesses.APIOpenAIChat}}})
	if err := a.Verify(context.Background()); err != nil || up.last().path != "/api/v1/models" {
		t.Fatalf("models mode: %v %s", err, up.last().path)
	}
	up2, _, b := setup(t, connectors.Config{Adapter: "openai", Model: &spec.ModelEndpoint{Verify: "none"}, Secret: map[string]string{"api_key": "sk-wrong"}})
	if err := b.Verify(context.Background()); err != nil || up2.count() != 0 {
		t.Fatalf("none mode: %v, %d requests", err, up2.count())
	}
	// Nothing to probe: list models instead.
	up3, _, c := setup(t, connectors.Config{Adapter: "anthropic"})
	if err := c.Verify(context.Background()); err != nil || up3.last().path != "/api/v1/models" {
		t.Fatalf("no probes: %v", err)
	}
}

func TestAPIForPath(t *testing.T) {
	for path, want := range map[string]string{
		"/v1/messages": harnesses.APIAnthropicMessages, "/v1/messages/count_tokens": harnesses.APIAnthropicMessages,
		"/v1/responses": harnesses.APIOpenAIResponses, "/v1/responses/compact": harnesses.APIOpenAIResponses,
		"/v1/chat/completions": harnesses.APIOpenAIChat, "/v1/models": "models", "/v1/files": "", "/messages": "",
	} {
		if got := APIForPath(path); got != want {
			t.Errorf("%s: %q, want %q", path, got, want)
		}
	}
}
