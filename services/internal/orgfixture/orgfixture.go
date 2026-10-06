// Package orgfixture builds a compiled organisation for service tests: two
// representatives bound to humans, and an engineering team.
package orgfixture

import (
	"strings"
	"testing"

	"github.com/darcys22/steadmesh/pkg/compile"
	"github.com/darcys22/steadmesh/pkg/spec"
)

// Human user ids and Slack account of the fixture.
const (
	Account = "T0001"
	UserA   = "U0ALICE"
	UserB   = "U0BOB"
)

func ref(name string) string {
	return "configmap:bundles/" + name + ".md#sha256:" + strings.Repeat("a", 64)
}

// Spec returns the fixture specification; tests may modify it before Compile.
func Spec() spec.OrganizationSpec {
	seat := func(role string, teams []string, personal string) spec.Seat {
		return spec.Seat{RoleRef: ref(role), Teams: teams, HarnessProfile: "fake", ExecutionProfile: "interactive",
			SandboxProfile: "standard", PersonalMemory: personal}
	}
	eng := []string{"engineering"}
	return spec.OrganizationSpec{
		Key:         "acme",
		DisplayName: "Acme",
		CultureRefs: []string{ref("culture")},
		Teams: map[string]spec.Team{
			"engineering": {InstructionRefs: []string{ref("engineering")},
				SharedMemory: map[string][]string{"engineering": {"read", "search", "write", "revise", "history"}}},
		},
		MemoryStores: map[string]spec.MemoryStore{
			"organisation": {}, "engineering": {}, "rep_a": {}, "rep_b": {}, "lead": {}, "engineer": {}, "reviewer": {},
		},
		HarnessProfiles:   map[string]spec.HarnessProfile{"fake": {Adapter: "fake", ImageDigest: "steadmesh/seat-fake:dev"}},
		ExecutionProfiles: map[string]spec.ExecutionProfile{"interactive": {Backend: "kubernetes"}},
		SandboxProfiles:   map[string]spec.SandboxProfile{"standard": {}},
		Connections: map[string]spec.Connection{
			"slack":   {Adapter: "slack", AccountID: Account, SecretRef: "k8s:slack"},
			"tracker": {Adapter: "linear", SecretRef: "k8s:linear"},
		},
		Seats: map[string]spec.Seat{
			"rep_a":    seat("representative", nil, "rep_a"),
			"rep_b":    seat("representative", nil, "rep_b"),
			"lead":     seat("lead", eng, "lead"),
			"engineer": seat("engineer", eng, "engineer"),
			"reviewer": seat("reviewer", eng, "reviewer"),
		},
		Grants: map[string]spec.Grant{
			"lead_tracker": {Subject: "seat:lead", Resource: "connection:tracker",
				Operations: []string{"project.read", "project.create", "task.write"}},
			"org_memory_read": {Subject: "seat:rep_a", Resource: "memory:organisation", Operations: []string{"read", "search"}},
			"org_memory_lead": {Subject: "seat:lead", Resource: "memory:organisation", Operations: []string{"read", "search", "write"}},
		},
		MessageRoutes: map[string]spec.MessageRoute{
			"rep_a_to_lead":        {From: "seat:rep_a", To: "seat:lead", Reply: true},
			"rep_b_to_lead":        {From: "seat:rep_b", To: "seat:lead", Reply: true},
			"lead_engineer":        {From: "seat:lead", To: "seat:engineer", Bidirectional: true},
			"engineer_to_reviewer": {From: "seat:engineer", To: "seat:reviewer"},
		},
		ChannelBindings: map[string]spec.ChannelBinding{
			"alice": {Connection: "slack", ExternalUserID: UserA, Seat: "rep_a"},
			"bob":   {Connection: "slack", ExternalUserID: UserB, Seat: "rep_b"},
		},
	}
}

// Compile compiles s with the default catalog.
func Compile(t testing.TB, s spec.OrganizationSpec) *compile.Manifest {
	t.Helper()
	m, err := compile.Compile(s, compile.DefaultCatalog())
	if err != nil {
		t.Fatalf("compile fixture: %v", err)
	}
	return m
}

// Manifest compiles the unmodified fixture.
func Manifest(t testing.TB) *compile.Manifest { return Compile(t, Spec()) }
