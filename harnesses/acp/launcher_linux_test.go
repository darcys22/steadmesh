//go:build linux

package acp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/darcys22/steadmesh/harnesses"
	"github.com/darcys22/steadmesh/pkg/runtimeapi"
)

func TestImmutablePathRejectsWritableAndSymlink(t *testing.T) {
	dir := t.TempDir()
	if err := immutablePath(dir, true); err == nil {
		t.Fatal("accepted mutable test directory")
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink("/", link); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{link, "relative", "/etc/../etc"} {
		if err := immutablePath(p, true); err == nil {
			t.Fatalf("accepted %s", p)
		}
	}
}
func TestPrivateParentBoundaries(t *testing.T) {
	root := t.TempDir()
	ws := filepath.Join(root, "workspace")
	parent := filepath.Join(root, "private")
	for _, p := range []string{ws, parent} {
		if err := os.Mkdir(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := privateParent(parent, ws); err != nil {
		t.Fatal(err)
	}
	if err := privateParent(ws, ws); err == nil {
		t.Fatal("accepted workspace")
	}
	inside := filepath.Join(ws, "private")
	_ = os.Mkdir(inside, 0700)
	if err := privateParent(inside, ws); err == nil {
		t.Fatal("accepted workspace descendant")
	}
	_ = os.Chmod(parent, 0755)
	if err := privateParent(parent, ws); err == nil {
		t.Fatal("accepted shared parent")
	}
	_ = os.Chmod(parent, 0700)
	link := filepath.Join(root, "link")
	_ = os.Symlink(parent, link)
	if err := privateParent(link, ws); err == nil {
		t.Fatal("accepted symlink")
	}
}
func TestOwnedProcessCloseUnblocksPipesAndIsConcurrent(t *testing.T) {
	p, err := launchOwned("/bin/sh", []string{"-c", "sleep 60 & wait"}, []string{"PATH=/usr/bin:/bin"}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	readDone := make(chan struct{})
	go func() { var b [1]byte; _, _ = p.Read(b[:]); close(readDone) }()
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := p.Close(); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	select {
	case <-readDone:
	case <-time.After(time.Second):
		t.Fatal("read remained blocked")
	}
	select {
	case <-p.done:
	default:
		t.Fatal("process not reaped")
	}
}
func TestOwnedProcessDoesNotInheritEnvironment(t *testing.T) {
	t.Setenv("ACP_TEST_SECRET", "do-not-inherit")
	p, err := launchOwned("/bin/sh", []string{"-c", `printf '%s' "${ACP_TEST_SECRET-unset}"`}, []string{"PATH=/usr/bin:/bin"}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	b := make([]byte, 64)
	n, err := p.Read(b)
	if err != nil {
		t.Fatal(err)
	}
	if string(b[:n]) != "unset" {
		t.Fatalf("inherited environment: %q", b[:n])
	}
}
func TestLauncherFailsBeforeLaunch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := (ExperimentalLauncher{}).Run(ctx, planEnv(), "x", nil); err != context.Canceled {
		t.Fatalf("got %v", err)
	}
	// No paths are executable, including in root test containers.
	_, err := (ExperimentalLauncher{}).Run(context.Background(), planEnv(), "x", nil)
	if err == nil || !(strings.Contains(err.Error(), "non-root") || strings.Contains(err.Error(), "immutable")) {
		t.Fatalf("unexpected validation: %v", err)
	}
}

func TestVerifyRuntimeChecksExitOutputAndCancellation(t *testing.T) {
	for _, tc := range []struct {
		name, command string
		wantOK        bool
	}{
		{"matching", `printf 'pinned 1.0\n'`, true},
		{"nonzero", `printf 'pinned 1.0\n'; exit 1`, false},
		{"wrong", `printf 'other 1.0\n'`, false},
		{"oversized", `head -c 2000 /dev/zero`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cleaned, err := verifyRuntime(context.Background(), "/bin/sh", []string{"-c", tc.command}, []string{"PATH=/usr/bin:/bin"}, t.TempDir(), "pinned 1.0")
			if !cleaned || (err == nil) != tc.wantOK {
				t.Fatalf("cleaned=%v err=%v", cleaned, err)
			}
		})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	cleaned, err := verifyRuntime(ctx, "/bin/sh", []string{"-c", "sleep 60 & wait"}, []string{"PATH=/usr/bin:/bin"}, t.TempDir(), "pinned 1.0")
	if !cleaned || err != context.DeadlineExceeded {
		t.Fatalf("cleaned=%v err=%v", cleaned, err)
	}
}

func TestLauncherRejectsExtraMCPAndSkillsBeforePaths(t *testing.T) {
	env := planEnv()
	env.MCPServers = []harnesses.MCPServer{{Name: "extra", Command: "/missing"}}
	if _, err := (ExperimentalLauncher{}).Run(context.Background(), env, "x", nil); err == nil || !strings.Contains(err.Error(), "platform bridge") {
		t.Fatalf("unexpected extra MCP result: %v", err)
	}
	env = planEnv()
	env.LoadSkills = func(context.Context) (runtimeapi.SkillsBundleResponse, error) {
		return runtimeapi.SkillsBundleResponse{Skills: []runtimeapi.SkillBundle{{}}}, nil
	}
	if _, err := (ExperimentalLauncher{}).Run(context.Background(), env, "x", nil); err == nil || !strings.Contains(err.Error(), "skill loading") {
		t.Fatalf("unexpected skills result: %v", err)
	}
}

func TestNeutralContextPreservesBridgeAndIdentifiesWorkspace(t *testing.T) {
	env := planEnv()
	env.ExtraEnv = []string{"STEADMESH_EXECUTION=execution-7"}
	plan, err := PlanCodex(context.Background(), env, "/opt/codex")
	if err != nil {
		t.Fatal(err)
	}
	startup := plan.StartupConfigTOML
	if err := applyNeutralContext(&plan, "/opt/empty", env.WorkspaceDir); err != nil {
		t.Fatal(err)
	}
	if plan.Session.CWD != "/opt/empty" || plan.StartupConfigTOML != startup {
		t.Fatal("changed bridge or incorrect cwd")
	}
	if !strings.Contains(startup, env.RunnerDir) || !strings.Contains(startup, env.TokenFile) || !strings.Contains(startup, "execution-7") {
		t.Fatal("lost bridge authority")
	}
	for _, v := range plan.Env {
		if strings.HasPrefix(v, "CODEX_CONFIG=") {
			var c map[string]any
			if err := json.Unmarshal([]byte(strings.TrimPrefix(v, "CODEX_CONFIG=")), &c); err != nil {
				t.Fatal(err)
			}
			text, _ := c["developer_instructions"].(string)
			if !strings.Contains(text, env.WorkspaceDir) || !strings.Contains(text, "not automatically loaded") {
				t.Fatal("missing neutral workspace semantics")
			}
			return
		}
	}
	t.Fatal("missing configuration")
}

func TestLauncherRejectsUnavailableSkillsBeforeRuntimeValidation(t *testing.T) {
	env := planEnv()
	env.LoadSkills = func(context.Context) (runtimeapi.SkillsBundleResponse, error) {
		return runtimeapi.SkillsBundleResponse{}, errors.New("fixture authorization outage")
	}
	if _, err := (ExperimentalLauncher{}).Run(context.Background(), env, "x", nil); err == nil || !strings.Contains(err.Error(), "authorization unavailable") {
		t.Fatalf("unexpected unavailable skills result: %v", err)
	}
}
