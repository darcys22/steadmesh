package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/darcys22/steadmesh/harnesses"
	"github.com/darcys22/steadmesh/pkg/runtimeapi"
	"github.com/darcys22/steadmesh/pkg/spec"
)

// Run a real stdio subprocess so the tests cover the app-server handshake,
// skill cache reloads, process replacement, and thread-resume ordering.
func TestMain(m *testing.M) {
	if os.Getenv("STEADMESH_TEST_CODEX") == "1" {
		if len(os.Args) == 2 && os.Args[1] == "--version" {
			fmt.Println(PinnedVersion)
			os.Exit(0)
		}
		runFakeCodex()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

type codexRequest struct {
	ID     int64          `json:"id"`
	Method string         `json:"method"`
	Params map[string]any `json:"params"`
	PID    int            `json:"pid"`
}

func runFakeCodex() {
	log, err := os.OpenFile(os.Getenv("STEADMESH_TEST_LOG"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		os.Exit(2)
	}
	defer log.Close()
	enc := json.NewEncoder(os.Stdout)
	var roots []string
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		var req codexRequest
		if json.Unmarshal(sc.Bytes(), &req) != nil {
			os.Exit(3)
		}
		req.PID = os.Getpid()
		_ = json.NewEncoder(log).Encode(req)
		var result any = map[string]any{}
		switch req.Method {
		case "initialized":
			continue
		case "initialize":
			result = map[string]any{"userAgent": "fake"}
		case "skills/extraRoots/set":
			if req.Params["extraRoots"] == nil { // null must never mean clear
				os.Exit(4)
			}
			roots = nil
			for _, v := range req.Params["extraRoots"].([]any) {
				roots = append(roots, v.(string))
			}
		case "skills/list":
			failure, _ := os.ReadFile(os.Getenv("STEADMESH_TEST_FAILURE"))
			if strings.TrimSpace(string(failure)) == "reload" {
				_ = enc.Encode(map[string]any{"id": req.ID, "error": map[string]any{"code": -32000, "message": "skill reload unavailable"}})
				continue
			}
			entries := []any{}
			if strings.TrimSpace(string(failure)) != "missing" {
				for _, root := range roots {
					path := filepath.Join(root, "SKILL.md")
					if _, err := os.ReadFile(path); err != nil {
						os.Exit(5)
					}
					entries = append(entries, map[string]any{"path": path, "enabled": true})
				}
			}
			result = map[string]any{"data": []any{map[string]any{"skills": entries, "errors": []any{}}}}
		case "thread/start", "thread/resume":
			result = map[string]any{"thread": map[string]any{"id": "native-thread"}}
		case "turn/start":
			id := fmt.Sprintf("turn-%d", req.ID)
			_ = enc.Encode(map[string]any{"id": req.ID, "result": map[string]any{"turn": map[string]any{"id": id, "status": "inProgress"}}})
			_ = enc.Encode(map[string]any{"method": "turn/completed", "params": map[string]any{"turn": map[string]any{"id": id, "status": "completed"}}})
			continue
		default:
			os.Exit(6)
		}
		_ = enc.Encode(map[string]any{"id": req.ID, "result": result})
	}
}

func codexSkillFixture(name string) runtimeapi.SkillsBundleResponse {
	return runtimeapi.SkillsBundleResponse{Skills: []runtimeapi.SkillBundle{{
		Ref:    spec.SkillRef{Marketplace: "team", Plugin: "engineering", Skill: name},
		Source: spec.SkillMarketplace{Repository: "example/skills", Ref: strings.Repeat("a", 40)},
		Files:  map[string][]byte{"SKILL.md": []byte("---\nname: " + name + "\ndescription: Test skill\n---\nUse " + name + ".\n")},
	}}}
}

func newCodexSkillTestAdapter(t *testing.T, load func(context.Context) (runtimeapi.SkillsBundleResponse, error)) (*Adapter, string, string) {
	t.Helper()
	base := t.TempDir()
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	log, failure := filepath.Join(base, "requests.jsonl"), filepath.Join(base, "failure")
	env := harnesses.Environment{
		HomeDir: filepath.Join(base, "home"), RunnerDir: filepath.Join(base, "runner"), WorkspaceDir: filepath.Join(base, "workspace"),
		ToolCommand: "/unused/steadmesh-tools", TokenFile: filepath.Join(base, "token"),
		Model:         &harnesses.ModelEndpoint{ID: "fake", API: harnesses.APIOpenAIResponses, BaseURL: "http://127.0.0.1/unused"},
		HarnessConfig: map[string]string{ConfigBinary: bin}, LoadSkills: load,
		ExtraEnv: []string{"STEADMESH_TEST_CODEX=1", "STEADMESH_TEST_LOG=" + log, "STEADMESH_TEST_FAILURE=" + failure},
	}
	a := New()
	if err := a.Prepare(t.Context(), env); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Stop(context.Background()) })
	return a, log, failure
}

