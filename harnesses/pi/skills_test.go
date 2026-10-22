package pi

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/darcys22/steadmesh/harnesses"
	"github.com/darcys22/steadmesh/pkg/runtimeapi"
	"github.com/darcys22/steadmesh/pkg/spec"
)

type skillRPCLog struct {
	Kind          string            `json:"kind"`
	PID           int               `json:"pid"`
	Args          []string          `json:"args,omitempty"`
	SessionID     string            `json:"session_id"`
	Skills        map[string]string `json:"skills,omitempty"`
	SkillsPresent bool              `json:"skills_present,omitempty"`
}

// TestPiSkillsRPCProcess is a fake executable speaking the pinned JSONL RPC
// contract. A wrapper invokes this test in a child process with native CLI args.
func TestPiSkillsRPCProcess(t *testing.T) {
	if os.Getenv("STEADMESH_PI_SKILLS_HELPER") != "1" {
		return
	}
	if slices.Contains(os.Args, "--version") {
		fmt.Println(PinnedVersion)
		os.Exit(0)
	}
	fail := func(err error) {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	args := os.Args[slices.Index(os.Args, "--")+1:]
	value := func(flag string) string {
		if n := slices.Index(args, flag); n >= 0 && n+1 < len(args) {
			return args[n+1]
		}
		return ""
	}
	sessionID := value("--session-id")
	if sessionID == "" {
		sessionID = fmt.Sprintf("native-%d", os.Getpid())
	}
	sessionFile := value("--session")
	if sessionFile != "" {
		b, err := os.ReadFile(sessionFile)
		if err != nil {
			fail(err)
		}
		sessionID = string(b)
	} else {
		sessionFile = filepath.Join(value("--session-dir"), "session_"+sessionID+".jsonl")
	}
	skillFiles := map[string]string{}
	for i, arg := range args {
		if arg != "--skill" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(args[i+1], "SKILL.md"))
		if err != nil {
			fail(err)
		}
		skillFiles[args[i+1]] = string(b)
	}
	f, err := os.OpenFile(os.Getenv("STEADMESH_PI_SKILLS_LOG"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		fail(err)
	}
	log := func(kind string) {
		present := true
		for path := range skillFiles {
			if _, err := os.Stat(filepath.Join(path, "SKILL.md")); err != nil {
				present = false
			}
		}
		if err := json.NewEncoder(f).Encode(skillRPCLog{Kind: kind, PID: os.Getpid(), Args: args, SessionID: sessionID, Skills: skillFiles, SkillsPresent: present}); err != nil {
			fail(err)
		}
	}
	log("start")
	out := json.NewEncoder(os.Stdout)
	scan := bufio.NewScanner(os.Stdin)
	for scan.Scan() {
		var cmd struct {
			Type    string `json:"type"`
			ID      string `json:"id"`
			Message string `json:"message"`
		}
		if err := json.Unmarshal(scan.Bytes(), &cmd); err != nil {
			fail(err)
		}
		response := map[string]any{"type": "response", "id": cmd.ID, "command": cmd.Type, "success": true}
		switch cmd.Type {
		case "get_state":
			response["data"] = map[string]any{"sessionFile": sessionFile, "sessionId": sessionID}
		case "prompt":
			log("prompt")
			if err := os.WriteFile(sessionFile, []byte(sessionID), 0600); err != nil {
				fail(err)
			}
			response["data"] = map[string]any{"disposition": "started"}
		default:
			fail(fmt.Errorf("unverified RPC command %s", cmd.Type))
		}
		if err := out.Encode(response); err != nil {
			fail(err)
		}
		if cmd.Type == "prompt" {
			if strings.Contains(cmd.Message, "wait-for-release") {
				deadline := time.Now().Add(10 * time.Second)
				for {
					if _, err := os.Stat(os.Getenv("STEADMESH_PI_SKILLS_RELEASE")); err == nil {
						break
					}
					if time.Now().After(deadline) {
						fail(errors.New("timed out waiting for turn release"))
					}
					time.Sleep(5 * time.Millisecond)
				}
			}
			_ = out.Encode(map[string]any{"type": "message_end", "message": map[string]any{"role": "assistant", "stopReason": "stop", "content": []any{map[string]any{"type": "text", "text": "done"}}}})
			_ = out.Encode(map[string]any{"type": "agent_settled"})
		}
	}
	log("exit")
	_ = f.Close()
	os.Exit(0)
}

type skillLoader struct {
	mu     sync.Mutex
	bundle runtimeapi.SkillsBundleResponse
	err    error
	calls  int
}

func (l *skillLoader) load(context.Context) (runtimeapi.SkillsBundleResponse, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls++
	return l.bundle, l.err
}

func (l *skillLoader) set(bundle runtimeapi.SkillsBundleResponse, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.bundle, l.err = bundle, err
}

func piSkillBundle(text string) runtimeapi.SkillsBundleResponse {
	return runtimeapi.SkillsBundleResponse{Skills: []runtimeapi.SkillBundle{{
		Ref:    spec.SkillRef{Marketplace: "team", Plugin: "engineering", Skill: "review"},
		Source: spec.SkillMarketplace{Repository: "example/skills", Ref: strings.Repeat("a", 40)},
		Files:  map[string][]byte{"SKILL.md": []byte(text), "references/checklist.txt": []byte("test supporting files")},
		Owner:  "static",
	}}}
}

func newSkillsAdapter(t *testing.T, load func(context.Context) (runtimeapi.SkillsBundleResponse, error)) (*Adapter, string, string) {
	t.Helper()
	dir := t.TempDir()
	testBin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "pi")
	quoted := "'" + strings.ReplaceAll(testBin, "'", "'\"'\"'") + "'"
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexec "+quoted+" -test.run=^TestPiSkillsRPCProcess$ -- \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	log, release := filepath.Join(dir, "rpc.jsonl"), filepath.Join(dir, "release")
	env := harnesses.Environment{
		WorkspaceDir: filepath.Join(dir, "workspace"), HomeDir: filepath.Join(dir, "home"), RunnerDir: filepath.Join(dir, "runner"),
		ToolCommand: "/fake-tools", LoadSkills: load,
		HarnessConfig: map[string]string{ConfigBinary: bin, ConfigVersion: PinnedVersion},
		Model:         &harnesses.ModelEndpoint{ID: "model", API: harnesses.APIOpenAIChat, BaseURL: "http://127.0.0.1:1", APIKey: "placeholder"},
		ExtraEnv:      []string{"STEADMESH_PI_SKILLS_HELPER=1", "STEADMESH_PI_SKILLS_LOG=" + log, "STEADMESH_PI_SKILLS_RELEASE=" + release},
	}
	a := New()
	if err := a.Prepare(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := a.Stop(context.Background()); err != nil {
			t.Error(err)
		}
	})
	return a, log, release
}

