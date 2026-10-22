package claudecode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/darcys22/steadmesh/harnesses"
	"github.com/darcys22/steadmesh/pkg/runtimeapi"
	"github.com/darcys22/steadmesh/pkg/skills"
	"github.com/darcys22/steadmesh/pkg/spec"
)

// The test binary doubles as a deterministic Claude executable. It records
// what was actually available to the child, not just the generated arguments.
func TestMain(m *testing.M) {
	if os.Getenv("STEADMESH_TEST_CLAUDE") == "1" {
		if len(os.Args) == 2 && os.Args[1] == "--version" {
			fmt.Println(PinnedVersion)
			os.Exit(0)
		}
		rec := claudeInvocation{Args: os.Args[1:]}
		for i, arg := range rec.Args {
			if arg != "--plugin-dir" || i+1 >= len(rec.Args) {
				continue
			}
			dir := rec.Args[i+1]
			rec.PluginDirs = append(rec.PluginDirs, dir)
			if _, err := os.Stat(filepath.Join(dir, ".claude-plugin", "plugin.json")); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			paths, _ := filepath.Glob(filepath.Join(dir, "skills", "*", "SKILL.md"))
			for _, p := range paths {
				b, _ := os.ReadFile(p)
				rec.Skills = append(rec.Skills, string(b))
			}
		}
		f, err := os.OpenFile(os.Getenv("STEADMESH_TEST_LOG"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			os.Exit(2)
		}
		_ = json.NewEncoder(f).Encode(rec)
		_ = f.Close()
		fmt.Println(`{"type":"result","subtype":"success","is_error":false,"result":"ok","session_id":"native-session"}`)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

type claudeInvocation struct {
	Args       []string
	PluginDirs []string
	Skills     []string
}

func skillFixture(name string) runtimeapi.SkillsBundleResponse {
	return runtimeapi.SkillsBundleResponse{Skills: []runtimeapi.SkillBundle{{
		Ref:    spec.SkillRef{Marketplace: "team", Plugin: "engineering", Skill: name},
		Source: spec.SkillMarketplace{Repository: "example/skills", Ref: strings.Repeat("a", 40)},
		Files:  map[string][]byte{"SKILL.md": []byte("---\nname: " + name + "\ndescription: A test skill\n---\nUse " + name + ".\n")},
	}}}
}

func readClaudeInvocations(t *testing.T, path string) []claudeInvocation {
	t.Helper()
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var records []claudeInvocation
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var rec claudeInvocation
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatal(err)
		}
		records = append(records, rec)
	}
	return records
}

func newSkillTestAdapter(t *testing.T, load func(context.Context) (runtimeapi.SkillsBundleResponse, error)) (*Adapter, string) {
	t.Helper()
	base := t.TempDir()
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(base, "invocations.jsonl")
	env := harnesses.Environment{
		HomeDir: filepath.Join(base, "home"), RunnerDir: filepath.Join(base, "runner"), WorkspaceDir: filepath.Join(base, "workspace"),
		ToolCommand: "/unused/steadmesh-tools", TokenFile: filepath.Join(base, "token"),
		Model:         &harnesses.ModelEndpoint{ID: "fake", API: harnesses.APIAnthropicMessages, BaseURL: "http://127.0.0.1/unused"},
		HarnessConfig: map[string]string{ConfigBinary: bin}, LoadSkills: load,
		ExtraEnv: []string{"STEADMESH_TEST_CLAUDE=1", "STEADMESH_TEST_LOG=" + log},
	}
	a := New()
	if err := a.Prepare(t.Context(), env); err != nil {
		t.Fatal(err)
	}
	if _, err := a.StartOrResume(t.Context(), harnesses.RecoveryDescriptor{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Stop(context.Background()) })
	return a, log
}

func TestMarketplaceSkillsRefreshEveryTurn(t *testing.T) {
	current := skillFixture("review")
	calls := 0
	var loadErr error
	a, log := newSkillTestAdapter(t, func(context.Context) (runtimeapi.SkillsBundleResponse, error) {
		calls++
		return current, loadErr
	})
	deliver := func(want string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		res, err := a.Deliver(ctx, harnesses.Delivery{ExecutionID: "exec", Message: runtimeapi.Envelope{MessageID: "m"}})
		if err != nil || res.Status != want {
			t.Fatalf("Deliver: %+v, %v", res, err)
		}
	}
	deliver(harnesses.TurnCompleted)
	current = skillFixture("testing")
	deliver(harnesses.TurnCompleted)
	current = runtimeapi.SkillsBundleResponse{}
	deliver(harnesses.TurnCompleted)
	loadErr = errors.New("authorization unavailable")
	deliver(harnesses.TurnFailed)
	if calls != 4 {
		t.Fatalf("skill refreshes = %d, want once per turn", calls)
	}
	records := readClaudeInvocations(t, log)
	if len(records) != 3 {
		t.Fatalf("spawned %d processes; refresh failure must not run stale skills", len(records))
	}
	for i, name := range []string{"review", "testing"} {
		if len(records[i].PluginDirs) != 1 || len(records[i].Skills) != 1 || !strings.Contains(records[i].Skills[0], "name: "+skills.NativeSkillName(skillFixture(name).Skills[0].Ref)) {
			t.Fatalf("turn %d skills: %+v", i, records[i])
		}
		for _, path := range records[i].PluginDirs {
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("generated plugin survives completed child: %s (%v)", path, err)
			}
		}
		if !contains(records[i].Args, "--bare") {
			t.Fatal("explicit skill loading must not disable --bare")
		}
	}
	if len(records[2].PluginDirs) != 0 || len(records[2].Skills) != 0 {
		t.Fatalf("revoked skills remain: %+v", records[2])
	}
	for _, rec := range records[1:] {
		if !strings.Contains(strings.Join(rec.Args, " "), "--resume native-session") {
			t.Fatal("skill refresh lost native session")
		}
	}
}

