package provider

import (
	"context"
	"reflect"
	"testing"

	"github.com/darcys22/steadmesh/pkg/spec"
)

func TestNativeTeamsRoundTrip(t *testing.T) {
	s := loadFixture(t)
	seat := s.Seats["reviewer"]
	seat.PersonalMemory = ""
	s.Teams["native"] = spec.Team{Coordination: &spec.TeamCoordination{Mode: "self_organising", Mission: "Build", MissionEditors: []string{"team:native"}, MaxMessagesPerExecution: 20}, Context: &spec.TeamContext{EditableInstructions: true, EditableSummary: true, RequireInstructionApproval: true, InstructionApprovers: []string{"team:native"}}, Pool: &spec.TeamPool{MinMembers: 1, MaxMembers: 4, IdleTimeoutSeconds: 900, DrainTimeoutSeconds: 300, RetentionSeconds: 86400, Draining: true, Templates: map[string]spec.WorkerTemplate{"builder": {Instructions: "Build", Seat: &seat}}}}

	for _, explicit := range []bool{true, false} {
		if !explicit {
			team := s.Teams["native"]
			team.Context = nil
			team.Coordination = nil
			s.Teams["native"] = team
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
		if !reflect.DeepEqual(s.Teams, got.Teams) {
			t.Fatalf("policy changed: %#v", got.Teams)
		}
	}
}
