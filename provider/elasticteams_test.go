package provider

import (
	"context"
	"reflect"
	"testing"

	"github.com/darcys22/steadmesh/pkg/spec"
)

func TestElasticTeamsRoundTrip(t *testing.T) {
	s := loadFixture(t)
	s.ElasticTeams = &spec.ElasticTeams{EnabledSeats: []string{"reviewer"}, MaxActiveWorkers: 20, MaxActivePerExecution: 10, MaxLaunchesPerExecution: 20, MaxTimeoutSeconds: 600, Draining: true, Templates: map[string]spec.WorkerTemplate{"researcher": {Instructions: "Research", ToolNames: []string{"memory.read"}, HarnessProfile: "fake", ExecutionProfile: "interactive"}}}
	for _, explicit := range []bool{true, false} {
		if !explicit {
			s.ElasticTeams.Draining = false
			w := s.ElasticTeams.Templates["researcher"]
			w.HarnessProfile = ""
			w.ExecutionProfile = ""
			w.ToolNames = nil
			s.ElasticTeams.Templates["researcher"] = w
		}
		d, err := declarationFromSpec(s)
		if err != nil {
			t.Fatal(err)
		}
		v, err := treeToAttr(context.Background(), specType(context.Background()), d.Spec)
		if err != nil {
			t.Fatal(err)
		}
		tv, err := v.ToTerraformValue(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		tree, err := valueToTree(tv)
		if err != nil {
			t.Fatal(err)
		}
		d.Spec = tree
		got, err := d.toOrganizationSpec()
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(s.ElasticTeams, got.ElasticTeams) {
			t.Fatalf("policy changed: %#v", got.ElasticTeams)
		}
	}
}
