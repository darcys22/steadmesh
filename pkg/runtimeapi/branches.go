package runtimeapi

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

const MaxRunAlternatives = 4

// RunBranchRequest starts independent alternatives from explicitly supplied
// context. It never reads a parent workspace, checkpoint, environment or secret.
// The request and each child's input are immutable after creation.
type RunBranchRequest struct {
	RequestID      string           `json:"request_id"`
	Handoff        string           `json:"handoff"`
	InputFiles     []TaskFile       `json:"input_files,omitempty"`
	ArtifactPaths  []string         `json:"artifact_paths,omitempty"`
	TimeoutSeconds int              `json:"timeout_seconds,omitempty"`
	Alternatives   []RunAlternative `json:"alternatives"`
}

type RunAlternative struct {
	Name   string `json:"name"`
	Prompt string `json:"prompt"`
}

// RunBranchSelection records a deliberate choice, never a merge or file write.
// The digest binds the choice to the complete, immutable result reviewed.
type RunBranchSelection struct {
	TaskRunID    string    `json:"task_run_id"`
	ResultSHA256 string    `json:"result_sha256"`
	SelectedAt   time.Time `json:"selected_at"`
}

type RunBranchSelectRequest struct {
	ID                   string `json:"id"`
	TaskRunID            string `json:"task_run_id"`
	ExpectedRevision     int    `json:"expected_revision"`
	ExpectedResultSHA256 string `json:"expected_result_sha256"`
}

// RunBranchComparison is bounded metadata and result previews. tasks.get and
// the task-run API retrieve the full per-alternative inputs and results.
type RunBranchComparison struct {
	ID                string                `json:"id"`
	RequestID         string                `json:"request_id"`
	ParentExecutionID string                `json:"parent_execution_id"`
	SnapshotSHA256    string                `json:"snapshot_sha256"`
	InputFileCount    int                   `json:"input_file_count"`
	Revision          int                   `json:"revision"`
	AllTerminal       bool                  `json:"all_terminal"`
	Alternatives      []RunAlternativeState `json:"alternatives"`
	Selection         *RunBranchSelection   `json:"selection,omitempty"`
	CreatedAt         time.Time             `json:"created_at"`
}

type RunAlternativeState struct {
	Name            string     `json:"name"`
	TaskRunID       string     `json:"task_run_id"`
	State           string     `json:"state"`
	OutputPreview   string     `json:"output_preview,omitempty"`
	OutputTruncated bool       `json:"output_truncated,omitempty"`
	ErrorPreview    string     `json:"error_preview,omitempty"`
	ErrorTruncated  bool       `json:"error_truncated,omitempty"`
	ResultSHA256    string     `json:"result_sha256,omitempty"`
	ArtifactCount   int        `json:"artifact_count"`
	ArtifactBytes   int        `json:"artifact_bytes"`
	FinishedAt      *time.Time `json:"finished_at,omitempty"`
}

// BranchTaskRequest uses only explicit handoff/files and an alternative's
// prompt. No platform tools are delegated, and no parent session is resumed.
func BranchTaskRequest(r RunBranchRequest, a RunAlternative, requestID string) TaskRunRequest {
	return TaskRunRequest{
		RequestID:      requestID,
		Prompt:         "Shared handoff (explicit snapshot):\n" + r.Handoff + "\n\nAlternative " + a.Name + ":\n" + a.Prompt,
		TimeoutSeconds: r.TimeoutSeconds,
		InputFiles:     append([]TaskFile(nil), r.InputFiles...),
		ArtifactPaths:  append([]string(nil), r.ArtifactPaths...),
	}
}

// ValidateRunBranch normalizes defaults before idempotency hashing. The task
// validator remains the single authority for files, paths and timeout limits.
func ValidateRunBranch(r *RunBranchRequest) error {
	if strings.TrimSpace(r.Handoff) == "" || len(r.Handoff) > 8<<10 || !utf8.ValidString(r.Handoff) {
		return fmt.Errorf("handoff is required and limited to 8192 UTF-8 bytes")
	}
	if len(r.Alternatives) < 2 || len(r.Alternatives) > MaxRunAlternatives {
		return fmt.Errorf("between 2 and %d alternatives are required", MaxRunAlternatives)
	}
	seen := map[string]bool{}
	for _, a := range r.Alternatives {
		if strings.TrimSpace(a.Name) != a.Name || a.Name == "" || len(a.Name) > 64 || strings.ContainsAny(a.Name, "\r\n\x00") || !utf8.ValidString(a.Name) {
			return fmt.Errorf("alternative names must be 1–64 UTF-8 bytes without surrounding whitespace or line breaks")
		}
		key := strings.ToLower(a.Name)
		if seen[key] {
			return fmt.Errorf("duplicate alternative name %q", a.Name)
		}
		seen[key] = true
		if strings.TrimSpace(a.Prompt) == "" || len(a.Prompt) > 8<<10 || !utf8.ValidString(a.Prompt) {
			return fmt.Errorf("each alternative prompt is required and limited to 8192 UTF-8 bytes")
		}
		task := BranchTaskRequest(*r, a, "")
		if err := ValidateTaskRun(&task); err != nil {
			return err
		}
		r.TimeoutSeconds = task.TimeoutSeconds
	}
	return nil
}

// BranchSnapshotSHA256 identifies exactly the supplied shared handoff/files.
// Alternative-specific instructions are deliberately outside this snapshot.
func BranchSnapshotSHA256(r RunBranchRequest) string {
	return branchDigest(struct {
		Handoff string     `json:"handoff"`
		Files   []TaskFile `json:"files,omitempty"`
	}{r.Handoff, r.InputFiles})
}

func TaskResultSHA256(r *TaskRunResult) string { return branchDigest(r) }

func branchDigest(v any) string {
	b, _ := json.Marshal(v) // only concrete string/integer structs reach here
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
