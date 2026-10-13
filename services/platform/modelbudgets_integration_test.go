//go:build integration

package platform_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/darcys22/steadmesh/connectors"
	"github.com/darcys22/steadmesh/pkg/runtimeapi"
	"github.com/darcys22/steadmesh/pkg/spec"
	"github.com/darcys22/steadmesh/services/internal/orgfixture"
	"github.com/darcys22/steadmesh/services/platform"
)

type budgetModel struct {
	calls atomic.Int64
	limit atomic.Int64
	mode  atomic.Int64
}

func (m *budgetModel) Verify(context.Context) error { return nil }
func (m *budgetModel) Proxy() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			MaxTokens int64 `json:"max_tokens"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		m.calls.Add(1)
		m.limit.Store(body.MaxTokens)
		switch m.mode.Load() {
		case 1: // Interrupted streams keep the whole reservation.
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, "data: {\"usage\":{\"output_tokens\":1}}\n\n")
		case 2: // No usage is also conservative.
			_, _ = fmt.Fprint(w, `{"content":"unmetered"}`)
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"type":"message","usage":{"output_tokens":2}}`)
		}
	})
}

func TestModelBudgetProxyAdmissionSettlementAndResume(t *testing.T) {
	model := &budgetModel{}
	e := newEnvWith(t, false, func(o *platform.Options) {
		o.Factories.Model = map[string]func(connectors.Config) (connectors.Model, error){
			"anthropic": func(connectors.Config) (connectors.Model, error) { return model, nil },
		}
	})
	sp := orgfixture.Spec()
	sp.Connections["model"] = spec.Connection{Adapter: "anthropic", SecretRef: "k8s:anthropic"}
	sp.HarnessProfiles["claude"] = spec.HarnessProfile{Adapter: "claude-code", ImageDigest: "seat:dev", Model: &spec.ModelSelection{Connection: "model", ID: "claude-x"}}
	leadSpec := sp.Seats["lead"]
	leadSpec.HarnessProfile = "claude"
	sp.Seats["lead"] = leadSpec
	sp.ModelBudgets = &spec.ModelBudgets{OrganizationDailyOutputTokens: 10, SeatDailyOutputTokens: 10, ExecutionOutputTokens: 10, MaxRequestOutputTokens: 4}
	e.sync(sp)
	lead := e.seat("lead")
	code, b := e.request("POST", runtimeapi.PathInternalSeats+e.seats["lead"]+"/probe", controllerToken, nil, struct{}{})
	if code != http.StatusAccepted && code != http.StatusOK {
		t.Fatalf("probe %d %s", code, b)
	}
	d := lead.next()
	if d == nil {
		t.Fatal("missing execution")
	}
	headers := map[string]string{runtimeapi.HeaderGeneration: fmt.Sprint(lead.gen), runtimeapi.HeaderExecution: d.ExecutionID}
	infer := func(body map[string]any) (int, []byte) {
		return e.request("POST", "/v1/model/model/v1/messages", lead.token, headers, body)
	}
	status := func() runtimeapi.ModelBudgetStatus {
		code, b := e.request("GET", runtimeapi.PathModelBudgets, lead.token, headers, nil)
		var out runtimeapi.ModelBudgetStatus
		if code != 200 || json.Unmarshal(b, &out) != nil {
			t.Fatalf("status %d %s", code, b)
		}
		return out
	}
	if code, b := infer(map[string]any{"model": "claude-x", "max_tokens": 100}); code != 200 {
		t.Fatalf("infer %d %s", code, b)
	}
	if model.limit.Load() != 4 || status().ExecutionOutputTokens != 2 {
		t.Fatalf("limit=%d status=%+v", model.limit.Load(), status())
	}
	model.mode.Store(1)
	if code, b := infer(map[string]any{"model": "claude-x", "stream": true}); code != 200 {
		t.Fatalf("stream %d %s", code, b)
	}
	if status().ExecutionOutputTokens != 6 {
		t.Fatalf("interrupted stream refunded: %+v", status())
	}
	model.mode.Store(2)
	if code, b := infer(map[string]any{"model": "claude-x"}); code != 200 {
		t.Fatalf("missing usage %d %s", code, b)
	}
	if status().ExecutionOutputTokens != 10 {
		t.Fatalf("missing usage refunded: %+v", status())
	}
	calls := model.calls.Load()
	if code, b := infer(map[string]any{"model": "claude-x"}); code != 429 {
		t.Fatalf("exhaustion %d %s", code, b)
	}
	if model.calls.Load() != calls {
		t.Fatal("exhausted request reached provider")
	}
	if code, b := infer(map[string]any{"model": "claude-x", "max_tokens": 0}); code != 400 {
		t.Fatalf("invalid %d %s", code, b)
	}
	if code, _ := e.request("POST", "/v1/model/model/v1/messages", lead.token, nil, map[string]any{"model": "claude-x"}); code != 409 {
		t.Fatalf("missing generation: %d", code)
	}
	// Raising the policy restores admission without clearing accounting.
	sp.ModelBudgets = &spec.ModelBudgets{OrganizationDailyOutputTokens: 20, SeatDailyOutputTokens: 20, ExecutionOutputTokens: 20, MaxRequestOutputTokens: 4}
	e.sync(sp)
	if code, b := infer(map[string]any{"model": "claude-x"}); code != 200 {
		t.Fatalf("resume %d %s", code, b)
	}
	if status().ExecutionOutputTokens != 14 {
		t.Fatalf("usage reset by policy edit: %+v", status())
	}
}
