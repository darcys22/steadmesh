package compile_test

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/darcys22/steadmesh/harnesses"
	"github.com/darcys22/steadmesh/pkg/compile"
	"github.com/darcys22/steadmesh/pkg/spec"
)

func startupFallbackSpec(t *testing.T) spec.OrganizationSpec {
	t.Helper()
	image := "example/seat-bundle@sha256:" + strings.Repeat("a", 64)
	s := mixed(t, spec.HarnessProfile{
		Adapter: "claude-code", ImageDigest: image,
		Model:                &spec.ModelSelection{Connection: "anthropic", ID: "claude-primary"},
		RequiredCapabilities: []string{"tools", "mcp"},
		StartupFallback:      &spec.StartupFallbackPolicy{Profiles: []string{"secondary", "third"}, MaxAttempts: 3, TotalTimeout: "30s"},
	})
	s.HarnessProfiles["secondary"] = spec.HarnessProfile{
		Adapter: "codex", ImageDigest: image,
		Model:  &spec.ModelSelection{Connection: "openai", ID: "gpt-secondary", Settings: map[string]string{"reasoning_effort": "high"}},
		Config: map[string]string{"binary": "codex"}, RequiredCapabilities: []string{"interrupt"},
	}
	s.HarnessProfiles["third"] = spec.HarnessProfile{
		Adapter: "pi", ImageDigest: image,
		Model: &spec.ModelSelection{Connection: "selfhosted", ID: "qwen3.8-27b"},
	}
	return s
}

func mutateHarness(s *spec.OrganizationSpec, key string, f func(*spec.HarnessProfile)) {
	h := s.HarnessProfiles[key]
	f(&h)
	s.HarnessProfiles[key] = h
}

func TestStartupFallbackResolution(t *testing.T) {
	s := startupFallbackSpec(t)
	m, err := compile.Compile(s, compile.DefaultCatalog())
	if err != nil {
		t.Fatal(err)
	}
	sm := m.Seats["reviewer"]
	if len(sm.StartupFallbacks) != 2 || sm.StartupFallbacks[0].Profile != "secondary" || sm.StartupFallbacks[1].Profile != "third" {
		t.Fatalf("wrong ordered alternatives: %+v", sm.StartupFallbacks)
	}
	candidate := sm.StartupFallbacks[0].Harness
	if candidate.Model.API != harnesses.APIOpenAIResponses || candidate.StartupFallback != nil || !slices.Equal(candidate.RequiredCapabilities, []string{"interrupt", "mcp", "tools"}) {
		t.Fatalf("candidate not fully resolved: %+v", candidate)
	}
	if s.HarnessProfiles["secondary"].Model.API != "" || !slices.Equal(s.HarnessProfiles["secondary"].RequiredCapabilities, []string{"interrupt"}) {
		t.Fatal("compilation mutated input candidate")
	}
	for _, conn := range []string{"anthropic", "openai", "selfhosted"} {
		if !slices.ContainsFunc(sm.Capabilities, func(c compile.Capability) bool {
			return c.Resource == "connection:"+conn && slices.Equal(c.Operations, []string{"model.infer"})
		}) {
			t.Errorf("missing exact implicit model grant on %s", conn)
		}
	}
	uses := compile.ModelUses(m)
	if !slices.Equal(uses["openai"], []compile.ModelUse{{ID: "gpt-secondary", API: harnesses.APIOpenAIResponses}}) || !slices.Equal(uses["selfhosted"], []compile.ModelUse{{ID: "qwen3.8-27b", API: harnesses.APIOpenAIChat}}) {
		t.Fatalf("readiness lacks candidates: %+v", uses)
	}
	required := compile.RequiredConnections(m, compile.DefaultCatalog())
	if !required["anthropic"] || required["openai"] || required["selfhosted"] {
		t.Fatalf("optional alternatives must not block healthy primary: %+v", required)
	}
	data, err := json.Marshal(sm)
	if err != nil {
		t.Fatal(err)
	}
	var back compile.SeatManifest
	if err := json.Unmarshal(data, &back); err != nil || !reflect.DeepEqual(sm, back) {
		t.Fatalf("manifest roundtrip: %v", err)
	}
	mutateHarness(&s, "secondary", func(h *spec.HarnessProfile) { h.Model.ID = "updated" })
	changed, err := compile.Compile(s, compile.DefaultCatalog())
	if err != nil {
		t.Fatal(err)
	}
	if changed.Seats["reviewer"].ConfigRevision == sm.ConfigRevision || changed.Digest == m.Digest {
		t.Fatal("candidate changes did not change committed revision")
	}
}

