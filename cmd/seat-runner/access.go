package main

import (
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"

	"github.com/darcys22/steadmesh/harnesses"
	"github.com/darcys22/steadmesh/pkg/access"
	"github.com/darcys22/steadmesh/pkg/egressforward"
)

// PlaywrightMCP is the browser MCP server binary (@playwright/mcp).
const PlaywrightMCP = "playwright-mcp"

// requiredBinaries are the binaries the seat's access needs in the image.
func requiredBinaries(a *access.SeatAccess, browserExecutable string) []string {
	if a == nil {
		return nil
	}
	need := append([]string(nil), a.Binaries...)
	if len(a.GitHub) > 0 {
		for _, g := range a.GitHub {
			if g.Delivery == access.DeliverySandbox {
				need = append(need, "git")
				break
			}
		}
	}
	if a.Browser != nil {
		need = append(need, PlaywrightMCP, browserExecutable)
	}
	return need
}

// missingBinaries lists required binaries that are not on PATH.
func missingBinaries(need []string) []string {
	var missing []string
	seen := map[string]bool{}
	for _, b := range need {
		if seen[b] {
			continue
		}
		seen[b] = true
		if _, err := exec.LookPath(b); err != nil {
			missing = append(missing, b)
		}
	}
	return missing
}

// setupAccess applies the seat's access before the harness starts: it
// refuses to start when a required binary is missing (so readiness never
// claims a tool the image lacks), starts the local egress proxy, configures
// git's credential helper, and adds the browser MCP server.
func (r *Runner) setupAccess(a *access.SeatAccess, display string) error {
	if missing := missingBinaries(requiredBinaries(a, r.cfg.BrowserExecutable)); len(missing) > 0 {
		return fmt.Errorf("access profiles %v need %s, which this seat image does not provide; use an image that has them (docs/sandbox.html)",
			a.Profiles, strings.Join(missing, ", "))
	}
	var env []string
	if a.UsesGateway() {
		if r.cfg.EgressURL == "" {
			return fmt.Errorf("access profiles %v need the egress gateway, but STEADMESH_EGRESS_URL is not set", a.Profiles)
		}
		if r.efwd == nil {
			f, err := egressforward.Start(egressforward.Options{GatewayURL: r.cfg.EgressURL, TokenFile: r.cfg.TokenFile,
				Generation: r.gen.Load, Execution: r.currentExecution, Log: r.log})
			if err != nil {
				return err
			}
			r.efwd = f
		}
		direct := []string{}
		if u, err := url.Parse(r.cfg.PlatformURL); err == nil && u.Hostname() != "" {
			direct = append(direct, u.Hostname())
		}
		env = append(env, r.efwd.Env(direct...)...)
	}
	if sandboxGit(a) {
		env = append(env, gitConfigEnv(map[string][]string{
			// The empty value resets helpers configured elsewhere.
			"credential.helper": {"", r.cfg.ToolCommand + " credential git"},
			"user.name":         {firstNonEmpty(display, r.cfg.SeatKey)},
			"user.email":        {r.cfg.SeatKey + "@seats.steadmesh.invalid"},
		})...)
	}
	r.mu.Lock()
	r.sandboxEnv = env
	r.mcpServers = nil
	if a != nil && a.Browser != nil {
		e := map[string]string{}
		for _, kv := range append(r.toolEnv(), env...) {
			if k, v, ok := strings.Cut(kv, "="); ok {
				e[k] = v
			}
		}
		e["STEADMESH_BROWSER_EXECUTABLE"] = r.cfg.BrowserExecutable
		r.mcpServers = []harnesses.MCPServer{{Name: "browser", Command: r.cfg.ToolCommand, Args: []string{"browser-mcp"}, Env: e}}
	}
	r.mu.Unlock()
	return nil
}

func sandboxGit(a *access.SeatAccess) bool {
	if a == nil {
		return false
	}
	for _, g := range a.GitHub {
		if g.Delivery == access.DeliverySandbox {
			return true
		}
	}
	return false
}

// gitConfigEnv expresses git configuration as GIT_CONFIG_* variables, so no
// file in the seat's home is touched. Keys are sorted for stable output.
func gitConfigEnv(cfg map[string][]string) []string {
	keys := make([]string, 0, len(cfg))
	for k := range cfg {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []string
	n := 0
	for _, k := range keys {
		for _, v := range cfg[k] {
			out = append(out, "GIT_CONFIG_KEY_"+strconv.Itoa(n)+"="+k, "GIT_CONFIG_VALUE_"+strconv.Itoa(n)+"="+v)
			n++
		}
	}
	return append(out, "GIT_CONFIG_COUNT="+strconv.Itoa(n))
}

// browserExecutable is the Chromium binary the browser plugin uses.
func browserExecutable() string {
	if v := os.Getenv("STEADMESH_BROWSER_EXECUTABLE"); v != "" {
		return v
	}
	return "/usr/bin/chromium"
}
