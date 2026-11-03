package all

import (
	"github.com/darcys22/steadmesh/harnesses/claudecode"
	"github.com/darcys22/steadmesh/harnesses/codex"
	"github.com/darcys22/steadmesh/harnesses/pi"
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestBundlePinsAndSecurityEnvelope(t *testing.T) {
	b, err := os.ReadFile("../../build/seat-bundle.Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	for name, want := range map[string]string{"CODEX_VERSION": codex.PinnedVersion, "CLAUDE_CODE_VERSION": claudecode.PinnedVersion, "PI_VERSION": pi.PinnedVersion} {
		match := regexp.MustCompile(`(?m)^ARG ` + name + `=(\S+)$`).FindStringSubmatch(text)
		if len(match) != 2 || match[1] != want {
			t.Fatalf("bundle pin %s must equal %s", name, want)
		}
	}
	for _, required := range []string{"USER 1000:1000", "DISABLE_AUTOUPDATER=1", "PI_OFFLINE=1", "STEADMESH_TOKEN_FILE=/var/run/steadmesh/token", "COPY build/gh-wrapper.sh"} {
		if !strings.Contains(text, required) {
			t.Fatalf("bundle missing security envelope: %s", required)
		}
	}
}
