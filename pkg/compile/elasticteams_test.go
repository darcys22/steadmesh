package compile_test

import (
	"strings"
	"testing"

	"github.com/darcys22/steadmesh/pkg/compile"
	"github.com/darcys22/steadmesh/pkg/spec"
)

func elasticSpec(t *testing.T) spec.OrganizationSpec {
	s := compileValid(t).Spec
	s.ModelBudgets = &spec.ModelBudgets{OrganizationDailyOutputTokens: 100000, ExecutionOutputTokens: 20000}
	s.ElasticTeams = &spec.ElasticTeams{EnabledSeats: []string{"reviewer", "representative_sean"}, MaxActiveWorkers: 20, MaxActivePerExecution: 10, MaxLaunchesPerExecution: 20, MaxTimeoutSeconds: 1200, Templates: map[string]spec.WorkerTemplate{"researcher": {Instructions: "Research the assigned topic and cite evidence.", ToolNames: []string{"memory.read", "memory.search"}, HarnessProfile: "fake", ExecutionProfile: "interactive"}}}
	return s
}

func TestElasticTeamsPolicy(t *testing.T) {
	s := elasticSpec(t)
	m, err := compile.Compile(s, compile.DefaultCatalog())
	if err != nil {
		t.Fatal(err)
	}
	if m.Spec.ElasticTeams.MaxActivePerExecution != 10 {
		t.Fatal("policy lost from manifest")
	}
	m.Spec.ElasticTeams.EnabledSeats[0] = "changed"
	w := m.Spec.ElasticTeams.Templates["researcher"]
	w.ToolNames[0] = "changed"
	if s.ElasticTeams.EnabledSeats[0] != "reviewer" || s.ElasticTeams.Templates["researcher"].ToolNames[0] != "memory.read" {
		t.Fatal("compiler aliases caller policy")
	}
	copy := s.DeepCopy()
	copy.ElasticTeams.EnabledSeats[0] = "changed"
	w = copy.ElasticTeams.Templates["researcher"]
	w.ToolNames[0] = "changed"
	if s.ElasticTeams.EnabledSeats[0] != "reviewer" || s.ElasticTeams.Templates["researcher"].ToolNames[0] != "memory.read" {
		t.Fatal("deep copy aliases policy")
	}
}

