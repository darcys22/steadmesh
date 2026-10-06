package client

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/darcys22/steadmesh/pkg/runtimeapi"
)

func writeToken(t *testing.T, path, tok string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(tok+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func newTestClient(t *testing.T, h http.Handler, gen *atomic.Int64) (*Client, string) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	tf := filepath.Join(t.TempDir(), "token")
	writeToken(t, tf, "tok-1")
	c, err := New(Config{BaseURL: srv.URL, TokenFile: tf, Generation: gen.Load, Execution: func() string { return "exec-1" }})
	if err != nil {
		t.Fatal(err)
	}
	return c, tf
}

func TestTokenReReadOnEveryRequest(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("Authorization"))
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(runtimeapi.Self{SeatKey: "alice"})
	})
	var gen atomic.Int64
	c, tf := newTestClient(t, h, &gen)
	ctx := context.Background()
	if _, err := c.Self(ctx); err != nil {
		t.Fatal(err)
	}
	writeToken(t, tf, "tok-2")
	if _, err := c.Self(ctx); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 || seen[0] != "Bearer tok-1" || seen[1] != "Bearer tok-2" {
		t.Fatalf("authorization headers = %v", seen)
	}
}

func TestGenerationAndExecutionHeaders(t *testing.T) {
	var got []string
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Header.Get(runtimeapi.HeaderGeneration)+"|"+r.Header.Get(runtimeapi.HeaderExecution))
		w.WriteHeader(http.StatusNoContent)
	})
	var gen atomic.Int64
	c, _ := newTestClient(t, h, &gen)
	ctx := context.Background()
	_ = c.ReportState(ctx, runtimeapi.StateRequest{State: "Warm"})
	gen.Store(7)
	_ = c.ReportState(ctx, runtimeapi.StateRequest{State: "Warm"})
	if got[0] != "|exec-1" || got[1] != "7|exec-1" {
		t.Fatalf("headers = %v", got)
	}
}

func TestErrorDecoding(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   error
	}{
		{409, `{"code":"fenced","message":"stale generation 3"}`, ErrFenced},
		{409, `{"code":"conflict","message":"revision"}`, ErrConflict},
		{403, `{"code":"forbidden","message":"no grant"}`, ErrForbidden},
		{404, `{"code":"not_found"}`, ErrNotFound},
		{401, `not json`, ErrUnauthenticated},
		{503, ``, ErrUnavailable},
	}
	for _, tc := range cases {
		h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(tc.body))
		})
		var gen atomic.Int64
		c, _ := newTestClient(t, h, &gen)
		_, err := c.RenewLease(context.Background(), 3)
		if !errors.Is(err, tc.want) {
			t.Errorf("status %d body %q: err %v, want %v", tc.status, tc.body, err, tc.want)
		}
		var ae *APIError
		if !errors.As(err, &ae) || ae.Status != tc.status {
			t.Errorf("status %d: not an APIError with status: %v", tc.status, err)
		}
	}
	// Fenced is distinct from conflict even though both are 409.
	err := &APIError{Status: 409, Code: "fenced"}
	if errors.Is(err, ErrConflict) || !IsFenced(err) {
		t.Fatal("fenced must not match conflict")
	}
}

func TestInboxLongPoll(t *testing.T) {
	var calls atomic.Int32
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != runtimeapi.PathInboxNext || r.URL.Query().Get("wait") != "25" {
			t.Errorf("unexpected %s?%s", r.URL.Path, r.URL.RawQuery)
		}
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		_ = json.NewEncoder(w).Encode(runtimeapi.InboxDelivery{DeliveryID: 5, ExecutionID: "e5", Message: runtimeapi.Envelope{MessageID: "m1", Body: "hi"}})
	})
	var gen atomic.Int64
	gen.Store(2)
	c, _ := newTestClient(t, h, &gen)
	d, err := c.InboxNext(context.Background(), 25*time.Second)
	if err != nil || d != nil {
		t.Fatalf("first poll: %v %v", d, err)
	}
	d, err = c.InboxNext(context.Background(), 25*time.Second)
	if err != nil || d == nil || d.DeliveryID != 5 || d.Message.Body != "hi" {
		t.Fatalf("second poll: %+v %v", d, err)
	}
}

