package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/darcys22/steadmesh/harnesses"
)

const CodexACPVersion = "2.5.0"
const CodexRuntimeVersion = "0.166.0"
const CodexACPName = "@agentclientprotocol/codex-acp"

type EnvVariable struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}
type StdioServer struct {
	Name    string        `json:"name"`
	Command string        `json:"command"`
	Args    []string      `json:"args"`
	Env     []EnvVariable `json:"env"`
}
type SessionParams struct {
	CWD        string        `json:"cwd"`
	MCPServers []StdioServer `json:"mcpServers"`
}

var mcpName = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func (p SessionParams) validate() error {
	if !filepath.IsAbs(p.CWD) {
		return errors.New("acp: absolute workspace required")
	}
	if p.MCPServers == nil {
		return errors.New("acp: explicit MCP server list required")
	}
	names := map[string]bool{}
	for _, s := range p.MCPServers {
		if !mcpName.MatchString(s.Name) || names[s.Name] || !filepath.IsAbs(s.Command) {
			return errors.New("acp: invalid or duplicate MCP server")
		}
		names[s.Name] = true
		vars := map[string]bool{}
		for _, e := range s.Env {
			if e.Name == "" || strings.ContainsAny(e.Name, "=\x00") || strings.ContainsRune(e.Value, '\x00') || vars[e.Name] {
				return errors.New("acp: invalid MCP environment")
			}
			vars[e.Name] = true
		}
	}
	return nil
}

// CodexPlan is a deterministic target-specific configuration, not permission
// to launch. The production Adapter remains disabled until its launch/recovery
// and native project-settings boundaries have real target conformance evidence.
// No ambient environment or real model credential is copied into the plan.
type CodexPlan struct {
	Env     []string
	Session SessionParams
	// StartupConfigTOML must be written by the trusted host to a fresh private
	// CODEX_HOME/config.toml before launching the pinned runtime. ACP 2.5.0
	// otherwise applies the provider only to the thread, not the app-server.
	// It contains a placeholder, never a provider credential.
	StartupConfigTOML string
}

