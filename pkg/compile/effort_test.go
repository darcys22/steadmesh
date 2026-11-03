package compile_test

import (
	"strings"
	"testing"

	"github.com/darcys22/steadmesh/pkg/compile"
	"github.com/darcys22/steadmesh/pkg/spec"
)

func TestPortableEffortCompile(t *testing.T) {
	for _, tc := range []struct{ adapter, connection, effort, setting, native string }{
		{"claude-code", "anthropic", "high", "effort", "high"},
		{"claude", "anthropic", "low", "effort", "low"},
		{"codex", "openai", "high", "reasoning_effort", "high"},
		{"pi", "openai", "none", "thinking", "off"},
	} {
		t.Run(tc.adapter, func(t *testing.T) {
			s := mixed(t, spec.HarnessProfile{Adapter: tc.adapter, Model: &spec.ModelSelection{Connection: tc.connection, ID: "model", Effort: tc.effort}})
			m, err := compile.Compile(s, compile.DefaultCatalog())
			if err != nil {
				t.Fatal(err)
			}
			model := m.Seats["reviewer"].Harness.Model
			if model.Effort != tc.effort || model.Settings[tc.setting] != tc.native {
				t.Fatalf("model=%+v", model)
			}
			if s.HarnessProfiles["h"].Model.Settings != nil {
				t.Fatal("compiler mutated input settings")
			}
		})
	}
}

func TestPortableEffortRejected(t *testing.T) {
	for _, tc := range []struct {
		name, effort string
		settings     map[string]string
		want         string
	}{
		{"unsupported label", "none", nil, "unsupported effort"},
		{"unknown label", "invented", nil, "unsupported effort"},
		{"conflict", "high", map[string]string{"effort": "low"}, "conflicts"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := mixed(t, spec.HarnessProfile{Adapter: "claude-code", Model: &spec.ModelSelection{Connection: "anthropic", ID: "model", Effort: tc.effort, Settings: tc.settings}})
			_, err := compile.Compile(s, compile.DefaultCatalog())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestPortableEffortRequiresAdapterMapping(t *testing.T) {
	s := mixed(t, spec.HarnessProfile{Adapter: "claude-code", Model: &spec.ModelSelection{Connection: "anthropic", ID: "model", Effort: "high"}})
	cat := compile.DefaultCatalog()
	info := cat.Harnesses["claude-code"]
	info.Effort = nil
	cat.Harnesses["claude-code"] = info
	if _, err := compile.Compile(s, cat); err == nil || !strings.Contains(err.Error(), "does not support portable effort") {
		t.Fatalf("expected unsupported effort, got %v", err)
	}
}
