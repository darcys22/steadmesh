package compile_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/darcys22/steadmesh/pkg/compile"
	"github.com/darcys22/steadmesh/pkg/spec"
)

func TestWorkPublicationValidation(t *testing.T) {
	base, _ := load(t, filepath.Join(fixtures, "valid", "representative_and_worker.yaml"))
	var tracker string
	for k, c := range base.Connections {
		if c.Adapter == "linear" {
			tracker = k
		}
	}
	if tracker == "" {
		t.Fatal("fixture has no tracker connection")
	}
	for name, tc := range map[string]struct {
		pub  spec.WorkPublication
		want string
	}{
		"valid":              {spec.WorkPublication{Connection: tracker, Stores: []string{"engineering"}}, ""},
		"unknown connection": {spec.WorkPublication{Connection: "nope", Stores: []string{"engineering"}}, "is not declared"},
		"not a tracker":      {spec.WorkPublication{Connection: "slack", Stores: []string{"engineering"}}, "is not a work tracker"},
		"no stores":          {spec.WorkPublication{Connection: tracker}, "at least one"},
		"unknown store":      {spec.WorkPublication{Connection: tracker, Stores: []string{"missing"}}, "is not declared"},
		"personal store":     {spec.WorkPublication{Connection: tracker, Stores: []string{"rep_sean"}}, "personal memory is never published"},
	} {
		t.Run(name, func(t *testing.T) {
			s := base
			s.WorkPublication = &tc.pub
			_, err := compile.Compile(s, compile.DefaultCatalog())
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("rejected: %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("error %v, want %q", err, tc.want)
			}
		})
	}
}

// Compile records the role-based default, so everything reading the
// manifest (controller, console, provider outputs) sees the same answer.
func TestCompileDefaultsRequiredByRole(t *testing.T) {
	m := compileValid(t)
	for k, c := range m.Spec.Connections {
		if c.Required == nil {
			t.Fatalf("%s: required not defaulted", k)
		}
	}
	if *m.Spec.Connections["tracker"].Required {
		t.Error("an undeclared tracker defaulted to required")
	}
	if !*m.Spec.Connections["slack"].Required {
		t.Error("the bound communication connection is not required")
	}
	got := compile.RequiredConnections(m, compile.DefaultCatalog())
	if got["tracker"] || !got["slack"] {
		t.Errorf("RequiredConnections = %v", got)
	}
}
