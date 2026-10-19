package policy

import (
	"github.com/darcys22/steadmesh/pkg/compile"
	"github.com/darcys22/steadmesh/pkg/spec"
	"testing"
)

func TestTeamSubjectsNeverGrantByRole(t *testing.T) {
	sm := compile.SeatManifest{Key: "worker", DisplayName: "CEO", Teams: []string{"a"}}
	org := compile.Manifest{Spec: spec.OrganizationSpec{Teams: map[string]spec.Team{"a": {Coordination: &spec.TeamCoordination{MissionEditors: []string{"seat:ceo"}, MaxMessagesPerExecution: 3}}, "b": {Coordination: &spec.TeamCoordination{MissionEditors: []string{"team:b"}}}}}}
	if CanEditMission(&sm, &org, "a") || CanEditMission(&sm, &org, "b") {
		t.Fatal("role or outside team conferred authority")
	}
	a := org.Spec.Teams["a"]
	a.Coordination.MissionEditors = []string{"team:a"}
	org.Spec.Teams["a"] = a
	if !CanEditMission(&sm, &org, "a") {
		t.Fatal("explicit team editor denied")
	}
	if MessageLimit(&sm, &org) != 3 {
		t.Fatal("bound not resolved")
	}
	sm.Teams = nil
	if MessageLimit(&sm, &org) != 0 {
		t.Fatal("legacy behavior changed")
	}
}
