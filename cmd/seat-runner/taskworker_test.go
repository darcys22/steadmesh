package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/darcys22/steadmesh/harnesses"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/darcys22/steadmesh/pkg/compile"
	"github.com/darcys22/steadmesh/pkg/runtimeapi"
	"github.com/darcys22/steadmesh/pkg/spec"
	"github.com/darcys22/steadmesh/pkg/taskworker"
)

// failingTaskWorkerTool is a test-owned replacement for the platform tool bridge.
// These task-worker cases must not make tool calls. Keep accidental calls failing
// without depending on false's location (/usr/bin on macOS, /bin on some Linuxes).
func failingTaskWorkerTool(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "unexpected-tool-call")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestFailingTaskWorkerTool(t *testing.T) {
	tool := failingTaskWorkerTool(t)
	// The fixture must not rely on an external false executable or PATH.
	cmd := exec.Command(tool, "call", "unexpected", "{}")
	cmd.Env = []string{"PATH="}
	var exitErr *exec.ExitError
	if err := cmd.Run(); !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
		t.Fatalf("expected failing tool exit 1, got %v", err)
	}
}

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
	cfg := Config{PlatformURL: srv.URL, TokenFile: token, ToolCommand: failingTaskWorkerTool(t), WorkspaceDir: filepath.Join(root, "workspace"), HomeDir: filepath.Join(root, "home"), RunnerDir: filepath.Join(root, "runner"), TmpDir: filepath.Join(root, "tmp")}
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

type structuredWorkerAdapter struct {
	harnesses.Adapter
	output string
}

func (a *structuredWorkerAdapter) Deliver(_ context.Context, d harnesses.Delivery) (harnesses.TurnResult, error) {
	if len(d.OutputSchema) == 0 {
		return harnesses.TurnResult{}, fmt.Errorf("schema not propagated")
	}
	return harnesses.TurnResult{Status: harnesses.TurnCompleted, Output: a.output}, nil
}

func TestTaskWorkerStructuredOutput(t *testing.T) {
	for _, tc := range []struct {
		name, output string
		success      bool
	}{
		{"valid-negative", `{"accepted":false}`, true},
		{"wrong-type", `{"accepted":"yes"}`, false},
		{"oversized", `{"accepted":true,"extra":"` + strings.Repeat("x", runtimeapi.MaxTaskOutputBytes) + `"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			token := filepath.Join(root, "token")
			if err := os.WriteFile(token, []byte("worker-token"), 0600); err != nil {
				t.Fatal(err)
			}
			var got runtimeapi.TaskRunFinish
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case runtimeapi.PathTaskWorker + "/claim":
					_ = json.NewEncoder(w).Encode(taskworker.Bootstrap{Run: runtimeapi.TaskRun{ID: "run", Deadline: time.Now().Add(time.Minute), Request: runtimeapi.TaskRunRequest{Prompt: "/output " + tc.output, OutputSchema: []byte(`{"type":"object","properties":{"accepted":{"type":"boolean"}},"required":["accepted"],"additionalProperties":false}`)}}, Manifest: compile.SeatManifest{Harness: spec.HarnessProfile{Adapter: "fake"}}})
				case runtimeapi.PathTaskWorker + "/finish":
					_ = json.NewDecoder(r.Body).Decode(&got)
					w.WriteHeader(204)
				default:
					t.Errorf("unexpected endpoint %s", r.URL.Path)
					w.WriteHeader(403)
				}
			}))
			defer srv.Close()
			cfg := Config{PlatformURL: srv.URL, TokenFile: token, ToolCommand: failingTaskWorkerTool(t), WorkspaceDir: filepath.Join(root, "workspace"), HomeDir: filepath.Join(root, "home"), RunnerDir: filepath.Join(root, "runner"), TmpDir: filepath.Join(root, "tmp")}
			factory := func(name string) (harnesses.Adapter, error) {
				a, err := DefaultAdapters(name)
				if tc.name != "oversized" {
					return a, err
				}
				return &structuredWorkerAdapter{Adapter: a, output: tc.output}, err
			}
			r, err := NewRunner(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), factory)
			if err != nil {
				t.Fatal(err)
			}
			err = r.runTaskWorker(context.Background(), "run")
			if tc.success {
				if err != nil || got.State != runtimeapi.TaskCompleted || string(got.Result.StructuredOutput) != tc.output {
					t.Fatalf("%+v %v", got, err)
				}
			} else if err == nil || got.State != runtimeapi.TaskFailed || got.Result.ErrorKind != runtimeapi.StructuredOutputInvalid || len(got.Result.StructuredOutput) > 0 {
				t.Fatalf("%+v %v", got, err)
			}
		})
	}
}
