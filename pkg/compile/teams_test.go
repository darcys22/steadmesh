package compile_test

import (
	"strings"
	"testing"

	"github.com/darcys22/steadmesh/pkg/compile"
	"github.com/darcys22/steadmesh/pkg/spec"
)

func nativePoolSpec(t *testing.T) spec.OrganizationSpec {
	s := compileValid(t).Spec
	s.ModelBudgets = &spec.ModelBudgets{OrganizationDailyOutputTokens: 100000, ExecutionOutputTokens: 20000}
	seat := s.Seats["reviewer"]
	seat.PersonalMemory = ""
	seat.AdoptFrom = ""
	seat.Teams = nil
	s.Teams["pool_team"] = spec.Team{Pool: &spec.TeamPool{MaxMembers: 4, Templates: map[string]spec.WorkerTemplate{"builder": {Instructions: "Build and verify", Seat: &seat}}}, Context: &spec.TeamContext{EditableSummary: true}, Coordination: &spec.TeamCoordination{Mode: "self_organising", MissionEditors: []string{"team:pool_team"}}}
	return s
}
func TestComposableTeamsDefaultsAndIdentity(t *testing.T) {
	s := nativePoolSpec(t)
	m, err := compile.Compile(s, compile.DefaultCatalog())
	if err != nil {
		t.Fatal(err)
	}
	p := m.Spec.Teams["pool_team"].Pool
	if p.MinMembers != 0 || p.IdleTimeoutSeconds != 900 || p.DrainTimeoutSeconds != 300 || p.RetentionSeconds != 0 {
		t.Fatalf("incorrect defaults: %+v", p)
	}
	if m.Spec.Teams["pool_team"].Coordination.MaxMessagesPerExecution != 20 {
		t.Fatal("coordination default lost")
	}
	if s.Teams["pool_team"].Pool.IdleTimeoutSeconds != 0 {
		t.Fatal("mutated caller")
	}
	sm, err := compile.CompilePoolSeat(s, compile.DefaultCatalog(), "pool_team", "builder", "pool_member_one")
	if err != nil {
		t.Fatal(err)
	}
	if sm.Key != "pool_member_one" || sm.PoolTeam != "pool_team" || sm.Context == nil || !sm.Context.EditableSummary || sm.Context.EditableInstructions {
		t.Fatalf("incorrect independent identity/context: %+v", sm)
	}
	if _, ok := m.Seats["pool_member_one"]; ok {
		t.Fatal("runtime member leaked into desired manifest")
	}
	if _, ok := s.Seats["pool_member_one"]; ok {
		t.Fatal("runtime member leaked into input")
	}
	if len(sm.SendTo) != 0 {
		t.Fatalf("coordination created implicit routes: %+v", sm.SendTo)
	}
}
func TestComposableTeamsIndependentModes(t *testing.T) {
	s := compileValid(t).Spec
	s.Teams["coordinated"] = spec.Team{Coordination: &spec.TeamCoordination{Mode: "self_organising"}}
	if _, err := compile.Compile(s, compile.DefaultCatalog()); err != nil {
		t.Fatal(err)
	}
	s = nativePoolSpec(t)
	team := s.Teams["pool_team"]
	team.Coordination = nil
	team.Context = nil
	s.Teams["pool_team"] = team
	if _, err := compile.Compile(s, compile.DefaultCatalog()); err != nil {
		t.Fatal(err)
	}
}
func TestComposableTeamsDenyInvalidPolicies(t *testing.T) {
	cases := map[string]func(*spec.OrganizationSpec){
		"min_over_max":        func(s *spec.OrganizationSpec) { s.Teams["pool_team"].Pool.MinMembers = 5 },
		"negative_idle":       func(s *spec.OrganizationSpec) { s.Teams["pool_team"].Pool.IdleTimeoutSeconds = -1 },
		"unbounded_retention": func(s *spec.OrganizationSpec) { s.Teams["pool_team"].Pool.RetentionSeconds = 2592001 },
		"missing_budget":      func(s *spec.OrganizationSpec) { s.ModelBudgets = nil },
		"missing_template_seat": func(s *spec.OrganizationSpec) {
			p := s.Teams["pool_team"].Pool
			v := p.Templates["builder"]
			v.Seat = nil
			p.Templates["builder"] = v
		},
		"unknown_profile": func(s *spec.OrganizationSpec) {
			s.Teams["pool_team"].Pool.Templates["builder"].Seat.HarnessProfile = "missing"
		},
		"fixed_private_memory": func(s *spec.OrganizationSpec) {
			s.Teams["pool_team"].Pool.Templates["builder"].Seat.PersonalMemory = "fixed"
		},
		"adopt_identity": func(s *spec.OrganizationSpec) { s.Teams["pool_team"].Pool.Templates["builder"].Seat.AdoptFrom = "old" },
		"legacy_tools": func(s *spec.OrganizationSpec) {
			p := s.Teams["pool_team"].Pool
			v := p.Templates["builder"]
			v.ToolNames = []string{"memory.read"}
			p.Templates["builder"] = v
		},
		"editor_outside_team": func(s *spec.OrganizationSpec) {
			s.Teams["pool_team"].Coordination.MissionEditors = []string{"seat:reviewer"}
		},
		"invalid_mode":           func(s *spec.OrganizationSpec) { s.Teams["pool_team"].Coordination.Mode = "manager" },
		"required_without_board": func(s *spec.OrganizationSpec) { s.Teams["pool_team"].Coordination.ReviewPolicy = "required" },
		"ungranted_board": func(s *spec.OrganizationSpec) {
			s.MemoryStores["board"] = spec.MemoryStore{}
			s.Teams["pool_team"].Coordination.WorkStore = "board"
		},
	}
	for name, edit := range cases {
		t.Run(name, func(t *testing.T) {
			s := nativePoolSpec(t)
			edit(&s)
			_, err := compile.Compile(s, compile.DefaultCatalog())
			if err == nil || !strings.Contains(err.Error(), "pool_team") {
				t.Fatalf("expected policy denial, got %v", err)
			}
		})
	}
}
func TestLegacyElasticTeamsRejectNativeSeat(t *testing.T) {
	s := elasticSpec(t)
	v := s.ElasticTeams.Templates["researcher"]
	seat := s.Seats["reviewer"]
	v.Seat = &seat
	s.ElasticTeams.Templates["researcher"] = v
	if _, err := compile.Compile(s, compile.DefaultCatalog()); err == nil {
		t.Fatal("legacy authority escalated")
	}
}
func TestNativePoolDeepCopy(t *testing.T) {
	s := nativePoolSpec(t)
	c := s.DeepCopy()
	c.Teams["pool_team"].Pool.Templates["builder"].Seat.RoleRef = "changed"
	c.Teams["pool_team"].Coordination.MissionEditors[0] = "changed"
	c.Teams["pool_team"].Context.EditableSummary = false
	if s.Teams["pool_team"].Pool.Templates["builder"].Seat.RoleRef == "changed" || s.Teams["pool_team"].Coordination.MissionEditors[0] == "changed" || !s.Teams["pool_team"].Context.EditableSummary {
		t.Fatal("deepcopy aliases native policy")
	}
}

