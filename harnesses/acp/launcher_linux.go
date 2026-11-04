//go:build linux

package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/darcys22/steadmesh/harnesses"
)

// ExperimentalLauncher runs one pinned session, never a registered harness.
// The host must additionally provide an isolated container with no credentials,
// controlled system configuration/skills, and the normal model/tool boundaries.
// Ownership checks do not replace those unverified production requirements.
type ExperimentalLauncher struct {
	NodePath, ACPPath, CodexPath string
	NeutralDir                   string
	// PrivateParent must be an existing owner-only directory outside WorkspaceDir.
	PrivateParent string
}

// Run performs exactly one prompt and tears down the owned process group.
// It does not promise native recovery, managed skills, or workspace cwd semantics.
func (l ExperimentalLauncher) Run(ctx context.Context, env harnesses.Environment, prompt string, emit func(Observation)) (Observation, error) {
	var zero Observation
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	if len(env.MCPServers) != 0 {
		return zero, errors.New("acp: launcher supports only the platform bridge")
	}
	if err := CheckManagedSkills(ctx, env); err != nil {
		return zero, err
	}
	if os.Geteuid() == 0 {
		return zero, errors.New("acp: launcher requires a non-root runtime")
	}
	for _, p := range []string{l.NodePath, l.ACPPath, l.CodexPath, env.ToolCommand} {
		if err := immutablePath(p, false); err != nil {
			return zero, err
		}
	}
	if err := immutablePath(l.NeutralDir, true); err != nil {
		return zero, err
	}
	entries, err := os.ReadDir(l.NeutralDir)
	if err != nil {
		return zero, err
	}
	if len(entries) != 0 {
		return zero, errors.New("acp: neutral directory must be empty")
	}
	// Ancestor instructions/configuration would make an otherwise empty cwd non-neutral.
	for p := l.NeutralDir; ; p = filepath.Dir(p) {
		for _, name := range []string{".codex", ".agents", "AGENTS.md", "AGENTS.override.md"} {
			if _, err := os.Lstat(filepath.Join(p, name)); !os.IsNotExist(err) {
				return zero, errors.New("acp: neutral ancestor contains configuration or cannot be inspected")
			}
		}
		if p == "/" {
			break
		}
	}
	if err := privateParent(l.PrivateParent, env.WorkspaceDir); err != nil {
		return zero, err
	}
	root, err := os.MkdirTemp(l.PrivateParent, "acp-turn-")
	if err != nil {
		return zero, err
	}
	// This tree was freshly created by this invocation, never supplied by a caller.
	cleanupAllowed := true
	defer func() {
		if cleanupAllowed {
			_ = os.RemoveAll(root)
		}
	}()
	for _, d := range []string{"home", "runner", "tmp"} {
		if err := os.Mkdir(filepath.Join(root, d), 0700); err != nil {
			return zero, err
		}
	}
	env.HomeDir = filepath.Join(root, "home")
	env.TmpDir = filepath.Join(root, "tmp")
	plan, err := PlanCodex(ctx, env, l.CodexPath)
	if err != nil {
		return zero, err
	}
	if err := applyNeutralContext(&plan, l.NeutralDir, env.WorkspaceDir); err != nil {
		return zero, err
	}
	home := filepath.Join(root, "runner", "acp-codex")
	for i, v := range plan.Env {
		if strings.HasPrefix(v, "CODEX_HOME=") {
			plan.Env[i] = "CODEX_HOME=" + home
		}
	}
	if err := os.Mkdir(home, 0700); err != nil {
		return zero, err
	}
	f, err := os.OpenFile(filepath.Join(home, "config.toml"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return zero, err
	}
	_, writeErr := f.WriteString(plan.StartupConfigTOML)
	closeErr := f.Close()
	if writeErr != nil {
		return zero, writeErr
	}
	if closeErr != nil {
		return zero, closeErr
	}
	for _, check := range []struct {
		bin  string
		args []string
		want string
	}{
		{l.NodePath, []string{l.ACPPath, "--version"}, CodexACPName + " " + CodexACPVersion},
		{l.CodexPath, []string{"--version"}, "codex-cli " + CodexRuntimeVersion},
	} {
		cleaned, err := verifyRuntime(ctx, check.bin, check.args, plan.Env, l.NeutralDir, check.want)
		if !cleaned {
			cleanupAllowed = false
		}
		if err != nil {
			return zero, err
		}
	}
	p, err := launchOwned(l.NodePath, []string{l.ACPPath}, plan.Env, l.NeutralDir)
	if err != nil {
		return zero, err
	}
	cleanupAllowed = false
	defer func() { cleanupAllowed = p.Close() == nil }()
	c := NewClient(p)
	defer c.Close()
	c.beforePrompt = func(ctx context.Context) error { return CheckManagedSkills(ctx, env) }
	if err := c.Initialize(ctx, CodexACPName, CodexACPVersion); err != nil {
		return zero, err
	}
	if _, err := c.NewSession(ctx, plan.Session); err != nil {
		return zero, err
	}
	result, err := c.Prompt(ctx, prompt, emit)
	if cleanupErr := p.Close(); err == nil {
		err = cleanupErr
	}
	return result, err
}

func immutablePath(p string, directory bool) error {
	if !filepath.IsAbs(p) || filepath.Clean(p) != p {
		return errors.New("acp: canonical absolute immutable path required")
	}
	first := true
	for {
		st, err := os.Lstat(p)
		if err != nil {
			return err
		}
		raw, ok := st.Sys().(*syscall.Stat_t)
		if !ok || raw.Uid != 0 || st.Mode()&os.ModeSymlink != 0 || st.Mode().Perm()&0022 != 0 {
			return fmt.Errorf("acp: non-immutable path: %s", p)
		}
		if first && !directory {
			if !st.Mode().IsRegular() {
				return errors.New("acp: regular runtime file required")
			}
		} else if !st.IsDir() {
			return errors.New("acp: directory required")
		}
		first = false
		if p == "/" {
			return nil
		}
		p = filepath.Dir(p)
	}
}
func privateParent(parent, workspace string) error {
	if !filepath.IsAbs(parent) || filepath.Clean(parent) != parent || !filepath.IsAbs(workspace) {
		return errors.New("acp: canonical private parent and absolute workspace required")
	}
	resolved, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return err
	}
	if resolved != parent {
		return errors.New("acp: private parent must not contain symlinks")
	}

	// No other user may replace an ancestor. Root-owned sticky temporary
	// directories are allowed because they protect this user's own entries.
	for p := filepath.Dir(parent); ; p = filepath.Dir(p) {
		st, err := os.Lstat(p)
		if err != nil {
			return err
		}
		raw, ok := st.Sys().(*syscall.Stat_t)
		if !ok || !st.IsDir() || (raw.Uid != 0 && raw.Uid != uint32(os.Geteuid())) ||
			(st.Mode().Perm()&0022 != 0 && !(raw.Uid == 0 && st.Mode()&os.ModeSticky != 0)) {
			return errors.New("acp: private parent has replaceable ancestors")
		}
		if p == "/" {
			break
		}
	}
	ws, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(ws, parent)
	if err != nil {
		return err
	}
	if rel == "." || rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return errors.New("acp: private parent must be outside workspace")
	}
	st, err := os.Lstat(parent)
	if err != nil {
		return err
	}
	raw, ok := st.Sys().(*syscall.Stat_t)
	if !ok || !st.IsDir() || raw.Uid != uint32(os.Geteuid()) || st.Mode().Perm()&0077 != 0 {
		return errors.New("acp: private parent must be owner-only")
	}
	return nil
}

