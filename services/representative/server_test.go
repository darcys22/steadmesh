package representative

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/darcys22/steadmesh/services/store"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type testAuth struct {
	mu      sync.Mutex
	revoked bool
}

func (a *testAuth) Authenticate(r *http.Request) (Principal, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.revoked || r.Header.Get("Authorization") != "Bearer test-token" {
		return Principal{}, ErrUnauthenticated
	}
	return Principal{ID: "dot", OrganizationID: "organization", RepresentativeSeatKey: "rep", CallerID: "dot"}, nil
}
func (*testAuth) Discovery(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	io.WriteString(w, `{"resource":"https://example.test/mcp"}`)
}
func (*testAuth) Challenge() string {
	return `Bearer resource_metadata="https://example.test/.well-known/oauth-protected-resource/mcp"`
}

type testStore struct {
	mu            sync.Mutex
	principal     store.ExternalPrincipal
	sent          store.ExternalMessage
	offset, limit int
	failure       error
}

func (s *testStore) SendExternalMessage(_ context.Context, p store.ExternalPrincipal, in store.ExternalMessage) (*store.ExternalRequest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.principal = p
	s.sent = in
	return &store.ExternalRequest{RequestID: "request", ConversationID: "conversation", Status: "queued", Replies: []store.ExternalConversationMessage{}}, s.failure
}
func (s *testStore) ExternalRequestStatus(_ context.Context, p store.ExternalPrincipal, id string) (*store.ExternalRequest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.principal = p
	return &store.ExternalRequest{RequestID: id, ConversationID: "conversation", Status: "running", Replies: []store.ExternalConversationMessage{}}, s.failure
}
func (s *testStore) ReadExternalConversation(_ context.Context, p store.ExternalPrincipal, id string, offset, limit int) (*store.ExternalConversation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.principal = p
	s.offset = offset
	s.limit = limit
	return &store.ExternalConversation{ConversationID: id, Messages: []store.ExternalConversationMessage{}, HasMore: true, NextOffset: offset + limit}, s.failure
}

type bearerTransport struct {
	token string
	base  http.RoundTripper
}

func (t bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+t.token)
	return t.base.RoundTrip(r)
}
func connectClient(t *testing.T, srv *httptest.Server, version string) *mcp.ClientSession {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	session, err := mcp.NewClient(&mcp.Implementation{Name: "gateway-test", Version: "1"}, nil).Connect(ctx, &mcp.StreamableClientTransport{Endpoint: srv.URL + "/mcp", HTTPClient: &http.Client{Transport: bearerTransport{"test-token", srv.Client().Transport}}}, &mcp.ClientSessionOptions{ProtocolVersion: version})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	return session
}

func TestOfficialMCPClientRoundTrip(t *testing.T) {
	for _, version := range []string{"2025-03-26", "2025-06-18", "2025-11-25", "2026-07-28"} {
		t.Run(version, func(t *testing.T) { officialMCPClientRoundTrip(t, version) })
	}
}
func officialMCPClientRoundTrip(t *testing.T, version string) {
	st := &testStore{}
	auth := &testAuth{}
	srv := httptest.NewServer(New(st, auth))
	defer srv.Close()
	session := connectClient(t, srv, version)
	ctx := context.Background()
	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools.Tools) != 3 {
		t.Fatalf("tools: %+v", tools)
	}
	for _, tool := range tools.Tools {
		if !strings.HasPrefix(tool.Name, "representative_") {
			t.Fatalf("privileged tool exposed: %s", tool.Name)
		}
	}
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "representative_send", Arguments: map[string]any{"message": "Please ask the team for a status update", "idempotency_key": "stable-1"}})
	if err != nil || result.IsError {
		t.Fatalf("send %v %+v", err, result)
	}
	if !strings.Contains(result.Content[0].(*mcp.TextContent).Text, `"status":"queued"`) {
		t.Fatalf("send result: %+v", result)
	}
	if st.principal.CallerID != "dot" || st.principal.OrganizationID != "organization" || st.sent.IdempotencyKey != "stable-1" {
		t.Fatalf("scope/input: %+v %+v", st.principal, st.sent)
	}
	result, err = session.CallTool(ctx, &mcp.CallToolParams{Name: "representative_status", Arguments: map[string]any{"request_id": "request"}})
	if err != nil || result.IsError {
		t.Fatalf("status %v %+v", err, result)
	}
	result, err = session.CallTool(ctx, &mcp.CallToolParams{Name: "representative_read", Arguments: map[string]any{"conversation_id": "conversation", "cursor": "10", "limit": 5}})
	if err != nil || result.IsError {
		t.Fatalf("read %v %+v", err, result)
	}
	if st.offset != 10 || st.limit != 5 || !strings.Contains(result.Content[0].(*mcp.TextContent).Text, `"next_cursor":"15"`) {
		t.Fatalf("pagination %+v", result)
	}
	// Client-controlled scope overrides are rejected by schema/handler.
	result, err = session.CallTool(ctx, &mcp.CallToolParams{Name: "representative_send", Arguments: map[string]any{"message": "x", "idempotency_key": "k", "organization_id": "victim"}})
	if err == nil && !result.IsError {
		t.Fatal("accepted organization override")
	}
	auth.mu.Lock()
	auth.revoked = true
	auth.mu.Unlock()
	if _, err = session.ListTools(ctx, nil); err == nil {
		t.Fatal("existing initialized client survived revocation")
	}
}