func TestPathsAndBodies(t *testing.T) {
	type rec struct{ method, path, body string }
	var got []rec
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, 4096)
		n, _ := r.Body.Read(b)
		got = append(got, rec{r.Method, r.URL.EscapedPath(), string(b[:n])})
		switch r.URL.Path {
		case runtimeapi.PathTools:
			_, _ = w.Write([]byte(`{"tools":[{"name":"memory.write","description":"d","input_schema":{"type":"object"}}]}`))
		case runtimeapi.PathToolCall + "memory.write":
			_, _ = w.Write([]byte(`{"content":{"id":"r1"}}`))
		default:
			w.WriteHeader(http.StatusOK)
		}
	})
	var gen atomic.Int64
	gen.Store(4)
	c, _ := newTestClient(t, h, &gen)
	ctx := context.Background()
	tools, err := c.ListTools(ctx)
	if err != nil || len(tools) != 1 || tools[0].Name != "memory.write" {
		t.Fatalf("tools %v %v", tools, err)
	}
	res, err := c.CallTool(ctx, "memory.write", json.RawMessage(`{"store":"s"}`))
	if err != nil || string(res.Content) != `{"id":"r1"}` {
		t.Fatalf("call %s %v", res.Content, err)
	}
	if err := c.Ack(ctx, 12, runtimeapi.InboxAckRequest{ExecutionID: "e", Outcome: "completed"}); err != nil {
		t.Fatal(err)
	}
	if err := c.PostEvents(ctx, "e1", []runtimeapi.ExecutionEvent{{Kind: "output"}}); err != nil {
		t.Fatal(err)
	}
	if err := c.PutCheckpoint(ctx, runtimeapi.Checkpoint{HarnessAdapter: "fake"}); err != nil {
		t.Fatal(err)
	}
	if err := c.ReleaseLease(ctx, 4); err != nil {
		t.Fatal(err)
	}
	want := []rec{
		{"GET", "/v1/tools", ""},
		{"POST", "/v1/tools/memory.write", `{"arguments":{"store":"s"}}`},
		{"POST", "/v1/inbox/12/ack", `{"execution_id":"e","outcome":"completed"}`},
		{"POST", "/v1/executions/e1/events", `[{"kind":"output","time":"0001-01-01T00:00:00Z"}]`},
		{"PUT", "/v1/checkpoint", `{"harness_adapter":"fake","format_version":"","checkpoint_ref":"","guarantee":""}`},
		{"POST", "/v1/lease/release", `{"generation":4}`},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d requests: %+v", len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("request %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if c.ModelProxyURL("model") != c.BaseURL()+"/v1/model/model" {
		t.Error(c.ModelProxyURL("model"))
	}
}

func TestListToolsBareArray(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"name":"self","input_schema":{"type":"object"}}]`))
	})
	var gen atomic.Int64
	c, _ := newTestClient(t, h, &gen)
	tools, err := c.ListTools(context.Background())
	if err != nil || len(tools) != 1 || tools[0].Name != "self" {
		t.Fatalf("%v %v", tools, err)
	}
}

func TestGenerationFromFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvRunnerDir, dir)
	t.Setenv(EnvGeneration, "")
	src := GenerationFromEnvOrFile()
	if src() != 0 {
		t.Fatal("expected 0 without file")
	}
	if err := WriteFileAtomic(filepath.Join(dir, "generation"), []byte("42\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if src() != 42 {
		t.Fatalf("got %d", src())
	}
	t.Setenv(EnvGeneration, "9")
	if src() != 9 {
		t.Fatalf("env override: got %d", src())
	}
}
