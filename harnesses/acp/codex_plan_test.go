package acp

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/darcys22/steadmesh/harnesses"
	"github.com/darcys22/steadmesh/pkg/runtimeapi"
)

func planEnv() harnesses.Environment {
	return harnesses.Environment{WorkspaceDir: "/seat/workspace", HomeDir: "/seat/home", RunnerDir: "/seat/runner", TokenFile: "/seat/token", ToolCommand: "/bin/steadmesh-tools", PlatformURL: "http://platform", Model: &harnesses.ModelEndpoint{ID: "test-model", API: harnesses.APIOpenAIResponses, BaseURL: "http://127.0.0.1:5555", APIKey: "must-not-copy"}}
}
func TestPlanUsesOnlyForwarderAndPlaceholder(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "ambient-secret")
	env := planEnv()
	env.SandboxEnv = []string{"HTTPS_PROXY=http://gateway:8080"}
	plan, err := PlanCodex(context.Background(), env, "/opt/codex")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(plan.Env, "\n") + "\n" + plan.StartupConfigTOML
	for _, secret := range []string{"ambient-secret", "must-not-copy"} {
		if strings.Contains(joined, secret) {
			t.Fatalf("credential copied: %s", secret)
		}
	}
	for _, expected := range []string{"http://127.0.0.1:5555/v1", "steadmesh-placeholder", "INITIAL_AGENT_MODE=read-only", "HTTPS_PROXY=http://gateway:8080"} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("missing boundary: %s", expected)
		}
	}
	if len(plan.Session.MCPServers) != 1 || plan.Session.MCPServers[0].Command != env.ToolCommand {
		t.Fatal("missing platform bridge")
	}
}
func TestPlanRejectsBoundaryOverrides(t *testing.T) {
	tests := map[string]func(*harnesses.Environment){
		"remote endpoint": func(e *harnesses.Environment) { e.Model.BaseURL = "https://api.openai.com" },
		"localhost DNS":   func(e *harnesses.Environment) { e.Model.BaseURL = "http://localhost:5555" },
		"provider key":    func(e *harnesses.Environment) { e.SandboxEnv = []string{"OPENAI_API_KEY=key"} },
		"path override":   func(e *harnesses.Environment) { e.SandboxEnv = []string{"PATH=/evil"} },
		"model setting":   func(e *harnesses.Environment) { e.Model.Settings = map[string]string{"unknown": "true"} },
		"bridge override": func(e *harnesses.Environment) { e.ExtraEnv = []string{"STEADMESH_TOKEN_FILE=/other"} },
		"duplicate MCP": func(e *harnesses.Environment) {
			e.MCPServers = []harnesses.MCPServer{{Name: "steadmesh", Command: "/evil"}}
		},
		"relative MCP": func(e *harnesses.Environment) {
			e.MCPServers = []harnesses.MCPServer{{Name: "other", Command: "relative"}}
		},
	}
	for name, change := range tests {
		t.Run(name, func(t *testing.T) {
			e := planEnv()
			change(&e)
			if _, err := PlanCodex(context.Background(), e, "/opt/codex"); err == nil {
				t.Fatal("accepted unsafe plan")
			}
		})
	}
}
func TestManagedSkillsAreRechecked(t *testing.T) {
	e := planEnv()
	calls := 0
	e.LoadSkills = func(context.Context) (runtimeapi.SkillsBundleResponse, error) {
		calls++
		if calls == 1 {
			return runtimeapi.SkillsBundleResponse{}, nil
		}
		return runtimeapi.SkillsBundleResponse{}, errors.New("unavailable secret detail")
	}
	if _, err := PlanCodex(context.Background(), e, "/opt/codex"); err != nil {
		t.Fatal(err)
	}
	if err := CheckManagedSkills(context.Background(), e); err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("revocation refresh: %v", err)
	}
	e.LoadSkills = func(context.Context) (runtimeapi.SkillsBundleResponse, error) {
		return runtimeapi.SkillsBundleResponse{Skills: []runtimeapi.SkillBundle{{}}}, nil
	}
	if err := CheckManagedSkills(context.Background(), e); err == nil {
		t.Fatal("unsupported skill set accepted")
	}
}

func TestPlanPinsProviderAtNativeStartup(t *testing.T) {
	p, err := PlanCodex(context.Background(), planEnv(), "/opt/codex")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`model_provider = "steadmesh"`, `[model_providers.steadmesh]`, `base_url = "http://127.0.0.1:5555/v1"`, `sandbox_mode = "read-only"`, `approval_policy = "on-request"`, `hooks = false`, `plugins = false`, `[mcp_servers."steadmesh"]`, `required = true`, `default_tools_approval_mode = "approve"`} {
		if !strings.Contains(p.StartupConfigTOML, want) {
			t.Fatalf("missing startup boundary %s", want)
		}
	}
}
