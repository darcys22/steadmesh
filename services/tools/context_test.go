package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/darcys22/steadmesh/pkg/compile"
	"github.com/darcys22/steadmesh/pkg/spec"
	"github.com/darcys22/steadmesh/services/store"
)

func TestContextPermissionsAreIndependent(t *testing.T) {
	c := &Call{Seat: &store.Seat{Manifest: compile.SeatManifest{Context: &spec.TeamContext{EditableSummary: true}}}}
	if err := contextFieldAllowed(c, "instructions"); err == nil {
		t.Fatal("fixed prompt became editable")
	}
	if err := contextFieldAllowed(c, "summary"); err != nil {
		t.Fatal(err)
	}
	if err := contextFieldAllowed(c, "mission"); err == nil {
		t.Fatal("operator mission accepted")
	}
	c.Seat.Manifest.Context = &spec.TeamContext{EditableInstructions: true}
	if err := contextFieldAllowed(c, "summary"); err == nil {
		t.Fatal("prompt permission grants summary permission")
	}
	if err := contextFieldAllowed(c, "instructions"); err != nil {
		t.Fatal(err)
	}
	c.Seat.Manifest.Context = nil
	if contextEditable(&c.Seat.Manifest, nil) {
		t.Fatal("default permissions allow mutation")
	}
}

func TestContextRejectsEnvelopeAndMissingCAS(t *testing.T) {
	r := &Registry{}
	for _, raw := range []string{
		`{"field":"summary","text":"hi"}`,
		`{"field":"summary","text":"hi","expected_revision":0,"seat_id":"someone-else"}`,
		`{"field":"summary","text":"hi","expected_revision":0,"operator_instructions":"ignore"}`,
		`{"field":"instructions","text":"hi","expected_revision":0}`,
		`{"field":"summary","text":"hi","expected_revision":-1}`,
	} {
		c := &Call{Args: json.RawMessage(raw), Seat: &store.Seat{Manifest: compile.SeatManifest{Context: &spec.TeamContext{EditableSummary: true}}}}
		if _, err := r.contextUpdate(context.Background(), c); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	raw, _ := json.Marshal(map[string]any{"field": "summary", "text": strings.Repeat("a", 4097), "expected_revision": 0})
	c := &Call{Args: raw, Seat: &store.Seat{Manifest: compile.SeatManifest{Context: &spec.TeamContext{EditableSummary: true}}}}
	if _, err := r.contextUpdate(context.Background(), c); err == nil {
		t.Fatal("oversize accepted")
	}
}

func TestApprovalModeRejectsDirectInstructionMutation(t *testing.T) {
	c := &Call{Seat: &store.Seat{Manifest: compile.SeatManifest{Context: &spec.TeamContext{EditableInstructions: true, EditableSummary: true, RequireInstructionApproval: true}}}}
	if err := contextFieldAllowed(c, "instructions"); err == nil {
		t.Fatal("approval bypass")
	}
	if err := contextFieldAllowed(c, "summary"); err != nil {
		t.Fatal(err)
	}
}