func TestNativePoolAuthorityAndContextUnion(t *testing.T) {
	s := nativePoolSpec(t)
	s.MemoryStores["board"] = spec.MemoryStore{}
	team := s.Teams["pool_team"]
	team.SharedMemory = map[string][]string{"board": {"read", "write", "revise"}}
	team.Coordination.WorkStore = "board"
	team.Coordination.ReviewPolicy = "required"
	s.Teams["pool_team"] = team
	s.Teams["extra"] = spec.Team{Context: &spec.TeamContext{EditableInstructions: true}}
	team.Pool.Templates["builder"].Seat.Teams = []string{"extra"}
	seat := s.Seats["reviewer"]
	seat.Teams = append(seat.Teams, "pool_team")
	s.Seats["reviewer"] = seat
	s.MessageRoutes["native_collaboration"] = spec.MessageRoute{From: "team:pool_team", To: "team:pool_team", Reply: true}
	sm, err := compile.CompilePoolSeat(s, compile.DefaultCatalog(), "pool_team", "builder", "pool_member_one")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, cap := range sm.Capabilities {
		if cap.Resource == "memory:board" {
			for _, op := range cap.Operations {
				if op == "write" {
					found = true
				}
			}
		}
	}
	if !found {
		t.Fatal("explicit write authority not propagated")
	}
	if sm.Context == nil || !sm.Context.EditableInstructions || !sm.Context.EditableSummary {
		t.Fatal("context membership permissions not unioned")
	}
	found = false
	for _, edge := range sm.SendTo {
		if edge.Seat == "reviewer" {
			found = true
		}
	}
	if !found {
		t.Fatal("explicit team route not expanded")
	}
	s.Teams["duplicate"] = spec.Team{SharedMemory: map[string][]string{"board": {"read", "write", "revise"}}, Coordination: &spec.TeamCoordination{WorkStore: "board"}}
	if _, err = compile.Compile(s, compile.DefaultCatalog()); err == nil || !strings.Contains(err.Error(), "already used") {
		t.Fatalf("ambiguous board accepted: %v", err)
	}
}

func TestNativePoolResolvesTemplateRoles(t *testing.T) {
	s := nativePoolSpec(t)
	role := s.Teams["pool_team"].Pool.Templates["builder"].Seat.RoleRef
	s.TeamTemplates = map[string]spec.TeamTemplate{"pool_roles": {Roles: map[string]string{"builder": role}}}
	team := s.Teams["pool_team"]
	team.Template = "pool_roles"
	team.Pool.Templates["builder"].Seat.RoleRef = "role:builder"
	s.Teams["pool_team"] = team
	m, err := compile.Compile(s, compile.DefaultCatalog())
	if err != nil {
		t.Fatal(err)
	}
	if m.Spec.Teams["pool_team"].Pool.Templates["builder"].Seat.RoleRef != role {
		t.Fatal("pool role not resolved before template flattening")
	}
	if _, err := compile.CompilePoolSeat(m.Spec, compile.DefaultCatalog(), "pool_team", "builder", "pool_member"); err != nil {
		t.Fatal(err)
	}
}