func readCodexRequests(t *testing.T, path string) []codexRequest {
	t.Helper()
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var records []codexRequest
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var rec codexRequest
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatal(err)
		}
		records = append(records, rec)
	}
	return records
}

func requestsWithMethod(reqs []codexRequest, method string) []codexRequest {
	var out []codexRequest
	for _, req := range reqs {
		if req.Method == method {
			out = append(out, req)
		}
	}
	return out
}

func codexDeliver(t *testing.T, a *Adapter, want string) harnesses.TurnResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	res, err := a.Deliver(ctx, harnesses.Delivery{ExecutionID: "exec", Message: runtimeapi.Envelope{MessageID: "m"}})
	if err != nil || res.Status != want {
		t.Fatalf("Deliver: %+v, %v", res, err)
	}
	return res
}

func TestMarketplaceSkillsSyncAndRevocation(t *testing.T) {
	current := codexSkillFixture("review")
	calls := 0
	a, log, _ := newCodexSkillTestAdapter(t, func(context.Context) (runtimeapi.SkillsBundleResponse, error) {
		calls++
		return current, nil
	})
	if _, err := a.StartOrResume(t.Context(), harnesses.RecoveryDescriptor{}); err != nil {
		t.Fatal(err)
	}
	original := a.skillSnapshot.Root
	codexDeliver(t, a, harnesses.TurnCompleted)
	if a.skillSnapshot.Root != original {
		t.Fatal("unchanged skill set must reuse its loaded snapshot")
	}
	current = codexSkillFixture("testing")
	codexDeliver(t, a, harnesses.TurnCompleted)
	if _, err := os.Stat(original); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("previous snapshot not removed after reload: %v", err)
	}
	changed := a.skillSnapshot.Root
	current = runtimeapi.SkillsBundleResponse{}
	codexDeliver(t, a, harnesses.TurnCompleted)
	if _, err := os.Stat(changed); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("revoked snapshot remains: %v", err)
	}
	if calls != 4 {
		t.Fatalf("LoadSkills calls = %d, want startup plus each turn", calls)
	}
	reqs := readCodexRequests(t, log)
	roots := requestsWithMethod(reqs, "skills/extraRoots/set")
	if len(roots) != 3 || len(roots[0].Params["extraRoots"].([]any)) != 1 || len(roots[2].Params["extraRoots"].([]any)) != 0 {
		t.Fatalf("expected startup, change, empty-root revocation; got %+v", roots)
	}
	if len(requestsWithMethod(reqs, "initialize")) != 3 || len(requestsWithMethod(reqs, "thread/start")) != 1 || len(requestsWithMethod(reqs, "thread/resume")) != 2 {
		t.Fatal("changed skills must reinitialize the process and resume, not replace, the native thread")
	}
	for _, r := range requestsWithMethod(reqs, "skills/list") {
		if r.Params["forceReload"] != true || len(r.Params["cwds"].([]any)) != 1 {
			t.Fatalf("skill reload is not forced for the workspace: %+v", r)
		}
	}
	methods := []string{}
	for _, r := range reqs[:5] {
		methods = append(methods, r.Method)
	}
	if got := strings.Join(methods, ","); got != "initialize,initialized,skills/extraRoots/set,skills/list,thread/start" {
		t.Fatalf("skills must load before opening the native thread: %s", got)
	}
	empty := a.skillSnapshot.Root
	_ = a.Stop(context.Background())
	if _, err := os.Stat(empty); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Stop left generated snapshot: %v", err)
	}
}

