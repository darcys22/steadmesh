package all

import (
	"os"
	"regexp"
	"testing"

	"github.com/darcys22/steadmesh/harnesses/claudecode"
	"github.com/darcys22/steadmesh/harnesses/codex"
	"github.com/darcys22/steadmesh/harnesses/pi"
)

// TestImagePinsMatchAdapters keeps each seat image's pinned harness version
// equal to the version its adapter was verified against.
func TestImagePinsMatchAdapters(t *testing.T) {
	for file, c := range map[string]struct{ arg, want string }{
		"../../build/seat-claudecode.Dockerfile": {"CLAUDE_CODE_VERSION", claudecode.PinnedVersion},
		"../../build/seat-codex.Dockerfile":      {"CODEX_VERSION", codex.PinnedVersion},
		"../../build/seat-pi.Dockerfile":         {"PI_VERSION", pi.PinnedVersion},
		"../../build/harness-bins.sh":            {"", ""},
	} {
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if c.arg == "" {
			for v, want := range map[string]string{"CLAUDE_VERSION": claudecode.PinnedVersion, "CODEX_VERSION": codex.PinnedVersion, "PI_VERSION": pi.PinnedVersion} {
				if m := regexp.MustCompile(`(?m)^` + v + `=(\S+)$`).FindSubmatch(b); m == nil || string(m[1]) != want {
					t.Errorf("%s: %s=%s, adapter pins %s", file, v, m, want)
				}
			}
			continue
		}
		m := regexp.MustCompile(`(?m)^ARG ` + c.arg + `=(\S+)$`).FindSubmatch(b)
		if m == nil || string(m[1]) != c.want {
			t.Errorf("%s: ARG %s=%s, adapter pins %s", file, c.arg, m, c.want)
		}
	}
}
