package codex

import (
	"strings"
	"testing"

	"github.com/darcys22/steadmesh/harnesses"
)

func TestConfigTOML(t *testing.T) {
	env := harnesses.Environment{PlatformURL: "http://p", TokenFile: "/var/run/steadmesh/token", RunnerDir: "/seat/runner",
		ToolCommand: "/usr/local/bin/steadmesh-tools", ExtraEnv: []string{"STEADMESH_SEAT_KEY=eng", "OPENAI_API_KEY=leak"},
		Model: &harnesses.ModelEndpoint{ID: "gpt-5.5", API: harnesses.APIOpenAIResponses, BaseURL: "http://127.0.0.1:4000/model/openai", APIKey: "steadmesh-local"}}
	got := ConfigTOML(env)
	for _, want := range []string{
		`model = "gpt-5.5"`, `model_provider = "steadmesh"`, `approval_policy = "never"`, `sandbox_mode = "danger-full-access"`,
		"[features]\nplugins = false", "[analytics]\nenabled = false",
		"[model_providers.steadmesh]", `base_url = "http://127.0.0.1:4000/model/openai/v1"`, `wire_api = "responses"`,
		`experimental_bearer_token = "steadmesh-local"`,
		"[mcp_servers.steadmesh]", `command = "/usr/local/bin/steadmesh-tools"`, `args = ["mcp"]`, `required = true`,
		`default_tools_approval_mode = "approve"`, `"STEADMESH_SEAT_KEY" = "eng"`, `"STEADMESH_TOKEN_FILE" = "/var/run/steadmesh/token"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("config.toml lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "leak") {
		t.Fatal("a non-steadmesh variable reached config.toml")
	}
	if strings.Contains(strings.Join(baseEnv(env, "/h"), " "), "OPENAI_API_KEY") {
		t.Fatal("OPENAI_API_KEY reached the codex environment")
	}
}

func TestPrepareRejectsOtherAPIs(t *testing.T) {
	env := harnesses.Environment{Model: &harnesses.ModelEndpoint{ID: "m", API: harnesses.APIOpenAIChat, BaseURL: "http://x"}}
	if err := New().Prepare(t.Context(), env); err == nil || !strings.Contains(err.Error(), "speaks only openai_responses") {
		t.Fatalf("got %v", err)
	}
}