func TestMarketplaceSkillFailuresCloseServerAndResume(t *testing.T) {
	for _, mode := range []string{"fetch", "reload", "missing"} {
		t.Run(mode, func(t *testing.T) {
			current := codexSkillFixture("review")
			var loadErr error
			a, log, failure := newCodexSkillTestAdapter(t, func(context.Context) (runtimeapi.SkillsBundleResponse, error) { return current, loadErr })
			if _, err := a.StartOrResume(t.Context(), harnesses.RecoveryDescriptor{}); err != nil {
				t.Fatal(err)
			}
			codexDeliver(t, a, harnesses.TurnCompleted)
			old := a.skillSnapshot.Root
			if mode == "fetch" {
				loadErr = errors.New("authorization unavailable")
			} else {
				current = codexSkillFixture("testing")
				if err := os.WriteFile(failure, []byte(mode), 0600); err != nil {
					t.Fatal(err)
				}
			}
			codexDeliver(t, a, harnesses.TurnFailed)
			if a.rpc != nil || a.skillSnapshot != nil {
				t.Fatal("failed refresh left a server or cached skill snapshot alive")
			}
			if _, err := os.Stat(old); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("failed refresh retained stale files: %v", err)
			}
			before := readCodexRequests(t, log)
			if len(requestsWithMethod(before, "turn/start")) != 1 {
				t.Fatal("failed refresh started a model turn")
			}
			loadErr = nil
			_ = os.Remove(failure)
			res := codexDeliver(t, a, harnesses.TurnCompleted)
			if res.SessionID != "native-thread" || res.Recovery != nil {
				t.Fatalf("skill failure destroyed native session: %+v", res)
			}
			reqs := readCodexRequests(t, log)
			if len(requestsWithMethod(reqs, "thread/start")) != 1 || len(requestsWithMethod(reqs, "thread/resume")) != 1 {
				t.Fatalf("recovery should resume exactly one existing thread: %+v", reqs)
			}
		})
	}
}

func TestMarketplaceSkillStartupFailureDoesNotStartServer(t *testing.T) {
	a, log, _ := newCodexSkillTestAdapter(t, func(context.Context) (runtimeapi.SkillsBundleResponse, error) {
		return runtimeapi.SkillsBundleResponse{}, errors.New("authorization unavailable")
	})
	if _, err := a.StartOrResume(t.Context(), harnesses.RecoveryDescriptor{}); err == nil {
		t.Fatal("startup must fail closed")
	}
	if got := readCodexRequests(t, log); len(got) != 0 {
		t.Fatalf("failed authorization started app-server: %+v", got)
	}
}

func TestMarketplaceSkillsDisabledAvoidsExtraRPC(t *testing.T) {
	a, log, _ := newCodexSkillTestAdapter(t, nil)
	if _, err := a.StartOrResume(t.Context(), harnesses.RecoveryDescriptor{}); err != nil {
		t.Fatal(err)
	}
	codexDeliver(t, a, harnesses.TurnCompleted)
	for _, r := range readCodexRequests(t, log) {
		if strings.HasPrefix(r.Method, "skills/") {
			t.Fatalf("disabled marketplace support made new RPC: %+v", r)
		}
	}
}

func TestStopCancelsMarketplaceSkillRefresh(t *testing.T) {
	fetching := make(chan struct{})
	calls := 0
	a, _, _ := newCodexSkillTestAdapter(t, func(ctx context.Context) (runtimeapi.SkillsBundleResponse, error) {
		calls++
		if calls == 1 {
			return codexSkillFixture("review"), nil
		}
		close(fetching)
		<-ctx.Done()
		return runtimeapi.SkillsBundleResponse{}, ctx.Err()
	})
	if _, err := a.StartOrResume(t.Context(), harnesses.RecoveryDescriptor{}); err != nil {
		t.Fatal(err)
	}
	turnCtx, cancelTurn := context.WithCancel(t.Context())
	defer cancelTurn()
	turnDone := make(chan struct{})
	go func() {
		defer close(turnDone)
		_, _ = a.Deliver(turnCtx, harnesses.Delivery{})
	}()
	select {
	case <-fetching:
	case <-time.After(5 * time.Second):
		t.Fatal("refresh never started")
	}
	stopCtx, cancelStop := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancelStop()
	stopDone := make(chan struct{})
	go func() { _ = a.Stop(stopCtx); close(stopDone) }()
	select {
	case <-stopDone:
	case <-time.After(3 * time.Second):
		cancelTurn()
		<-stopDone
		t.Fatal("Stop waited for skill fetch without cancelling it")
	}
	<-turnDone
}
