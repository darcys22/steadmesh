package taskworker

import (
	"github.com/darcys22/steadmesh/pkg/compile"
	"github.com/darcys22/steadmesh/pkg/spec"
	"testing"
)

func TestWorkerManifestNeverWidensParent(t *testing.T) {
	p := compile.SeatManifest{IsRepresentative: true, PersonalMemory: "private", Workspace: spec.WorkspaceBinding{Persistent: true, Shared: []string{"repo"}}, Harness: spec.HarnessProfile{Model: &spec.ModelSelection{Connection: "model", ID: "pinned"}}, Capabilities: []compile.Capability{{Resource: "memory:a", Operations: []string{"read", "write", "search", "history"}}, {Resource: "connection:model", Operations: []string{"model.infer"}}, {Resource: "connection:other", Operations: []string{"model.infer"}}, {Resource: "workspace:shared", Operations: []string{"write"}}}}
	none := Manifest(p, nil)
	if len(none.Capabilities) != 1 || none.Capabilities[0].Resource != "connection:model" || none.IsRepresentative || none.Access != nil || none.Workspace.Persistent || len(none.Workspace.Shared) > 0 {
		t.Fatalf("default authority: %+v", none)
	}
	read := Manifest(p, []string{"memory.read"})
	if len(read.Capabilities) != 2 || len(read.Capabilities[0].Operations) != 3 || p.Capabilities[0].Operations[1] != "write" {
		t.Fatalf("intersection modified/widened parent: %+v", read)
	}
}

func TestManifestDoesNotInheritMarketplaceSkills(t *testing.T) {
	parent := compile.SeatManifest{Skills: &spec.SkillPolicy{Mode: spec.SkillModeAgentChoice, Marketplaces: []string{"approved"}}, SkillMarketplaces: map[string]spec.SkillMarketplace{"approved": {Repository: "owner/repo"}}}
	worker := Manifest(parent, []string{"skills.list", "skills.install"})
	if worker.Skills != nil || worker.SkillMarketplaces != nil {
		t.Fatal("worker inherited persistent skill authority")
	}
	if parent.Skills == nil || len(parent.SkillMarketplaces) != 1 {
		t.Fatal("parent manifest mutated")
	}
}