func TestElasticTeamsRejections(t *testing.T) {
	tests := map[string]func(*spec.OrganizationSpec){
		"missing budgets":          func(s *spec.OrganizationSpec) { s.ModelBudgets = nil },
		"missing daily budget":     func(s *spec.OrganizationSpec) { s.ModelBudgets.OrganizationDailyOutputTokens = 0 },
		"missing execution budget": func(s *spec.OrganizationSpec) { s.ModelBudgets.ExecutionOutputTokens = 0 },
		"unknown seat":             func(s *spec.OrganizationSpec) { s.ElasticTeams.EnabledSeats = []string{"missing"} },
		"duplicate seat":           func(s *spec.OrganizationSpec) { s.ElasticTeams.EnabledSeats = []string{"reviewer", "reviewer"} },
		"empty seats":              func(s *spec.OrganizationSpec) { s.ElasticTeams.EnabledSeats = nil },
		"empty templates":          func(s *spec.OrganizationSpec) { s.ElasticTeams.Templates = nil },
		"empty key":                func(s *spec.OrganizationSpec) { s.ElasticTeams.Templates[""] = s.ElasticTeams.Templates["researcher"] },
		"zero org limit":           func(s *spec.OrganizationSpec) { s.ElasticTeams.MaxActiveWorkers = 0 },
		"large org limit":          func(s *spec.OrganizationSpec) { s.ElasticTeams.MaxActiveWorkers = 1025 },
		"zero active":              func(s *spec.OrganizationSpec) { s.ElasticTeams.MaxActivePerExecution = 0 },
		"large active":             func(s *spec.OrganizationSpec) { s.ElasticTeams.MaxActivePerExecution = 33 },
		"zero launches":            func(s *spec.OrganizationSpec) { s.ElasticTeams.MaxLaunchesPerExecution = 0 },
		"large launches":           func(s *spec.OrganizationSpec) { s.ElasticTeams.MaxLaunchesPerExecution = 129 },
		"zero timeout":             func(s *spec.OrganizationSpec) { s.ElasticTeams.MaxTimeoutSeconds = 0 },
		"large timeout":            func(s *spec.OrganizationSpec) { s.ElasticTeams.MaxTimeoutSeconds = 1201 },
		"profile mismatch second seat": func(s *spec.OrganizationSpec) {
			s.HarnessProfiles["other"] = s.HarnessProfiles["fake"]
			seat := s.Seats["representative_sean"]
			seat.HarnessProfile = "other"
			s.Seats["representative_sean"] = seat
		},
	}
	for name, edit := range tests {
		t.Run(name, func(t *testing.T) {
			s := elasticSpec(t)
			edit(&s)
			_, err := compile.Compile(s, compile.DefaultCatalog())
			if err == nil || !strings.Contains(err.Error(), "elastic_teams") {
				t.Fatalf("expected policy error, got %v", err)
			}
		})
	}
	for name, edit := range map[string]func(*spec.WorkerTemplate){
		"blank instructions": func(w *spec.WorkerTemplate) { w.Instructions = "  " },
		"large instructions": func(w *spec.WorkerTemplate) { w.Instructions = strings.Repeat("é", 4097) },
		"invalid UTF8":       func(w *spec.WorkerTemplate) { w.Instructions = "\xff" },
		"unknown harness":    func(w *spec.WorkerTemplate) { w.HarnessProfile = "missing" },
		"unknown execution":  func(w *spec.WorkerTemplate) { w.ExecutionProfile = "missing" },
		"write tool":         func(w *spec.WorkerTemplate) { w.ToolNames = []string{"memory.write"} },
		"nested workers":     func(w *spec.WorkerTemplate) { w.ToolNames = []string{"task.run"} },
		"duplicate tools":    func(w *spec.WorkerTemplate) { w.ToolNames = []string{"memory.read", "memory.read"} },
	} {
		t.Run(name, func(t *testing.T) {
			s := elasticSpec(t)
			w := s.ElasticTeams.Templates["researcher"]
			edit(&w)
			s.ElasticTeams.Templates["researcher"] = w
			_, err := compile.Compile(s, compile.DefaultCatalog())
			if err == nil {
				t.Fatal("expected policy rejection")
			}
		})
	}
}

func TestElasticTeamsBoundariesAndDrain(t *testing.T) {
	s := elasticSpec(t)
	s.ElasticTeams.MaxActiveWorkers = 1024
	s.ElasticTeams.MaxActivePerExecution = 32
	s.ElasticTeams.MaxLaunchesPerExecution = 128
	s.ElasticTeams.MaxTimeoutSeconds = 1200
	s.ElasticTeams.Draining = true
	w := s.ElasticTeams.Templates["researcher"]
	w.Instructions = strings.Repeat("é", 4096)
	w.ToolNames = []string{"memory.stores", "memory.list", "memory.read", "memory.search", "memory.history", "work.get", "work.list"}
	s.ElasticTeams.Templates["researcher"] = w
	if _, err := compile.Compile(s, compile.DefaultCatalog()); err != nil {
		t.Fatal(err)
	}
	s.ElasticTeams = nil
	s.ModelBudgets = nil
	if _, err := compile.Compile(s, compile.DefaultCatalog()); err != nil {
		t.Fatalf("omitted policy broke existing org: %v", err)
	}
}

func TestElasticTeamsInheritPerRequesterProfiles(t *testing.T) {
	s := elasticSpec(t)
	s.HarnessProfiles["other"] = s.HarnessProfiles["fake"]
	s.ExecutionProfiles["other"] = s.ExecutionProfiles["interactive"]
	seat := s.Seats["representative_sean"]
	seat.HarnessProfile = "other"
	seat.ExecutionProfile = "other"
	s.Seats["representative_sean"] = seat
	w := s.ElasticTeams.Templates["researcher"]
	w.HarnessProfile = ""
	w.ExecutionProfile = ""
	s.ElasticTeams.Templates["researcher"] = w
	if _, err := compile.Compile(s, compile.DefaultCatalog()); err != nil {
		t.Fatalf("omitted selectors should inherit each requester's profiles: %v", err)
	}
	w.ExecutionProfile = "interactive"
	s.ElasticTeams.Templates["researcher"] = w
	if _, err := compile.Compile(s, compile.DefaultCatalog()); err == nil {
		t.Fatal("explicit mismatched execution assertion accepted")
	}
}
