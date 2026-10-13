package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/darcys22/steadmesh/pkg/compile"
	"github.com/darcys22/steadmesh/pkg/runtimeapi"
	"github.com/darcys22/steadmesh/pkg/spec"
	"github.com/darcys22/steadmesh/pkg/taskworker"
)

func TestTaskWorkerFilesRejectEscapeAndOversize(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeTaskInputs(dir, []runtimeapi.TaskFile{{Path: "../escape", Content: "x"}}); err == nil {
		t.Fatal("accepted traversal")
	}
	if err := os.Symlink(outside, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := readTaskArtifacts(dir, []string{"link/secret"}); err == nil {
		t.Fatal("read outside workspace")
	}
	if err := os.WriteFile(filepath.Join(dir, "large"), []byte(strings.Repeat("x", runtimeapi.MaxTaskFileBytes+1)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readTaskArtifacts(dir, []string{"large"}); err == nil {
		t.Fatal("accepted oversized artifact")
	}
}
func TestTaskWorkerRunsFreshAndReturnsArtifactsWithoutSeatLease(t *testing.T) {
	root := t.TempDir()
	token := filepath.Join(root, "token")
	_ = os.WriteFile(token, []byte("worker-token"), 0600)
	var got runtimeapi.TaskRunFinish
	calls := []string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.URL.Path)
		switch r.URL.Path {
		case runtimeapi.PathTaskWorker + "/claim":
			_ = json.NewEncoder(w).Encode(taskworker.Bootstrap{Run: runtimeapi.TaskRun{ID: "run", ParentGeneration: 2, ParentExecutionID: "parent", Deadline: time.Now().Add(time.Minute), Request: runtimeapi.TaskRunRequest{Prompt: "/file read input.txt\n/file write result.txt answer", InputFiles: []runtimeapi.TaskFile{{Path: "input.txt", Content: "question"}}, ArtifactPaths: []string{"result.txt"}}}, Manifest: compile.SeatManifest{Harness: spec.HarnessProfile{Adapter: "fake"}}})
		case runtimeapi.PathTaskWorker + "/finish":
			_ = json.NewDecoder(r.Body).Decode(&got)
			w.WriteHeader(204)
		default:
			t.Errorf("worker touched persistent seat endpoint: %s", r.URL.Path)
			w.WriteHeader(403)
		}
	}))
	defer srv.Close()
	cfg := Config{PlatformURL: srv.URL, TokenFile: token, ToolCommand: "/bin/false", WorkspaceDir: filepath.Join(root, "workspace"), HomeDir: filepath.Join(root, "home"), RunnerDir: filepath.Join(root, "runner"), TmpDir: filepath.Join(root, "tmp")}
	r, err := NewRunner(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), DefaultAdapters)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.runTaskWorker(context.Background(), "run"); err != nil {
		t.Fatal(err)
	}
	if got.State != runtimeapi.TaskCompleted || len(got.Result.Artifacts) != 1 || got.Result.Artifacts[0].Content != "answer" || !strings.Contains(got.Result.Output, "question") {
		t.Fatalf("result: %+v", got)
	}
	if len(calls) != 2 {
		t.Fatalf("calls: %v", calls)
	}
}
