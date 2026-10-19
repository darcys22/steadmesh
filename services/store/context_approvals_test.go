package store

import (
	"github.com/darcys22/steadmesh/pkg/compile"
	"github.com/darcys22/steadmesh/pkg/spec"
	"testing"
)

func TestContextReviewerKeepsOriginatingTeamScope(t *testing.T) {
	p := &spec.TeamContext{EditableInstructions: true, RequireInstructionApproval: true, InstructionApprovers: []string{"seat:reviewer"}}
	owner := &Seat{ID: "owner", OrganizationID: "org", Manifest: compile.SeatManifest{Key: "owner", Teams: []string{"restricted", "unrelated"}, Context: p}}
	reviewer := &Seat{ID: "reviewer", OrganizationID: "org", Manifest: compile.SeatManifest{Key: "reviewer", Teams: []string{"unrelated"}}}
	org := &compile.Manifest{Spec: spec.OrganizationSpec{Teams: map[string]spec.Team{"restricted": {Context: p}, "unrelated": {}}}}
	if contextReviewer(owner, reviewer, org) {
		t.Fatal("unrelated shared team activated restricted reviewer selector")
	}
	reviewer.Manifest.Teams = append(reviewer.Manifest.Teams, "restricted")
	if !contextReviewer(owner, reviewer, org) {
		t.Fatal("explicit same-team reviewer denied")
	}
	p.InstructionApprovers = []string{"team:restricted"}
	if contextReviewer(owner, owner, org) {
		t.Fatal("broad team selector enabled self approval")
	}
	reviewer.OrganizationID = "other"
	if contextReviewer(owner, reviewer, org) {
		t.Fatal("cross-organization reviewer")
	}
}
