//go:build live

package live

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/darcys22/steadmesh/connectors"
	"github.com/darcys22/steadmesh/connectors/model"
	"github.com/darcys22/steadmesh/harnesses"
	"github.com/darcys22/steadmesh/harnesses/claudecode"
	"github.com/darcys22/steadmesh/harnesses/codex"
	"github.com/darcys22/steadmesh/harnesses/conformance"
	"github.com/darcys22/steadmesh/harnesses/pi"
	"github.com/darcys22/steadmesh/pkg/runtimeapi"
	"github.com/darcys22/steadmesh/pkg/spec"
)

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// TestLiveHarnesses runs each real harness CLI (build/harness-bins.sh) on a
// real model endpoint through the real model connector and forwarder: one
// turn in which the model calls the platform self tool over MCP, as the
// seat readiness probe does. Each case is skipped without its credential.
//
//	ANTHROPIC_API_KEY  claude-code and pi on Anthropic Messages (ANTHROPIC_MODEL)
//	OPENAI_API_KEY     codex and pi on OpenAI Responses (OPENAI_MODEL)
//	SELFHOSTED_API_KEY pi on a self-hosted endpoint, OpenAI Chat Completions
//	                   (SELFHOSTED_BASE_URL, required; SELFHOSTED_MODEL)
func TestLiveHarnesses(t *testing.T) {
	anthropicModel := envOr("ANTHROPIC_MODEL", "claude-haiku-4-5-20251001")
	openaiModel := envOr("OPENAI_MODEL", "gpt-5-mini")
	selfhostedModel := envOr("SELFHOSTED_MODEL", "qwen3.8-27b")
	for _, tc := range []struct {
		name, harness, keyEnv, adapter, endpoint, api, model string
		newAdapter                                           func() harnesses.Adapter
	}{
		{"claude-code on anthropic", "claude", "ANTHROPIC_API_KEY", "anthropic", "", harnesses.APIAnthropicMessages, anthropicModel, func() harnesses.Adapter { return claudecode.New() }},
		{"codex on openai responses", "codex", "OPENAI_API_KEY", "openai", "", harnesses.APIOpenAIResponses, openaiModel, func() harnesses.Adapter { return codex.New() }},
		{"pi on openai responses", "pi", "OPENAI_API_KEY", "openai", "", harnesses.APIOpenAIResponses, openaiModel, func() harnesses.Adapter { return pi.New() }},
		{"pi on anthropic", "pi", "ANTHROPIC_API_KEY", "anthropic", "", harnesses.APIAnthropicMessages, anthropicModel, func() harnesses.Adapter { return pi.New() }},
		{"pi on self-hosted chat completions", "pi", "SELFHOSTED_API_KEY", "model", os.Getenv("SELFHOSTED_BASE_URL"), harnesses.APIOpenAIChat, selfhostedModel, func() harnesses.Adapter { return pi.New() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key := os.Getenv(tc.keyEnv)
			if key == "" {
				t.Skipf("needs %s", tc.keyEnv)
			}
			if tc.adapter == "model" && tc.endpoint == "" {
				t.Skip("needs SELFHOSTED_BASE_URL")
			}
			bin := conformance.HarnessBin(t, tc.harness)
			cfg := connectors.Config{Key: "llm", Adapter: tc.adapter, Endpoint: tc.endpoint, Secret: map[string]string{"api_key": key},
				ModelUses: []connectors.ModelUse{{ID: tc.model, API: tc.api}}}
			if tc.adapter == "model" {
				cfg.Model = &spec.ModelEndpoint{APIs: []string{tc.api}, Models: []spec.ModelEntry{{ID: tc.model}}}
			}
			conn, err := model.New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			// Readiness level 1: the endpoint serves the claimed API and model.
			vctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			if err := conn.Verify(vctx); err != nil {
				t.Fatalf("connection probe: %v", err)
			}

			const token = "live-seat-token"
			ts := conformance.NewToolServer(t, token)
			dirs := conformance.NewDirs(t, token)
			env := conformance.Env(dirs, ts, conformance.BuildTools(t))
			chain := conformance.NewProxyChain(t, dirs.TokenFile, token, env.Generation, conn.Proxy())
			env.Model = chain.Endpoint("llm", tc.model, tc.api, nil)
			env.HarnessConfig = map[string]string{"claude_bin": bin, "codex_bin": bin, "pi_bin": bin}

			a := tc.newAdapter()
			go func() {
				for e := range a.Events() {
					if e.Kind == harnesses.EventToolRequest || e.Kind == harnesses.EventError {
						t.Logf("%s %s", e.Kind, e.Data)
					}
				}
			}()
			ctx, cancel2 := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel2()
			if err := a.Prepare(ctx, env); err != nil {
				t.Fatal(err)
			}
			if _, err := a.StartOrResume(ctx, harnesses.RecoveryDescriptor{}); err != nil {
				t.Fatal(err)
			}
			defer a.Stop(context.Background())
			chain.SetExecution("exec-live")
			tr, err := a.Deliver(ctx, harnesses.Delivery{DeliveryID: 1, ExecutionID: "exec-live", Message: runtimeapi.Envelope{
				MessageID: "live-1", Origin: "probe", RecipientSeat: "conformance", Body: harnesses.ProbePrompt, CreatedAt: time.Now()}})
			if err != nil || tr.Status != harnesses.TurnCompleted {
				t.Fatalf("turn: %+v %v", tr, err)
			}
			var self bool
			for _, c := range ts.Calls() {
				self = self || c.Name == "self"
			}
			if !self {
				t.Fatalf("the model did not call the self tool; output %q, tool calls %+v", tr.Output, ts.Calls())
			}
			for _, c := range chain.Calls() {
				if c.Token != token {
					t.Errorf("a model request carried %q instead of the seat token", c.Token)
				}
			}
			t.Logf("%s: %d model requests to %s, model %s over %s; answer %q", tc.name, len(chain.Calls()), hostOf(conn), tc.model, tc.api, strings.TrimSpace(tr.Output))
		})
	}
}

func hostOf(m connectors.Model) string {
	if h, ok := m.(interface{ Host() string }); ok {
		return h.Host()
	}
	return ""
}
