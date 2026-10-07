package compile_test

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/darcys22/steadmesh/harnesses"
	"github.com/darcys22/steadmesh/pkg/compile"
	"github.com/darcys22/steadmesh/pkg/spec"
)

// mixed returns the valid fixture with three model connections and a
// reviewer seat on harness profile "h".
func mixed(t *testing.T, h spec.HarnessProfile) spec.OrganizationSpec {
	t.Helper()
	s, _ := load(t, filepath.Join(fixtures, "valid", "representative_and_worker.yaml"))
	s.Connections["anthropic"] = spec.Connection{Adapter: "anthropic", SecretRef: "k8s:anthropic"}
	s.Connections["openai"] = spec.Connection{Adapter: "openai", SecretRef: "k8s:openai"}
	s.Connections["i14"] = spec.Connection{Adapter: "model", EndpointRef: "https://api-dev.i14.ai/v1", SecretRef: "k8s:i14",
		Model: &spec.ModelEndpoint{APIs: []string{harnesses.APIOpenAIChat}, Models: []spec.ModelEntry{{ID: "qwen3.8-27b"}}}}
	if h.ImageDigest == "" {
		h.ImageDigest = "seat:dev"
	}
	s.HarnessProfiles["h"] = h
	r := s.Seats["reviewer"]
	r.HarnessProfile = "h"
	s.Seats["reviewer"] = r
	return s
}

func TestModelSelectionResolvesAPI(t *testing.T) {
	for _, tc := range []struct {
		name    string
		adapter string
		model   spec.ModelSelection
		wantAPI string
	}{
		{"claude on anthropic", "claude-code", spec.ModelSelection{Connection: "anthropic", ID: "claude-sonnet-4-5"}, harnesses.APIAnthropicMessages},
		{"codex on openai", "codex", spec.ModelSelection{Connection: "openai", ID: "gpt-5.5", Settings: map[string]string{"reasoning_effort": "high"}}, harnesses.APIOpenAIResponses},
		{"pi on an openai-chat-only endpoint", "pi", spec.ModelSelection{Connection: "i14", ID: "qwen3.8-27b"}, harnesses.APIOpenAIChat},
		{"pi on openai prefers responses", "pi", spec.ModelSelection{Connection: "openai", ID: "gpt-5.5"}, harnesses.APIOpenAIResponses},
		{"pi with an explicit api", "pi", spec.ModelSelection{Connection: "openai", ID: "gpt-5.5", API: harnesses.APIOpenAIChat}, harnesses.APIOpenAIChat},
		{"pi on anthropic", "pi", spec.ModelSelection{Connection: "anthropic", ID: "claude-x", Settings: map[string]string{"thinking": "high"}}, harnesses.APIAnthropicMessages},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sel := tc.model
			m, err := compile.Compile(mixed(t, spec.HarnessProfile{Adapter: tc.adapter, Model: &sel}), compile.DefaultCatalog())
			if err != nil {
				t.Fatal(err)
			}
			got := m.Seats["reviewer"].Harness.Model
			if got == nil || got.API != tc.wantAPI || got.ID != tc.model.ID {
				t.Fatalf("resolved model %+v, want api %s", got, tc.wantAPI)
			}
			if !slices.ContainsFunc(m.Seats["reviewer"].Capabilities, func(c compile.Capability) bool {
				return c.Resource == "connection:"+tc.model.Connection && slices.Contains(c.Operations, "model.infer")
			}) {
				t.Fatal("model.infer not granted implicitly")
			}
			if !*m.Spec.Connections[tc.model.Connection].Required {
				t.Fatal("a model connection a seat uses must be required")
			}
			uses := compile.ModelUses(m)[tc.model.Connection]
			if len(uses) != 1 || uses[0] != (compile.ModelUse{ID: tc.model.ID, API: tc.wantAPI}) {
				t.Fatalf("model uses %+v", uses)
			}
		})
	}
}

func TestModelConnectionDefaults(t *testing.T) {
	m, err := compile.Compile(mixed(t, spec.HarnessProfile{Adapter: "fake"}), compile.DefaultCatalog())
	if err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]spec.ModelEndpoint{
		"anthropic": {APIs: []string{harnesses.APIAnthropicMessages}, Auth: "x-api-key", Verify: "request"},
		"openai":    {APIs: []string{harnesses.APIOpenAIResponses, harnesses.APIOpenAIChat}, Auth: "bearer", Verify: "request"},
		"i14":       {APIs: []string{harnesses.APIOpenAIChat}, Auth: "bearer", Verify: "request", Models: []spec.ModelEntry{{ID: "qwen3.8-27b"}}},
	} {
		got := m.Spec.Connections[k].Model
		if got == nil || !slices.Equal(got.APIs, want.APIs) || got.Auth != want.Auth || got.Verify != want.Verify || len(got.Models) != len(want.Models) {
			t.Errorf("%s: %+v, want %+v", k, got, want)
		}
		if *m.Spec.Connections[k].Required {
			t.Errorf("%s: an unused model connection must not be required", k)
		}
	}
}

