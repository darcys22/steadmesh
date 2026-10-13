package runtimeapi

import (
	"strings"
	"testing"
)

func TestTaskRunBounds(t *testing.T) {
	for _, p := range []string{"../secret", "/etc/passwd", "a/../b", "a\\b", ".git/config", "src/.GiT/config", "."} {
		if ValidTaskPath(p) {
			t.Errorf("accepted %q", p)
		}
	}
	for _, p := range []string{"README.md", "src/main.go", "nested/file.txt"} {
		if !ValidTaskPath(p) {
			t.Errorf("rejected %q", p)
		}
	}
	cases := []TaskRunRequest{{Prompt: ""}, {Prompt: strings.Repeat("x", MaxTaskPromptBytes+1)}, {Prompt: "x", TimeoutSeconds: 1201}, {Prompt: "x", ToolNames: []string{"tasks.start"}}, {Prompt: "x", ToolNames: []string{"connections.invoke"}}, {Prompt: "x", InputFiles: []TaskFile{{Path: "a", Content: "x"}, {Path: "a/b", Content: "y"}}}, {Prompt: "x", InputFiles: []TaskFile{{Path: "a", Content: strings.Repeat("x", MaxTaskFileBytes)}}}, {Prompt: "x", ArtifactPaths: []string{"a", "a"}}}
	for _, c := range cases {
		if ValidateTaskRun(&c) == nil {
			t.Errorf("accepted invalid task %+v", c)
		}
	}
	good := TaskRunRequest{Prompt: "review", ToolNames: []string{"memory.read"}, InputFiles: []TaskFile{{Path: "input.txt", Content: "hello"}}}
	if err := ValidateTaskRun(&good); err != nil || good.TimeoutSeconds != 600 {
		t.Fatalf("good task: %+v %v", good, err)
	}
}
