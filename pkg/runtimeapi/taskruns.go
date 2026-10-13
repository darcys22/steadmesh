package runtimeapi

import (
	"fmt"
	"path"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	PathTaskRuns            = "/v1/task-runs"
	PathTaskWorker          = "/v1/task-worker"
	MaxTaskFiles            = 64
	MaxTaskFileBytes        = 256 << 10
	MaxTaskOutputBytes      = 16 << 10
	MaxTaskPromptBytes      = 32 << 10
	MaxActiveTaskRuns       = 4
	MaxTaskRunsPerExecution = 16
	TaskPending             = "pending"
	TaskRunning             = "running"
	TaskCompleted           = "completed"
	TaskFailed              = "failed"
	TaskCancelled           = "cancelled"
	TaskTimedOut            = "timed_out"
)

// TaskFile is an explicit, bounded UTF-8 file. Paths are relative to a fresh
// workspace, never the persistent seat's volume. Binary artifacts are not v1.
type TaskFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// TaskRunRequest cannot select a new model, identity, runtime or credential.
// ToolNames narrows existing read-only memory grants; omitted means none.
type TaskRunRequest struct {
	RequestID      string     `json:"request_id"`
	Prompt         string     `json:"prompt"`
	TimeoutSeconds int        `json:"timeout_seconds,omitempty"`
	ToolNames      []string   `json:"tool_names,omitempty"`
	InputFiles     []TaskFile `json:"input_files,omitempty"`
	ArtifactPaths  []string   `json:"artifact_paths,omitempty"`
}

type TaskRunResult struct {
	Output    string     `json:"output,omitempty"`
	Artifacts []TaskFile `json:"artifacts,omitempty"`
	Error     string     `json:"error,omitempty"`
}

// TaskRun is durable after its disposable runtime has been removed. Technical
// completion is not approval of the agent's output or permission to apply it.
type TaskRun struct {
	ID                string         `json:"id"`
	ParentSeatID      string         `json:"parent_seat_id"`
	ParentExecutionID string         `json:"parent_execution_id"`
	ParentGeneration  int64          `json:"parent_generation"`
	ConfigRevision    string         `json:"config_revision"`
	ServiceAccount    string         `json:"service_account"`
	PodUID            string         `json:"pod_uid,omitempty"`
	State             string         `json:"state"`
	Request           TaskRunRequest `json:"request"`
	Result            *TaskRunResult `json:"result,omitempty"`
	CreatedAt         time.Time      `json:"created_at"`
	Deadline          time.Time      `json:"deadline"`
	FinishedAt        *time.Time     `json:"finished_at,omitempty"`
	CleanedAt         *time.Time     `json:"cleaned_at,omitempty"`
}

func (r TaskRun) Terminal() bool {
	return r.State == TaskCompleted || r.State == TaskFailed || r.State == TaskCancelled || r.State == TaskTimedOut
}

type TaskRunFinish struct {
	State  string        `json:"state"`
	Result TaskRunResult `json:"result"`
}

// ValidTaskPath rejects traversal, hidden Git metadata, OS-specific separators
// and file/directory ambiguity. A task never receives a repository's .git.
func ValidTaskPath(p string) bool {
	if p == "" || len(p) > 240 || p != path.Clean(p) || strings.HasPrefix(p, "/") || strings.ContainsAny(p, "\\\x00\r\n") {
		return false
	}
	for _, s := range strings.Split(p, "/") {
		if s == ".." || s == "." || strings.EqualFold(s, ".git") {
			return false
		}
	}
	return true
}

func ValidateTaskFiles(files []TaskFile) error {
	if len(files) > MaxTaskFiles {
		return fmt.Errorf("at most %d files are allowed", MaxTaskFiles)
	}
	total := 0
	seen := map[string]bool{}
	for _, f := range files {
		if !ValidTaskPath(f.Path) || !utf8.ValidString(f.Content) {
			return fmt.Errorf("invalid UTF-8 file or relative path %q", f.Path)
		}
		if seen[f.Path] {
			return fmt.Errorf("duplicate file %q", f.Path)
		}
		seen[f.Path] = true
		total += len(f.Content) + len(f.Path)
	}
	if total > MaxTaskFileBytes {
		return fmt.Errorf("files exceed %d bytes", MaxTaskFileBytes)
	}
	for p := range seen {
		for q := path.Dir(p); q != "."; q = path.Dir(q) {
			if seen[q] {
				return fmt.Errorf("file/directory collision at %q", q)
			}
		}
	}
	return nil
}

func ValidateTaskRun(req *TaskRunRequest) error {
	if strings.TrimSpace(req.Prompt) == "" || len(req.Prompt) > MaxTaskPromptBytes {
		return fmt.Errorf("prompt is required and limited to %d bytes", MaxTaskPromptBytes)
	}
	if req.TimeoutSeconds == 0 {
		req.TimeoutSeconds = 600
	}
	if req.TimeoutSeconds < 1 || req.TimeoutSeconds > 1200 {
		return fmt.Errorf("timeout_seconds must be between 1 and 1200")
	}
	if len(req.ToolNames) > 8 {
		return fmt.Errorf("too many tools")
	}
	for _, n := range req.ToolNames {
		switch n {
		case "memory.stores", "memory.list", "memory.read", "memory.search", "memory.history", "work.get", "work.list":
		default:
			return fmt.Errorf("tool %q is not delegable", n)
		}
	}
	if err := ValidateTaskFiles(req.InputFiles); err != nil {
		return err
	}
	if len(req.ArtifactPaths) > MaxTaskFiles {
		return fmt.Errorf("too many artifact paths")
	}
	seen := map[string]bool{}
	for _, p := range req.ArtifactPaths {
		if !ValidTaskPath(p) || seen[p] {
			return fmt.Errorf("invalid or duplicate artifact path %q", p)
		}
		seen[p] = true
	}
	return nil
}