func TestMarketplaceSkillsDisabledPreservesArguments(t *testing.T) {
	a, log := newSkillTestAdapter(t, nil)
	res, err := a.Deliver(t.Context(), harnesses.Delivery{Message: runtimeapi.Envelope{MessageID: "m"}})
	if err != nil || res.Status != harnesses.TurnCompleted {
		t.Fatalf("Deliver: %+v, %v", res, err)
	}
	recs := readClaudeInvocations(t, log)
	if len(recs) != 1 || !reflect.DeepEqual(recs[0].Args, a.Args("", a.bootstrap)) {
		t.Fatalf("disabled marketplace support changes native arguments: %+v", recs)
	}
}

func TestInvalidSkillNeverStartsClaude(t *testing.T) {
	bundle := skillFixture("bad")
	bundle.Skills[0].Files["../outside"] = []byte("escape")
	a, log := newSkillTestAdapter(t, func(context.Context) (runtimeapi.SkillsBundleResponse, error) { return bundle, nil })
	res, err := a.Deliver(t.Context(), harnesses.Delivery{Message: runtimeapi.Envelope{MessageID: "m"}})
	if err != nil || res.Status != harnesses.TurnFailed || !strings.Contains(res.Error, "unsafe skill file") {
		t.Fatalf("Deliver: %+v, %v", res, err)
	}
	if got := readClaudeInvocations(t, log); len(got) != 0 {
		t.Fatalf("invalid bundle started Claude: %+v", got)
	}
}

func TestManagedSkillsRejectPinnedSystemPrompt(t *testing.T) {
	for _, value := range []string{"on", ""} {
		env := harnesses.Environment{
			HarnessConfig: map[string]string{ConfigPromptSnapshot: value},
			LoadSkills: func(context.Context) (runtimeapi.SkillsBundleResponse, error) {
				return runtimeapi.SkillsBundleResponse{}, nil
			},
		}
		if err := New().Prepare(t.Context(), env); err == nil || !strings.Contains(err.Error(), "system_prompt_snapshot=off") {
			t.Fatalf("snapshot setting %q should reject stale-skill configuration: %v", value, err)
		}
	}
}

func contains(args []string, s string) bool {
	for _, arg := range args {
		if arg == s {
			return true
		}
	}
	return false
}