// PlanCodex binds the official pinned target to the seat model forwarder and
// platform tool bridge. Unknown settings, remote model endpoints and nonempty
// managed skill sets fail closed rather than being silently dropped.
func PlanCodex(ctx context.Context, env harnesses.Environment, codexPath string) (CodexPlan, error) {
	var plan CodexPlan
	if err := CheckManagedSkills(ctx, env); err != nil {
		return plan, err
	}
	if env.Model == nil || env.Model.ID == "" || env.Model.API != harnesses.APIOpenAIResponses {
		return plan, errors.New("acp: pinned Codex requires a Responses model")
	}
	u, err := url.Parse(env.Model.BaseURL)
	if err != nil || u.Scheme != "http" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return plan, errors.New("acp: local model forwarder required")
	}
	ip := net.ParseIP(u.Hostname())
	if ip == nil || !ip.IsLoopback() || u.Port() == "" {
		return plan, errors.New("acp: numeric loopback forwarder with explicit port required")
	}
	for k := range env.Model.Settings {
		if k != "reasoning_effort" {
			return plan, errors.New("acp: unsupported model setting")
		}
	}
	effort := env.Model.Settings["reasoning_effort"]
	if effort != "" && effort != "none" && effort != "minimal" && effort != "low" && effort != "medium" && effort != "high" && effort != "xhigh" {
		return plan, errors.New("acp: unsupported reasoning effort")
	}
	for _, p := range []string{env.WorkspaceDir, env.HomeDir, env.RunnerDir, env.TokenFile, env.ToolCommand, codexPath} {
		if !filepath.IsAbs(p) {
			return plan, errors.New("acp: absolute runtime paths required")
		}
	}
	config := map[string]any{
		"model": env.Model.ID, "model_provider": "steadmesh", "approval_policy": "on-request", "sandbox_mode": "read-only",
		"check_for_update_on_startup": false, "developer_instructions": harnesses.RenderBootstrap(env, ""),
		"features":  map[string]bool{"plugins": false, "hooks": false},
		"analytics": map[string]bool{"enabled": false}, "feedback": map[string]bool{"enabled": false},
		"model_providers": map[string]any{"steadmesh": map[string]any{
			"name": "Steadmesh model proxy", "base_url": strings.TrimSuffix(env.Model.BaseURL, "/") + "/v1", "wire_api": "responses",
			"experimental_bearer_token": "steadmesh-placeholder", "requires_openai_auth": false,
		}},
	}
	if effort != "" {
		config["model_reasoning_effort"] = effort
	}
	data, err := json.Marshal(config)
	if err != nil {
		return plan, err
	}
	quote := func(s string) string { b, _ := json.Marshal(s); return string(b) }
	plan.StartupConfigTOML = fmt.Sprintf(`model = %s
model_provider = "steadmesh"
approval_policy = "on-request"
sandbox_mode = "read-only"
check_for_update_on_startup = false
[features]
hooks = false
plugins = false
[analytics]
enabled = false
[feedback]
enabled = false
[model_providers.steadmesh]
name = "Steadmesh model proxy"
base_url = %s
wire_api = "responses"
experimental_bearer_token = "steadmesh-placeholder"
requires_openai_auth = false
`, quote(env.Model.ID), quote(strings.TrimSuffix(env.Model.BaseURL, "/")+"/v1"))
	plan.Env = []string{
		"PATH=/usr/local/bin:/usr/bin:/bin", "HOME=" + env.HomeDir, "CODEX_HOME=" + filepath.Join(env.RunnerDir, "acp-codex"),
		"CODEX_PATH=" + codexPath, "CODEX_CONFIG=" + string(data), "MODEL_PROVIDER=steadmesh", "INITIAL_AGENT_MODE=read-only", "NO_BROWSER=1", "LANG=C.UTF-8", "CODEX_ACP_PROMPT_SUGGESTIONS=false",
		`DEFAULT_AUTH_REQUEST={"methodId":"api-key","_meta":{"api-key":{"apiKey":"steadmesh-placeholder"}}}`,
	}
	if env.TmpDir != "" {
		if !filepath.IsAbs(env.TmpDir) {
			return CodexPlan{}, errors.New("acp: absolute temporary path required")
		}
		plan.Env = append(plan.Env, "TMPDIR="+env.TmpDir)
	}
	// Access-plugin egress settings only, never arbitrary overrides to HOME,
	// CODEX_CONFIG, loader paths, provider keys or the selected executable.
	allowedProxy := map[string]bool{"HTTP_PROXY": true, "HTTPS_PROXY": true, "ALL_PROXY": true, "NO_PROXY": true, "http_proxy": true, "https_proxy": true, "all_proxy": true, "no_proxy": true}
	seen := map[string]bool{}
	for _, value := range env.SandboxEnv {
		key, val, ok := strings.Cut(value, "=")
		if !ok || !allowedProxy[key] || seen[key] || strings.ContainsRune(val, '\x00') {
			return CodexPlan{}, errors.New("acp: unsupported sandbox environment override")
		}
		seen[key] = true
		plan.Env = append(plan.Env, value)
	}
	bridgeEnv := map[string]string{"STEADMESH_PLATFORM_URL": env.PlatformURL, "STEADMESH_TOKEN_FILE": env.TokenFile, "STEADMESH_RUNNER_DIR": env.RunnerDir}
	for _, value := range env.ExtraEnv {
		key, val, ok := strings.Cut(value, "=")
		if !ok || !strings.HasPrefix(key, "STEADMESH_") {
			return CodexPlan{}, errors.New("acp: unsupported extra environment override")
		}
		if existing, exists := bridgeEnv[key]; exists && existing != val {
			return CodexPlan{}, errors.New("acp: conflicting bridge environment override")
		}
		bridgeEnv[key] = val
	}
	vars := func(values map[string]string) []EnvVariable {
		keys := make([]string, 0, len(values))
		for k := range values {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out := make([]EnvVariable, 0, len(keys))
		for _, k := range keys {
			out = append(out, EnvVariable{Name: k, Value: values[k]})
		}
		return out
	}
	plan.Session = SessionParams{CWD: env.WorkspaceDir, MCPServers: []StdioServer{{Name: "steadmesh", Command: env.ToolCommand, Args: []string{"mcp"}, Env: vars(bridgeEnv)}}}
	for _, s := range env.MCPServers {
		args := s.Args
		if args == nil {
			args = []string{}
		}
		plan.Session.MCPServers = append(plan.Session.MCPServers, StdioServer{Name: s.Name, Command: s.Command, Args: args, Env: vars(s.Env)})
	}
	if err := plan.Session.validate(); err != nil {
		return CodexPlan{}, err
	}
	// Start platform-authorized bridges with the native process. Required
	// discovery prevents a prompt from racing the ACP session-only MCP reload.
	// Their server enforces seat permissions; native ACP requests still deny.
	for _, server := range plan.Session.MCPServers {
		args, _ := json.Marshal(server.Args)
		plan.StartupConfigTOML += fmt.Sprintf("\n[mcp_servers.%s]\ncommand = %s\nargs = %s\nrequired = true\nstartup_timeout_sec = 30\ntool_timeout_sec = 600\ndefault_tools_approval_mode = \"approve\"\n", quote(server.Name), quote(server.Command), args)
		plan.StartupConfigTOML += fmt.Sprintf("[mcp_servers.%s.env]\n", quote(server.Name))
		for _, variable := range server.Env {
			plan.StartupConfigTOML += fmt.Sprintf("%s = %s\n", quote(variable.Name), quote(variable.Value))
		}
	}
	// Native startup owns the bridge configuration. Do not request duplicate
	// session servers: pinned ACP reports these as failed startup tool updates.
	plan.Session.MCPServers = []StdioServer{}
	return plan, nil
}