func readSkillLog(t *testing.T, path, kind string) []skillRPCLog {
	t.Helper()
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var records []skillRPCLog
	lines := strings.Split(string(b), "\n")
	for _, line := range lines[:len(lines)-1] {
		var entry skillRPCLog
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatal(err)
		}
		if entry.Kind == kind {
			records = append(records, entry)
		}
	}
	return records
}

func deliverSkillTurn(t *testing.T, a *Adapter, text string) harnesses.TurnResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := a.Deliver(ctx, harnesses.Delivery{Message: runtimeapi.Envelope{MessageID: text, Body: text}})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func assertSkillRootRemoved(t *testing.T, root string) {
	t.Helper()
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("snapshot %s was not removed: %v", root, err)
	}
}

func TestSkillsRefreshAtNextTurnPreservesNativeSession(t *testing.T) {
	loader := &skillLoader{bundle: piSkillBundle("initial review instructions")}
	a, log, release := newSkillsAdapter(t, loader.load)
	if _, err := a.StartOrResume(context.Background(), harnesses.RecoveryDescriptor{}); err != nil {
		t.Fatal(err)
	}
	initialRoot := a.skillSnapshot.Root
	starts := readSkillLog(t, log, "start")
	if len(starts) != 1 || len(starts[0].Skills) != 1 || loader.calls != 1 {
		t.Fatalf("initial load: starts=%+v calls=%d", starts, loader.calls)
	}
	for _, text := range starts[0].Skills {
		if !strings.HasSuffix(text, "initial review instructions") {
			t.Fatalf("initial skill content: %q", text)
		}
	}
	first := deliverSkillTurn(t, a, "first")
	if first.Status != harnesses.TurnCompleted || first.SessionID == "" {
		t.Fatalf("first turn: %+v", first)
	}
	if a.skillSnapshot.Root != initialRoot || len(readSkillLog(t, log, "start")) != 1 || loader.calls != 2 {
		t.Fatal("unchanged skills restarted Pi or replaced its active snapshot")
	}

	// An installation while Pi is active cannot mutate the resources in use.
	type delivered struct {
		result harnesses.TurnResult
		err    error
	}
	turn := make(chan delivered, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go func() {
		result, err := a.Deliver(ctx, harnesses.Delivery{Message: runtimeapi.Envelope{MessageID: "hold", Body: "wait-for-release"}})
		turn <- delivered{result, err}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for len(readSkillLog(t, log, "prompt")) != 2 {
		if time.Now().After(deadline) {
			t.Fatal("second turn did not start")
		}
		time.Sleep(5 * time.Millisecond)
	}
	loader.set(piSkillBundle("updated review instructions"), nil)
	if len(readSkillLog(t, log, "start")) != 1 {
		t.Fatal("active turn was restarted")
	}
	if err := os.WriteFile(release, nil, 0600); err != nil {
		t.Fatal(err)
	}
	got := <-turn
	if got.err != nil || got.result.Status != harnesses.TurnCompleted || got.result.SessionID != first.SessionID {
		t.Fatalf("active turn changed: %+v, %v", got.result, got.err)
	}
	changed := deliverSkillTurn(t, a, "changed")
	starts = readSkillLog(t, log, "start")
	if changed.Status != harnesses.TurnCompleted || changed.SessionID != first.SessionID || len(starts) != 2 {
		t.Fatalf("changed skills: turn=%+v starts=%+v", changed, starts)
	}
	if !slices.Contains(starts[1].Args, "--session") || len(starts[1].Skills) != 1 {
		t.Fatalf("new process did not resume with skills: %+v", starts[1])
	}
	for _, text := range starts[1].Skills {
		if !strings.HasSuffix(text, "updated review instructions") {
			t.Fatalf("next turn loaded stale content: %q", text)
		}
	}
	assertSkillRootRemoved(t, initialRoot)
	updatedRoot := a.skillSnapshot.Root

	loader.set(runtimeapi.SkillsBundleResponse{Skills: []runtimeapi.SkillBundle{}}, nil)
	revoked := deliverSkillTurn(t, a, "revoked")
	starts = readSkillLog(t, log, "start")
	if revoked.Status != harnesses.TurnCompleted || revoked.SessionID != first.SessionID || len(starts) != 3 || len(starts[2].Skills) != 0 {
		t.Fatalf("revoked skills still loaded or lost session: %+v, %+v", revoked, starts)
	}
	assertSkillRootRemoved(t, updatedRoot)
	emptyRoot := a.skillSnapshot.Root
	if err := a.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertSkillRootRemoved(t, emptyRoot)
	for _, exit := range readSkillLog(t, log, "exit") {
		if !exit.SkillsPresent {
			t.Fatal("snapshot was removed before orderly process shutdown")
		}
	}
}

func TestSkillsFailureDoesNotRunStaleSkills(t *testing.T) {
	for _, failure := range []string{"fetch", "materialize"} {
		t.Run(failure, func(t *testing.T) {
			loader := &skillLoader{bundle: piSkillBundle("valid skill")}
			a, log, _ := newSkillsAdapter(t, loader.load)
			if _, err := a.StartOrResume(context.Background(), harnesses.RecoveryDescriptor{}); err != nil {
				t.Fatal(err)
			}
			first := deliverSkillTurn(t, a, "first")
			root := a.skillSnapshot.Root
			if failure == "fetch" {
				loader.set(runtimeapi.SkillsBundleResponse{}, errors.New("authorization lookup failed"))
			} else {
				invalid := piSkillBundle("invalid skill")
				invalid.Skills[0].Files["../outside"] = []byte("escape")
				loader.set(invalid, nil)
			}
			failed := deliverSkillTurn(t, a, "must-not-run")
			if failed.Status != harnesses.TurnFailed || !strings.Contains(failed.Error, "synchronize skills") {
				t.Fatalf("refresh failure was not reported: %+v", failed)
			}
			if a.proc != nil || a.skillSnapshot != nil || len(readSkillLog(t, log, "prompt")) != 1 {
				t.Fatal("refresh failure left stale skills available or sent another prompt")
			}
			assertSkillRootRemoved(t, root)
			loader.set(piSkillBundle("fresh authorization"), nil)
			recovered := deliverSkillTurn(t, a, "recovered")
			if recovered.Status != harnesses.TurnCompleted || recovered.SessionID != first.SessionID {
				t.Fatalf("failed refresh prevented native resume: %+v", recovered)
			}
		})
	}
}

func TestSkillsInitialFailureDoesNotStartPi(t *testing.T) {
	a, log, _ := newSkillsAdapter(t, func(context.Context) (runtimeapi.SkillsBundleResponse, error) {
		return runtimeapi.SkillsBundleResponse{}, errors.New("unavailable")
	})
	if _, err := a.StartOrResume(context.Background(), harnesses.RecoveryDescriptor{}); err == nil {
		t.Fatal("startup ignored skill fetch failure")
	}
	if len(readSkillLog(t, log, "start")) != 0 {
		t.Fatal("Pi started despite initial skill fetch failure")
	}
}

func TestSkillsRefreshBeforeFirstTurnPreservesEmptyNativeSession(t *testing.T) {
	loader := &skillLoader{bundle: piSkillBundle("initial")}
	a, log, _ := newSkillsAdapter(t, loader.load)
	if _, err := a.StartOrResume(context.Background(), harnesses.RecoveryDescriptor{}); err != nil {
		t.Fatal(err)
	}
	sessionID := a.session.SessionID
	loader.set(piSkillBundle("changed before first turn"), nil)
	turn := deliverSkillTurn(t, a, "first")
	starts := readSkillLog(t, log, "start")
	if turn.Status != harnesses.TurnCompleted || turn.SessionID != sessionID || len(starts) != 2 || !slices.Contains(starts[1].Args, "--session-id") {
		t.Fatalf("empty native session lost across refresh: %+v, %+v", turn, starts)
	}
}

func TestSkillsDisabledKeepsNativeDefaults(t *testing.T) {
	a, log, _ := newSkillsAdapter(t, nil)
	if _, err := a.StartOrResume(context.Background(), harnesses.RecoveryDescriptor{}); err != nil {
		t.Fatal(err)
	}
	turn := deliverSkillTurn(t, a, "no-managed-skills")
	starts := readSkillLog(t, log, "start")
	if turn.Status != harnesses.TurnCompleted || len(starts) != 1 || a.skillSnapshot != nil {
		t.Fatalf("disabled skills changed native behavior: %+v, %+v", turn, starts)
	}
	if slices.Contains(starts[0].Args, "--skill") || slices.Contains(starts[0].Args, "--no-skills") {
		t.Fatalf("disabled policy changed native skill flags: %v", starts[0].Args)
	}
}

func TestStopCancelsSkillRefresh(t *testing.T) {
	fetching := make(chan struct{})
	calls := 0
	a, log, _ := newSkillsAdapter(t, func(ctx context.Context) (runtimeapi.SkillsBundleResponse, error) {
		calls++
		if calls == 1 {
			return piSkillBundle("initial"), nil
		}
		close(fetching)
		<-ctx.Done()
		return runtimeapi.SkillsBundleResponse{}, ctx.Err()
	})
	if _, err := a.StartOrResume(context.Background(), harnesses.RecoveryDescriptor{}); err != nil {
		t.Fatal(err)
	}
	root := a.skillSnapshot.Root
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	turn := make(chan harnesses.TurnResult, 1)
	go func() {
		result, _ := a.Deliver(ctx, harnesses.Delivery{})
		turn <- result
	}()
	select {
	case <-fetching:
	case <-ctx.Done():
		t.Fatal("refresh did not start")
	}
	stopped := make(chan error, 1)
	go func() { stopped <- a.Stop(context.Background()) }()
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		cancel()
		<-stopped
		t.Fatal("Stop did not cancel the in-flight skill fetch")
	}
	if result := <-turn; result.Status != harnesses.TurnInterrupted {
		t.Fatalf("refresh interruption: %+v", result)
	}
	if len(readSkillLog(t, log, "prompt")) != 0 {
		t.Fatal("interrupted refresh sent a prompt")
	}
	assertSkillRootRemoved(t, root)
}
