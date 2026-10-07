package pi

import (
	"encoding/json"
	"testing"

	"github.com/darcys22/steadmesh/harnesses"
)

func TestModelsJSON(t *testing.T) {
	for api, want := range map[string]struct{ base, piAPI string }{
		harnesses.APIAnthropicMessages: {"http://127.0.0.1:4000/model/m", "anthropic-messages"},
		harnesses.APIOpenAIResponses:   {"http://127.0.0.1:4000/model/m/v1", "openai-responses"},
		harnesses.APIOpenAIChat:        {"http://127.0.0.1:4000/model/m/v1", "openai-completions"},
	} {
		env := harnesses.Environment{Model: &harnesses.ModelEndpoint{ID: "qwen3.8-27b", API: api, BaseURL: "http://127.0.0.1:4000/model/m", APIKey: "steadmesh-local",
			Settings: map[string]string{SettingContextWindow: "64000", SettingReasoning: "true"}}}
		b, err := ModelsJSON(env)
		if err != nil {
			t.Fatal(err)
		}
		var got struct {
			Providers map[string]struct {
				BaseURL string           `json:"baseUrl"`
				API     string           `json:"api"`
				APIKey  string           `json:"apiKey"`
				Models  []map[string]any `json:"models"`
			} `json:"providers"`
		}
		_ = json.Unmarshal(b, &got)
		p := got.Providers[ProviderID]
		if p.BaseURL != want.base || p.API != want.piAPI || p.APIKey != "steadmesh-local" || len(p.Models) != 1 ||
			p.Models[0]["id"] != "qwen3.8-27b" || p.Models[0]["contextWindow"] != float64(64000) || p.Models[0]["reasoning"] != true {
			t.Errorf("%s: %s", api, b)
		}
	}
	bad := harnesses.Environment{Model: &harnesses.ModelEndpoint{ID: "m", API: harnesses.APIOpenAIChat, Settings: map[string]string{SettingMaxTokens: "lots"}}}
	if _, err := ModelsJSON(bad); err == nil {
		t.Fatal("non-numeric max_tokens accepted")
	}
}

func TestMCPJSONIsDirect(t *testing.T) {
	var got struct {
		MCPServers map[string]map[string]any `json:"mcpServers"`
	}
	_ = json.Unmarshal(MCPJSON(harnesses.Environment{ToolCommand: "/t", ExtraEnv: []string{"STEADMESH_SEAT_KEY=a", "HOME=/x"}}), &got)
	s := got.MCPServers[MCPServerName]
	env, _ := s["env"].(map[string]any)
	if s["exposure"] != "direct" || s["command"] != "/t" || env["STEADMESH_SEAT_KEY"] != "a" || env["HOME"] != nil {
		t.Fatalf("mcp.json %v", got)
	}
}