func TestStartupFallbackRejected(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		mut        func(*spec.OrganizationSpec)
	}{
		{"missing alternatives", "1 to 7", func(s *spec.OrganizationSpec) { s.HarnessProfiles["h"].StartupFallback.Profiles = nil }},
		{"too many alternatives", "1 to 7", func(s *spec.OrganizationSpec) {
			s.HarnessProfiles["h"].StartupFallback.Profiles = []string{"a", "b", "c", "d", "e", "f", "g", "i"}
		}},
		{"unknown profile", "unknown harness profile", func(s *spec.OrganizationSpec) { s.HarnessProfiles["h"].StartupFallback.Profiles[0] = "missing" }},
		{"self", "self-referencing", func(s *spec.OrganizationSpec) { s.HarnessProfiles["h"].StartupFallback.Profiles[0] = "h" }},
		{"duplicate", "duplicate", func(s *spec.OrganizationSpec) { s.HarnessProfiles["h"].StartupFallback.Profiles[1] = "secondary" }},
		{"nested", "nested startup fallback", func(s *spec.OrganizationSpec) {
			mutateHarness(s, "secondary", func(h *spec.HarnessProfile) {
				h.StartupFallback = &spec.StartupFallbackPolicy{Profiles: []string{"third"}, MaxAttempts: 2, TotalTimeout: "30s"}
			})
		}},
		{"cycle", "nested startup fallback", func(s *spec.OrganizationSpec) {
			mutateHarness(s, "secondary", func(h *spec.HarnessProfile) {
				h.StartupFallback = &spec.StartupFallbackPolicy{Profiles: []string{"h"}, MaxAttempts: 2, TotalTimeout: "30s"}
			})
		}},
		{"different image", "same configured image_digest", func(s *spec.OrganizationSpec) {
			mutateHarness(s, "secondary", func(h *spec.HarnessProfile) { h.ImageDigest = "another:dev" })
		}},
		{"empty image", "same configured image_digest", func(s *spec.OrganizationSpec) {
			mutateHarness(s, "h", func(h *spec.HarnessProfile) { h.ImageDigest = "" })
		}},
		{"capability downgrade", "cannot preserve primary required capability", func(s *spec.OrganizationSpec) {
			mutateHarness(s, "secondary", func(h *spec.HarnessProfile) { h.Adapter = "fake"; h.Model = nil })
		}},
		{"bad candidate capability", "does not support capability", func(s *spec.OrganizationSpec) {
			mutateHarness(s, "secondary", func(h *spec.HarnessProfile) { h.RequiredCapabilities = []string{"unknown"} })
		}},
		{"bad candidate model", "not one of connection", func(s *spec.OrganizationSpec) { s.HarnessProfiles["third"].Model.ID = "unknown" }},
		{"bad candidate API", "does not speak", func(s *spec.OrganizationSpec) { s.HarnessProfiles["secondary"].Model.API = harnesses.APIOpenAIChat }},
		{"bad candidate setting", "has no setting", func(s *spec.OrganizationSpec) { s.HarnessProfiles["secondary"].Model.Settings["unknown"] = "bad" }},
		{"zero attempts", "max_attempts", func(s *spec.OrganizationSpec) { s.HarnessProfiles["h"].StartupFallback.MaxAttempts = 0 }},
		{"negative attempts", "max_attempts", func(s *spec.OrganizationSpec) { s.HarnessProfiles["h"].StartupFallback.MaxAttempts = -1 }},
		{"exceeds chain length", "max_attempts", func(s *spec.OrganizationSpec) { s.HarnessProfiles["h"].StartupFallback.MaxAttempts = 4 }},
		{"exceeds hard bound", "max_attempts", func(s *spec.OrganizationSpec) { s.HarnessProfiles["h"].StartupFallback.MaxAttempts = 9 }},
		{"missing timeout", "total_timeout", func(s *spec.OrganizationSpec) { s.HarnessProfiles["h"].StartupFallback.TotalTimeout = "" }},
		{"invalid timeout", "total_timeout", func(s *spec.OrganizationSpec) { s.HarnessProfiles["h"].StartupFallback.TotalTimeout = "forever" }},
		{"zero timeout", "total_timeout", func(s *spec.OrganizationSpec) { s.HarnessProfiles["h"].StartupFallback.TotalTimeout = "0s" }},
		{"negative timeout", "total_timeout", func(s *spec.OrganizationSpec) { s.HarnessProfiles["h"].StartupFallback.TotalTimeout = "-1s" }},
		{"unbounded timeout", "total_timeout", func(s *spec.OrganizationSpec) { s.HarnessProfiles["h"].StartupFallback.TotalTimeout = "5m1ns" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := startupFallbackSpec(t)
			tc.mut(&s)
			m, err := compile.Compile(s, compile.DefaultCatalog())
			if err == nil || m != nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected %q, got manifest=%v error=%v", tc.want, m, err)
			}
		})
	}
}