// CheckManagedSkills must run at every turn boundary for integrations using
// this limited target. Empty authorization is supported; nonempty skill sets
// need a verified native refresh/revocation bridge, not prompt-only emulation.
func CheckManagedSkills(ctx context.Context, env harnesses.Environment) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if env.LoadSkills == nil {
		return nil
	}
	bundle, err := env.LoadSkills(ctx)
	if err != nil {
		return errors.New("acp: managed skills authorization unavailable")
	}
	if len(bundle.Skills) != 0 {
		return errors.New("acp: managed skill loading is not verified for this target")
	}
	return nil
}

// OpenCodexSession negotiates the pinned protocol and creates a real ACP
// session on a transport already launched with plan.Env by a trusted host.
// It does not authenticate, launch a process, or assert that a caller supplied
// the correct sandbox. Ownership of transport transfers to the returned client.
// Managed-skill authorization is checked before every prompt automatically.
func OpenCodexSession(ctx context.Context, transport io.ReadWriteCloser, env harnesses.Environment, codexPath string) (*Client, error) {
	plan, err := PlanCodex(ctx, env, codexPath)
	if err != nil {
		_ = transport.Close()
		return nil, err
	}
	c := NewClient(transport)
	c.beforePrompt = func(ctx context.Context) error { return CheckManagedSkills(ctx, env) }
	if err := c.Initialize(ctx, CodexACPName, CodexACPVersion); err != nil {
		_ = c.Close()
		return nil, err
	}
	if _, err := c.NewSession(ctx, plan.Session); err != nil {
		_ = c.Close()
		return nil, err
	}
	if err := c.pinCodexSessionTitle(ctx); err != nil {
		_ = c.Close()
		return nil, err
	}
	return c, nil
}

// pinCodexSessionTitle disables the pinned target's automatic title generation.
// ACP 2.5.0 otherwise makes an asynchronous request with a hard-coded model.
// This target-specific extension must succeed before any model prompt.
func (c *Client) pinCodexSessionTitle(ctx context.Context) error {
	if !c.busy.TryLock() {
		return errors.New("acp: concurrent call")
	}
	defer c.busy.Unlock()
	if !c.initialized || c.session == "" {
		return errors.New("acp: no session")
	}
	var result json.RawMessage
	return c.request(ctx, "_session/rename", map[string]string{"sessionId": c.session, "title": "Steadmesh managed session"}, metadataUpdate, &result)
}