func TestTeamPolicyRevisionSemantics(t *testing.T) {
	s := nativePoolSpec(t)
	seat := s.Seats["reviewer"]
	seat.Teams = append(seat.Teams, "pool_team")
	s.Seats["reviewer"] = seat
	a, err := compile.Compile(s, compile.DefaultCatalog())
	if err != nil {
		t.Fatal(err)
	}
	s.Teams["pool_team"].Coordination.Mission = "Updated mission"
	s.Teams["pool_team"].Coordination.MaxMessagesPerExecution = 50
	b, err := compile.Compile(s, compile.DefaultCatalog())
	if err != nil {
		t.Fatal(err)
	}
	if a.Digest == b.Digest {
		t.Fatal("coordination change not reflected in manifest digest")
	}
	if a.Seats["reviewer"].ConfigRevision != b.Seats["reviewer"].ConfigRevision {
		t.Fatal("live coordination update changed pod configuration")
	}
	s.Teams["pool_team"].Context.EditableInstructions = true
	c, err := compile.Compile(s, compile.DefaultCatalog())
	if err != nil {
		t.Fatal(err)
	}
	if b.Seats["reviewer"].ConfigRevision == c.Seats["reviewer"].ConfigRevision {
		t.Fatal("context permission change absent from config revision")
	}
	if b.Seats["representative_sean"].ConfigRevision != c.Seats["representative_sean"].ConfigRevision {
		t.Fatal("unrelated seat changed")
	}
}

func TestInstructionApprovalStrictUnion(t *testing.T) {
	s := nativePoolSpec(t)
	team := s.Teams["pool_team"]
	team.Context = &spec.TeamContext{EditableInstructions: true, RequireInstructionApproval: true, InstructionApprovers: []string{"team:pool_team"}}
	s.Teams["pool_team"] = team
	s.Teams["direct"] = spec.Team{Context: &spec.TeamContext{EditableInstructions: true, EditableSummary: true}}
	team.Pool.Templates["builder"].Seat.Teams = []string{"direct"}
	sm, err := compile.CompilePoolSeat(s, compile.DefaultCatalog(), "pool_team", "builder", "pool_member")
	if err != nil {
		t.Fatal(err)
	}
	if !sm.Context.EditableInstructions || !sm.Context.EditableSummary || !sm.Context.RequireInstructionApproval || len(sm.Context.InstructionApprovers) != 1 || sm.Context.InstructionApprovers[0] != "team:pool_team" {
		t.Fatalf("approval union weakened: %+v", sm.Context)
	}
	copy := s.DeepCopy()
	copy.Teams["pool_team"].Context.InstructionApprovers[0] = "changed"
	if s.Teams["pool_team"].Context.InstructionApprovers[0] != "team:pool_team" {
		t.Fatal("approval selectors alias deepcopy")
	}
}
func TestInstructionApprovalValidation(t *testing.T) {
	for name, p := range map[string]spec.TeamContext{
		"no_edit_permission": {RequireInstructionApproval: true, InstructionApprovers: []string{"team:pool_team"}},
		"no_approvers":       {EditableInstructions: true, RequireInstructionApproval: true},
		"unknown_approver":   {EditableInstructions: true, RequireInstructionApproval: true, InstructionApprovers: []string{"seat:missing"}},
		"nonmember_approver": {EditableInstructions: true, RequireInstructionApproval: true, InstructionApprovers: []string{"seat:reviewer"}},
		"foreign_team":       {EditableInstructions: true, RequireInstructionApproval: true, InstructionApprovers: []string{"team:engineering"}},
		"duplicate":          {EditableInstructions: true, RequireInstructionApproval: true, InstructionApprovers: []string{"team:pool_team", "team:pool_team"}},
	} {
		t.Run(name, func(t *testing.T) {
			s := nativePoolSpec(t)
			team := s.Teams["pool_team"]
			team.Context = &p
			s.Teams["pool_team"] = team
			if _, err := compile.Compile(s, compile.DefaultCatalog()); err == nil || !strings.Contains(err.Error(), "context") {
				t.Fatalf("expected context rejection, got %v", err)
			}
		})
	}
}
func TestPoolRetentionIsExplicitOptIn(t *testing.T) {
	for _, seconds := range []int{0, 1, 2592000} {
		s := nativePoolSpec(t)
		s.Teams["pool_team"].Pool.RetentionSeconds = seconds
		m, err := compile.Compile(s, compile.DefaultCatalog())
		if err != nil {
			t.Fatal(err)
		}
		if m.Spec.Teams["pool_team"].Pool.RetentionSeconds != seconds {
			t.Fatal("retention opt-in changed")
		}
	}
	s := nativePoolSpec(t)
	s.Teams["pool_team"].Pool.RetentionSeconds = -1
	if _, err := compile.Compile(s, compile.DefaultCatalog()); err == nil {
		t.Fatal("negative retention accepted")
	}
}