func TestStartupFallbackBounds(t *testing.T) {
	s := startupFallbackSpec(t)
	p := s.HarnessProfiles["h"].StartupFallback
	p.Profiles = nil
	for _, key := range []string{"a", "b", "c", "d", "e", "f", "g"} {
		s.HarnessProfiles[key] = s.HarnessProfiles["secondary"]
		p.Profiles = append(p.Profiles, key)
	}
	p.MaxAttempts, p.TotalTimeout = 8, "5m"
	if _, err := compile.Compile(s, compile.DefaultCatalog()); err != nil {
		t.Fatal(err)
	}
	p.MaxAttempts, p.TotalTimeout = 1, "1ns"
	if _, err := compile.Compile(s, compile.DefaultCatalog()); err != nil {
		t.Fatal(err)
	}
}

func TestStartupCandidateBinding(t *testing.T) {
	h := startupFallbackSpec(t).HarnessProfiles["h"]
	binding := compile.StartupCandidateBinding("h", h)
	if len(binding) != 64 {
		t.Fatalf("binding = %q", binding)
	}
	h.StartupFallback = nil
	if got := compile.StartupCandidateBinding("h", h); got != binding {
		t.Fatal("chain policy changed native route binding")
	}
	if compile.StartupCandidateBinding("other", h) == binding {
		t.Fatal("profile identity omitted")
	}
	for _, mut := range []func(*spec.HarnessProfile){
		func(h *spec.HarnessProfile) { h.ImageDigest += "changed" },
		func(h *spec.HarnessProfile) { h.Adapter = "pi" },
		func(h *spec.HarnessProfile) { h.Model.ID = "changed" },
		func(h *spec.HarnessProfile) { h.Model.Connection = "changed" },
		func(h *spec.HarnessProfile) { h.Model.API = "changed" },
		func(h *spec.HarnessProfile) { h.Model.Settings = map[string]string{"effort": "low"} },
		func(h *spec.HarnessProfile) { h.Config = map[string]string{"binary": "changed"} },
		func(h *spec.HarnessProfile) { h.RequiredCapabilities = []string{"tools"} },
	} {
		changed := h.DeepCopy()
		mut(changed)
		if compile.StartupCandidateBinding("h", *changed) == binding {
			t.Fatal("route configuration omitted from binding")
		}
	}
}