func TestModelSelectionRejected(t *testing.T) {
	for _, tc := range []struct {
		name string
		h    spec.HarnessProfile
		mut  func(*spec.OrganizationSpec)
		want []string
	}{
		{"codex cannot speak chat", spec.HarnessProfile{Adapter: "codex", Model: &spec.ModelSelection{Connection: "i14", ID: "qwen3.8-27b"}},
			nil, []string{`harness "codex" speaks openai_responses`, `connection "i14" serves openai_chat`, "harnesses that would work: pi"}},
		{"claude cannot speak openai", spec.HarnessProfile{Adapter: "claude-code", Model: &spec.ModelSelection{Connection: "openai", ID: "gpt-5.5"}},
			nil, []string{`harness "claude-code" speaks anthropic_messages`, "harnesses that would work: codex, pi"}},
		{"explicit api the harness lacks", spec.HarnessProfile{Adapter: "codex", Model: &spec.ModelSelection{Connection: "openai", ID: "gpt-5.5", API: harnesses.APIOpenAIChat}},
			nil, []string{"harness_profiles.h.model.api", `harness "codex" does not speak openai_chat`}},
		{"explicit api the connection lacks", spec.HarnessProfile{Adapter: "pi", Model: &spec.ModelSelection{Connection: "i14", ID: "qwen3.8-27b", API: harnesses.APIOpenAIResponses}},
			nil, []string{`connection "i14" does not serve openai_responses`}},
		{"undeclared model", spec.HarnessProfile{Adapter: "pi", Model: &spec.ModelSelection{Connection: "i14", ID: "llama"}},
			nil, []string{`model "llama" is not one of connection "i14"'s models (qwen3.8-27b)`}},
		{"missing model", spec.HarnessProfile{Adapter: "codex"}, nil, []string{"harness_profiles.h.model", `adapter "codex" requires a model`}},
		{"not a model connection", spec.HarnessProfile{Adapter: "pi", Model: &spec.ModelSelection{Connection: "slack", ID: "x"}},
			nil, []string{`connection "slack" uses adapter "slack", which is not a model adapter`}},
		{"unknown setting", spec.HarnessProfile{Adapter: "codex", Model: &spec.ModelSelection{Connection: "openai", ID: "gpt-5.5", Settings: map[string]string{"temperature": "1"}}},
			nil, []string{`harness "codex" has no setting "temperature" (settings: reasoning_effort)`}},
		{"bad setting value", spec.HarnessProfile{Adapter: "claude-code", Model: &spec.ModelSelection{Connection: "anthropic", ID: "c", Settings: map[string]string{"effort": "extreme"}}},
			nil, []string{"settings.effort", "must be one of low, medium, high, xhigh, max"}},
		{"model adapter without apis or endpoint", spec.HarnessProfile{Adapter: "fake"},
			func(s *spec.OrganizationSpec) {
				s.Connections["bare"] = spec.Connection{Adapter: "model", SecretRef: "k8s:x"}
			},
			[]string{"connections.bare.endpoint_ref", "connections.bare.model.apis"}},
		{"bad auth and verify", spec.HarnessProfile{Adapter: "fake"},
			func(s *spec.OrganizationSpec) {
				c := s.Connections["openai"]
				c.Model = &spec.ModelEndpoint{Auth: "basic", Verify: "always"}
				s.Connections["openai"] = c
			}, []string{"connections.openai.model.auth", "connections.openai.model.verify"}},
		{"model api outside the connection's", spec.HarnessProfile{Adapter: "fake"},
			func(s *spec.OrganizationSpec) {
				c := s.Connections["i14"]
				c.Model.Models = []spec.ModelEntry{{ID: "a", APIs: []string{harnesses.APIAnthropicMessages}}, {ID: "a"}}
				s.Connections["i14"] = c
			}, []string{"anthropic_messages is not one of the connection's APIs", `duplicate model "a"`}},
		{"model block on a tracker", spec.HarnessProfile{Adapter: "fake"},
			func(s *spec.OrganizationSpec) {
				c := s.Connections["tracker"]
				c.Model = &spec.ModelEndpoint{}
				s.Connections["tracker"] = c
			}, []string{"connections.tracker.model", "only valid on model connections"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := mixed(t, tc.h)
			if tc.mut != nil {
				tc.mut(&s)
			}
			_, err := compile.Compile(s, compile.DefaultCatalog())
			if err == nil {
				t.Fatal("accepted")
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error lacks %q:\n%v", w, err)
				}
			}
		})
	}
}

func TestCatalogListsRegisteredHarnesses(t *testing.T) {
	cat := compile.DefaultCatalog()
	for _, name := range []string{"claude-code", "claude", "codex", "pi", "fake"} {
		if _, ok := cat.Harnesses[name]; !ok {
			t.Errorf("catalog lacks %s", name)
		}
	}
	if !slices.Equal(cat.Harnesses["pi"].APIs, []string{harnesses.APIOpenAIResponses, harnesses.APIOpenAIChat, harnesses.APIAnthropicMessages}) {
		t.Errorf("pi APIs %v", cat.Harnesses["pi"].APIs)
	}
}
