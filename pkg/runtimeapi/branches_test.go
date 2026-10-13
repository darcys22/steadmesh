package runtimeapi

import (
	"encoding/json"
	"strings"
	"testing"
)

func validBranch() RunBranchRequest {
	return RunBranchRequest{RequestID: "00000000-0000-4000-8000-000000000001", Handoff: "Build a parser; preserve existing semantics.",
		InputFiles: []TaskFile{{Path: "input.txt", Content: "fixture"}}, ArtifactPaths: []string{"result.txt"},
		Alternatives: []RunAlternative{{Name: "simple", Prompt: "Use a direct parser"}, {Name: "table", Prompt: "Use a lookup table"}}}
}

func TestRunBranchValidationAndIsolation(t *testing.T) {
	r := validBranch()
	if err := ValidateRunBranch(&r); err != nil || r.TimeoutSeconds != 600 {
		t.Fatalf("validation: %+v %v", r, err)
	}
	a := BranchTaskRequest(r, r.Alternatives[0], "child-a")
	b := BranchTaskRequest(r, r.Alternatives[1], "child-b")
	if a.RequestID == b.RequestID || a.Prompt == b.Prompt || len(a.ToolNames) != 0 || len(b.ToolNames) != 0 {
		t.Fatalf("independent tasks: %+v %+v", a, b)
	}
	a.InputFiles[0].Content = "modified"
	a.ArtifactPaths[0] = "different.txt"
	if b.InputFiles[0].Content != "fixture" || r.InputFiles[0].Content != "fixture" || b.ArtifactPaths[0] != "result.txt" {
		t.Fatal("alternative inputs alias the shared snapshot")
	}
}

func TestRunBranchRejectsInvalidRequests(t *testing.T) {
	cases := map[string]func(*RunBranchRequest){
		"empty handoff":   func(r *RunBranchRequest) { r.Handoff = " " },
		"large handoff":   func(r *RunBranchRequest) { r.Handoff = strings.Repeat("x", 8193) },
		"invalid UTF8":    func(r *RunBranchRequest) { r.Handoff = string([]byte{255}) },
		"one alternative": func(r *RunBranchRequest) { r.Alternatives = r.Alternatives[:1] },
		"five alternatives": func(r *RunBranchRequest) {
			r.Alternatives = append(r.Alternatives, r.Alternatives...)
			r.Alternatives = append(r.Alternatives, RunAlternative{Name: "extra", Prompt: "x"})
		},
		"duplicate name":     func(r *RunBranchRequest) { r.Alternatives[1].Name = "SIMPLE" },
		"blank name":         func(r *RunBranchRequest) { r.Alternatives[0].Name = "" },
		"surrounding space":  func(r *RunBranchRequest) { r.Alternatives[0].Name = " simple" },
		"line break":         func(r *RunBranchRequest) { r.Alternatives[0].Name = "simple\nnext" },
		"blank prompt":       func(r *RunBranchRequest) { r.Alternatives[0].Prompt = " " },
		"large prompt":       func(r *RunBranchRequest) { r.Alternatives[0].Prompt = strings.Repeat("x", 8193) },
		"traversal":          func(r *RunBranchRequest) { r.InputFiles[0].Path = "../secret" },
		"git metadata":       func(r *RunBranchRequest) { r.InputFiles[0].Path = ".git/config" },
		"artifact traversal": func(r *RunBranchRequest) { r.ArtifactPaths[0] = "../result" },
		"negative timeout":   func(r *RunBranchRequest) { r.TimeoutSeconds = -1 },
		"long timeout":       func(r *RunBranchRequest) { r.TimeoutSeconds = 1201 },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			r := validBranch()
			change(&r)
			if err := ValidateRunBranch(&r); err == nil {
				t.Fatal("accepted invalid request")
			}
		})
	}
}

func TestBranchDigestsBindSharedInputAndFullResults(t *testing.T) {
	r := validBranch()
	first := BranchSnapshotSHA256(r)
	r.Alternatives[0].Prompt = "a different approach"
	if BranchSnapshotSHA256(r) != first {
		t.Fatal("alternative changed the shared snapshot digest")
	}
	r.InputFiles[0].Content += " changed"
	if BranchSnapshotSHA256(r) == first {
		t.Fatal("file edit did not change digest")
	}
	result := &TaskRunResult{Output: "done", Artifacts: []TaskFile{{Path: "result.txt", Content: "one"}}}
	digest := TaskResultSHA256(result)
	result.Artifacts[0].Content = "two"
	if TaskResultSHA256(result) == digest {
		t.Fatal("artifact content was not hashed")
	}
	b, _ := json.Marshal(result)
	var roundtrip TaskRunResult
	_ = json.Unmarshal(b, &roundtrip)
	if TaskResultSHA256(&roundtrip) != TaskResultSHA256(result) {
		t.Fatal("digest changed on JSON round trip")
	}
}
