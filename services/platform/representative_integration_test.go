//go:build integration

package platform_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/darcys22/steadmesh/pkg/runtimeapi"
	"github.com/darcys22/steadmesh/services/internal/orgfixture"
	"github.com/darcys22/steadmesh/services/platform"
	"github.com/darcys22/steadmesh/services/representative"
	"github.com/darcys22/steadmesh/services/store"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type gatewayTransport struct{ token string }

func (t gatewayTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+t.token)
	return http.DefaultTransport.RoundTrip(r)
}

func TestRepresentativeMCPDurableRoundTrip(t *testing.T) {
	var st *store.Store
	e := newEnvWith(t, false, func(o *platform.Options) { st = o.Store })
	e.sync(orgfixture.Spec())
	tokenA, tokenB := strings.Repeat("a", 64), strings.Repeat("b", 64)
	digest := func(token string) string { sum := sha256.Sum256([]byte(token)); return hex.EncodeToString(sum[:]) }
	cfg := representative.Config{ResourceURL: "https://gateway.example/mcp", Principals: []representative.PrincipalConfig{
		{Principal: representative.Principal{ID: "dot-a", OrganizationID: e.org, RepresentativeSeatKey: "rep_a", CallerID: "dot-a"}, BearerSHA256: []string{digest(tokenA)}},
		{Principal: representative.Principal{ID: "dot-b", OrganizationID: e.org, RepresentativeSeatKey: "rep_a", CallerID: "dot-b"}, BearerSHA256: []string{digest(tokenB)}},
	}}
	path := filepath.Join(t.TempDir(), "gateway.json")
	writeConfig := func() {
		raw, _ := json.Marshal(cfg)
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeConfig()
	auth, err := representative.NewFileAuthenticator(path)
	if err != nil {
		t.Fatal(err)
	}
	gateway := httptest.NewServer(representative.New(st, auth))
	defer gateway.Close()
	connect := func(token string) *mcp.ClientSession {
		t.Helper()
		client := mcp.NewClient(&mcp.Implementation{Name: "external-agent-test", Version: "1"}, nil)
		session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: gateway.URL + "/mcp", HTTPClient: &http.Client{Transport: gatewayTransport{token}}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { session.Close() })
		return session
	}
	callerA, callerB := connect(tokenA), connect(tokenB)
	call := func(client *mcp.ClientSession, name string, args any, wantError bool) map[string]any {
		t.Helper()
		result, err := client.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatal(err)
		}
		if result.IsError != wantError {
			t.Fatalf("%s: %+v", name, result)
		}
		var decoded map[string]any
		if err := json.Unmarshal([]byte(result.Content[0].(*mcp.TextContent).Text), &decoded); err != nil {
			t.Fatal(err)
		}
		return decoded
	}
	sent := call(callerA, "representative_send", map[string]any{"message": "Ask the lead for a project update", "idempotency_key": "request-1"}, false)
	requestID, conversationID := sent["request_id"].(string), sent["conversation_id"].(string)
	if sent["status"] != "queued" {
		t.Fatalf("send: %+v", sent)
	}
	duplicate := call(callerA, "representative_send", map[string]any{"message": "Ask the lead for a project update", "idempotency_key": "request-1"}, false)
	if duplicate["request_id"] != requestID || duplicate["duplicate"] != true {
		t.Fatalf("retry: %+v", duplicate)
	}
	call(callerA, "representative_send", map[string]any{"message": "changed", "idempotency_key": "request-1"}, true)
	for _, client := range []*mcp.ClientSession{callerB} {
		call(client, "representative_status", map[string]any{"request_id": requestID}, true)
		call(client, "representative_read", map[string]any{"conversation_id": conversationID}, true)
		call(client, "representative_send", map[string]any{"conversation_id": conversationID, "message": "join", "idempotency_key": "foreign"}, true)
	}
	// The external credential is not a seat or controller credential.
	if code, _ := e.request("GET", runtimeapi.PathSelf, tokenA, nil, nil); code != 401 {
		t.Fatalf("external key on seat API: %d", code)
	}
	if code, _ := e.request("POST", runtimeapi.PathInternalSync, tokenA, nil, map[string]any{}); code != 401 {
		t.Fatalf("external key on controller API: %d", code)
	}
	rep, lead := e.seat("rep_a"), e.seat("lead")
	delivery := rep.next()
	if delivery == nil || delivery.Message.Origin != store.OriginExternalAgent || delivery.Message.ExternalCallerID != "dot-a" || delivery.Message.Binding != "" {
		t.Fatalf("external provenance: %+v", delivery)
	}
	running := call(callerA, "representative_status", map[string]any{"request_id": requestID}, false)
	if running["status"] != "running" {
		t.Fatalf("running: %+v", running)
	}
	// Existing representative routes can activate ordinary swarm work.
	rep.mustTool("messages.send", map[string]any{"to": "lead", "body": "Please check project progress"})
	leadDelivery := lead.next()
	if leadDelivery == nil {
		t.Fatal("lead not activated")
	}
	lead.mustTool("messages.reply", map[string]any{"message_id": leadDelivery.Message.MessageID, "body": "Private internal detail for the representative"})
	lead.ack(leadDelivery)
	// Undeclared direct routes are still denied.
	if _, isError := rep.tool("messages.send", map[string]any{"to": "reviewer", "body": "bypass route"}); !isError {
		t.Fatal("gateway changed representative route permissions")
	}
	rep.mustTool("messages.reply", map[string]any{"message_id": delivery.Message.MessageID, "body": "The lead is checking project progress"})
	if _, isError := lead.tool("messages.reply", map[string]any{"message_id": delivery.Message.MessageID, "body": "spoof representative"}); !isError {
		t.Fatal("other seat replied externally")
	}
	rep.ack(delivery)
	completed := call(callerA, "representative_status", map[string]any{"request_id": requestID}, false)
	if completed["status"] != "completed" || len(completed["replies"].([]any)) != 1 {
		t.Fatalf("completed: %+v", completed)
	}
	history := call(callerA, "representative_read", map[string]any{"conversation_id": conversationID, "limit": 1}, false)
	if history["has_more"] != true || history["next_cursor"] != "1" {
		t.Fatalf("page1: %+v", history)
	}
	page2 := call(callerA, "representative_read", map[string]any{"conversation_id": conversationID, "cursor": history["next_cursor"], "limit": 1}, false)
	raw, _ := json.Marshal(page2)
	if strings.Contains(string(raw), "Private internal detail") || !strings.Contains(string(raw), "The lead is checking") {
		t.Fatalf("private routing: %s", raw)
	}
	// Reconstructing the HTTP handler/client (no MCP sessions retained) leaves
	// request status/history durable in Postgres.
	restarted := httptest.NewServer(representative.New(st, auth))
	defer restarted.Close()
	restartClient := mcp.NewClient(&mcp.Implementation{Name: "restarted-client", Version: "1"}, nil)
	restartSession, err := restartClient.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: restarted.URL + "/mcp", HTTPClient: &http.Client{Transport: gatewayTransport{tokenA}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer restartSession.Close()
	result := call(restartSession, "representative_status", map[string]any{"request_id": requestID}, false)
	if result["status"] != "completed" || len(result["replies"].([]any)) != 1 {
		t.Fatalf("durable restarted MCP status: %+v", result)
	}

	// Revocation takes effect for an already initialized MCP connection.
	cfg.Principals = cfg.Principals[1:]
	writeConfig()
	if _, err := callerA.ListTools(context.Background(), nil); err == nil {
		t.Fatal("removed credential remained authorized")
	}
	if _, err := callerB.ListTools(context.Background(), nil); err != nil {
		t.Fatal("revocation affected another caller")
	}
}