func TestMCPHTTPBoundaries(t *testing.T) {
	srv := httptest.NewServer(New(&testStore{}, &testAuth{}))
	defer srv.Close()
	tests := []struct {
		name, method, path, body, auth, origin string
		want                                   int
	}{
		{"missing token", "POST", "/mcp", "{}", "", "", 401},
		{"query token", "POST", "/mcp?access_token=test-token", "{}", "test-token", "", 400},
		{"privileged API", "POST", "/internal/v1/sync", "{}", "test-token", "", 404},
		{"seat tools", "GET", "/v1/tools", "", "test-token", "", 404},
		{"cross origin", "POST", "/mcp", "{}", "test-token", "https://evil.example", 403},
		{"oversized", "POST", "/mcp", strings.Repeat("x", (64<<10)+1), "test-token", "", 413},
		{"no SSE session", "GET", "/mcp", "", "test-token", "", 405},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, _ := http.NewRequest(tt.method, srv.URL+tt.path, strings.NewReader(tt.body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json, text/event-stream")
			if tt.auth != "" {
				req.Header.Set("Authorization", "Bearer "+tt.auth)
			}
			if tt.origin != "" {
				req.Header.Set("Origin", tt.origin)
			}
			res, err := srv.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			if res.StatusCode != tt.want {
				b, _ := io.ReadAll(res.Body)
				t.Fatalf("%d want %d: %s", res.StatusCode, tt.want, b)
			}
		})
	}
}

func TestLegacyInitializeAndToolDiscovery(t *testing.T) {
	srv := httptest.NewServer(New(&testStore{}, &testAuth{}))
	defer srv.Close()
	for _, version := range []string{"2025-03-26", "2025-06-18", "2025-11-25"} {
		t.Run(version, func(t *testing.T) {
			body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"` + version + `","clientInfo":{"name":"legacy-test","version":"1"},"capabilities":{}}}`
			req, _ := http.NewRequest("POST", srv.URL+"/mcp", strings.NewReader(body))
			req.Header.Set("Authorization", "Bearer test-token")
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json, text/event-stream")
			res, err := srv.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			var decoded map[string]any
			if err = json.NewDecoder(res.Body).Decode(&decoded); err != nil {
				t.Fatal(err)
			}
			result, ok := decoded["result"].(map[string]any)
			if res.StatusCode != 200 || !ok || result["protocolVersion"] != version {
				t.Fatalf("initialize %d %+v", res.StatusCode, decoded)
			}
			if res.Header.Get("Mcp-Session-Id") != "" {
				t.Fatal("unexpected retained session")
			}
		})
	}
}

func TestRateLimitsBounded(t *testing.T) {
	l := &limiter{}
	for i := 0; i < 10; i++ {
		if !l.allow("caller", 10) {
			t.Fatal("premature rate limit")
		}
	}
	if l.allow("caller", 10) {
		t.Fatal("missing limit")
	}
	l.mu.Lock()
	l.windows["caller"] = window{start: time.Now().Add(-time.Minute), count: 10}
	l.mu.Unlock()
	if !l.allow("caller", 10) {
		t.Fatal("window failed to reset")
	}
	for i := 0; i < 5000; i++ {
		l.allow(string(rune(i+1)), 1)
	}
	if len(l.windows) > 4096 {
		t.Fatal("unbounded buckets")
	}
}

func TestDiscoveryOPTIONSReachSDK(t *testing.T) {
	path := authTestFile(t, authTestConfig())
	auth, err := NewFileAuthenticator(path)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(&testStore{}, auth))
	defer srv.Close()
	for _, path := range []string{"/.well-known/oauth-protected-resource", "/.well-known/oauth-protected-resource/mcp"} {
		req, _ := http.NewRequest(http.MethodOptions, srv.URL+path, nil)
		req.Header.Set("Origin", "https://client.example")
		req.Header.Set("Access-Control-Request-Method", "GET")
		res, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != 204 || res.Header.Get("Access-Control-Allow-Origin") != "*" {
			t.Fatalf("preflight: %d %+v", res.StatusCode, res.Header)
		}
	}
}

func TestUnauthenticatedAdmissionBound(t *testing.T) {
	handler := New(&testStore{}, &testAuth{})
	rejected := false
	for i := 0; i < 100; i++ {
		req := httptest.NewRequest("POST", "http://localhost/mcp", strings.NewReader("{}"))
		result := httptest.NewRecorder()
		handler.ServeHTTP(result, req)
		if result.Code == http.StatusTooManyRequests {
			rejected = true
			if result.Header().Get("Retry-After") != "1" {
				t.Fatal("missing retry hint")
			}
			break
		}
	}
	if !rejected {
		t.Fatal("unauthenticated requests are unbounded")
	}
}