type ownedProcess struct {
	in       io.WriteCloser
	out      *os.File
	cmd      *exec.Cmd
	done     chan struct{}
	once     sync.Once
	closeErr error
	waitErr  error
}

func (p *ownedProcess) Read(b []byte) (int, error)  { return p.out.Read(b) }
func (p *ownedProcess) Write(b []byte) (int, error) { return p.in.Write(b) }
func (p *ownedProcess) Close() error {
	p.once.Do(func() {
		_ = p.in.Close()
		if err := syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			p.closeErr = fmt.Errorf("acp: process group termination failed: %w", err)
		}
		_ = p.out.Close()
		select {
		case <-p.done:
		case <-time.After(3 * time.Second):
			p.closeErr = errors.New("acp: process cleanup timed out")
		}
	})
	return p.closeErr
}
func launchOwned(bin string, args, env []string, dir string) (*ownedProcess, error) {
	cmd := exec.Command(bin, args...)
	cmd.Env = env
	cmd.Dir = dir
	cmd.Stderr = io.Discard
	harnesses.NewProcessGroup(cmd)
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, w, err := os.Pipe()
	if err != nil {
		_ = in.Close()
		return nil, err
	}
	cmd.Stdout = w
	if err := cmd.Start(); err != nil {
		_ = in.Close()
		_ = out.Close()
		_ = w.Close()
		return nil, err
	}
	_ = w.Close()
	p := &ownedProcess{in: in, out: out, cmd: cmd, done: make(chan struct{})}
	go func() { p.waitErr = cmd.Wait(); close(p.done) }()
	return p, nil
}

// verifyRuntime bounds both output and lifetime. Version text from a failed
// process is not a verified pin. A false first return retains private files
// because process cleanup could not be established.
func verifyRuntime(ctx context.Context, bin string, args, env []string, dir, want string) (bool, error) {
	checkCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	p, err := launchOwned(bin, args, env, dir)
	if err != nil {
		return true, err
	}
	output := make(chan []byte, 1)
	go func() { b, _ := io.ReadAll(io.LimitReader(p, 1025)); output <- b }()
	var b []byte
	select {
	case b = <-output:
	case <-checkCtx.Done():
	}
	var exited bool
	if len(b) <= 1024 {
		select {
		case <-p.done:
			exited = true
		case <-checkCtx.Done():
		}
	}
	cleanupErr := p.Close()
	if cleanupErr != nil {
		return false, cleanupErr
	}
	if err := checkCtx.Err(); err != nil {
		return true, err
	}
	if !exited || p.waitErr != nil || len(b) > 1024 || strings.TrimSpace(string(b)) != want {
		return true, errors.New("acp: runtime version check failed")
	}
	return true, nil
}

// applyNeutralContext identifies the real workspace without importing its
// instructions. JSON quoting keeps unusual path characters visibly data.
func applyNeutralContext(plan *CodexPlan, neutral, workspace string) error {
	plan.Session.CWD = neutral
	pathJSON, _ := json.Marshal(workspace)
	for i, value := range plan.Env {
		if !strings.HasPrefix(value, "CODEX_CONFIG=") {
			continue
		}
		var config map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(value, "CODEX_CONFIG=")), &config); err != nil {
			return err
		}
		original, _ := config["developer_instructions"].(string)
		config["developer_instructions"] = original + "\nWorkspace location (JSON string): " + string(pathJSON) + "\nThis restricted ACP session starts in a neutral directory. Workspace instructions, configuration and skills are not automatically loaded. Use explicit workspace paths for permitted reads; do not infer write access.\n"
		encoded, err := json.Marshal(config)
		if err != nil {
			return err
		}
		plan.Env[i] = "CODEX_CONFIG=" + string(encoded)
		return nil
	}
	return errors.New("acp: missing trusted session configuration")
}
