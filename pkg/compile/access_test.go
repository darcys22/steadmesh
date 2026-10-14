package compile_test

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/darcys22/steadmesh/pkg/compile"
	"github.com/darcys22/steadmesh/pkg/spec"
	"github.com/darcys22/steadmesh/services/policy"
)

func withAccess(t *testing.T) spec.OrganizationSpec {
	t.Helper()
	s, _ := load(t, filepath.Join(fixtures, "valid", "representative_and_worker.yaml"))
	s.Connections["github"] = spec.Connection{Adapter: "github", SecretRef: "k8s:github"}
	s.AccessProfiles = map[string]spec.AccessProfile{
		"tools":  {Tools: &spec.ToolsAccess{Binaries: []string{"git", "gh"}}},
		"github": {GitHub: &spec.GitHubAccess{Connection: "github", Repos: []string{"acme/sandbox"}, Permissions: map[string]string{"contents": "write", "pull_requests": "write"}, Delivery: "sandbox"}},
		"pypi":   {Egress: &spec.EgressAccess{Hosts: []string{"pypi.org"}}},
	}
	tm := s.Teams["engineering"]
	tm.AccessProfiles = []string{"tools"}
	s.Teams["engineering"] = tm
	r := s.Seats["reviewer"]
	r.AccessProfiles = []string{"github", "pypi"}
	s.Seats["reviewer"] = r
	return s
}

func TestSeatAccessFromSeatAndTeams(t *testing.T) {
	m, err := compile.Compile(withAccess(t), compile.DefaultCatalog())
	if err != nil {
		t.Fatal(err)
	}
	a := m.Seats["reviewer"].Access
	if a == nil || !slices.Equal(a.Profiles, []string{"github", "pypi", "tools"}) || !slices.Equal(a.Binaries, []string{"gh", "git"}) {
		t.Fatalf("access %+v", a)
	}
	if _, ok := a.Allows("api.github.com", 443); !ok {
		t.Fatal("sandbox delivery does not allow github hosts")
	}
	if m.Seats["representative_sean"].Access != nil {
		t.Fatal("a seat without profiles has access")
	}
	// Repository operations are implicit, limited to the granted repositories.
	i := slices.IndexFunc(m.Seats["reviewer"].Capabilities, func(c compile.Capability) bool { return c.Resource == "connection:github" })
	if i < 0 {
		t.Fatal("no github capability")
	}
	c := m.Seats["reviewer"].Capabilities[i]
	if !slices.Contains(c.Operations, "pull_request.create") || !slices.Equal(c.Targets, []string{"acme/sandbox"}) {
		t.Fatalf("github capability %+v", c)
	}
}

func TestLiveAccessKeepsConfigRevision(t *testing.T) {
	s := withAccess(t)
	m1, err := compile.Compile(s, compile.DefaultCatalog())
	if err != nil {
		t.Fatal(err)
	}
	s.AccessProfiles["pypi"] = spec.AccessProfile{Egress: &spec.EgressAccess{Hosts: []string{"pypi.org", "files.pythonhosted.org"}}}
	g := *s.AccessProfiles["github"].GitHub
	g.Repos = []string{"acme/sandbox", "acme/other"}
	s.AccessProfiles["github"] = spec.AccessProfile{GitHub: &g}
	m2, err := compile.Compile(s, compile.DefaultCatalog())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m2.Seats["reviewer"].Access.Allows("files.pythonhosted.org", 443); !ok {
		t.Fatal("new host not in the manifest")
	}
	// Grants (capabilities) changed, which is a revision change today; the
	// access part alone must not be one. Compare with capabilities equalised.
	a1, a2 := m1.Seats["reviewer"], m2.Seats["reviewer"]
	a2.Capabilities = a1.Capabilities
	if compileRevision(t, a1) != compileRevision(t, a2) {
		t.Fatal("egress hosts or repositories changed the config revision")
	}
	s.AccessProfiles["tools"] = spec.AccessProfile{Tools: &spec.ToolsAccess{Binaries: []string{"git", "gh", "curl"}}}
	m3, _ := compile.Compile(s, compile.DefaultCatalog())
	if m3.Seats["reviewer"].ConfigRevision == m2.Seats["reviewer"].ConfigRevision {
		t.Fatal("a binary change kept the config revision")
	}
}

// compileRevision recompiles nothing: it recomputes a seat's revision.
func compileRevision(t *testing.T, sm compile.SeatManifest) string {
	t.Helper()
	return compile.Revision(sm)
}

func TestAccessRejected(t *testing.T) {
	s := withAccess(t)
	r := s.Seats["reviewer"]
	r.AccessProfiles = append(r.AccessProfiles, "missing")
	s.Seats["reviewer"] = r
	s.AccessProfiles["bad"] = spec.AccessProfile{GitHub: &spec.GitHubAccess{Connection: "slack", Repos: []string{"x"}}}
	_, err := compile.Compile(s, compile.DefaultCatalog())
	if err == nil {
		t.Fatal("accepted")
	}
	for _, want := range []string{`seats.reviewer.access_profiles[2]: unknown access profile "missing"`, "access_profiles.bad.github.connection", "access_profiles.bad.github.permissions"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q:\n%v", want, err)
		}
	}
}

func TestGrantClausesKeepOperationsPairedWithTargets(t *testing.T) {
	s := withAccess(t)
	// Team and seat grants, plus implicit access-profile capabilities, must
	// all preserve their own operation/target pairing.
	s.AccessProfiles["github"] = spec.AccessProfile{GitHub: &spec.GitHubAccess{Connection: "github", Repos: []string{"acme/a"}, Permissions: map[string]string{"issues": "read"}}}
	s.Grants["seat_write_b"] = spec.Grant{Subject: "seat:reviewer", Resource: "connection:github", Operations: []string{"issue.comment"}, Targets: []string{"acme/b"}}
	s.Grants["team_read_c"] = spec.Grant{Subject: "team:engineering", Resource: "connection:github", Operations: []string{"issue.read"}, Targets: []string{"acme/c"}}
	s.Grants["unrestricted_repo_read"] = spec.Grant{Subject: "seat:reviewer", Resource: "connection:github", Operations: []string{"repo.read"}}
	m, err := compile.Compile(s, compile.DefaultCatalog())
	if err != nil {
		t.Fatal(err)
	}
	sm := m.Seats["reviewer"]
	for _, tt := range []struct {
		op, target string
		want       bool
	}{
		{"issue.read", "acme/a", true}, {"issue.read", "acme/c", true},
		{"issue.comment", "acme/b", true}, {"issue.comment", "acme/a", false},
		{"issue.comment", "acme/c", false}, {"issue.read", "acme/b", false},
		{"repo.read", "acme/unlisted", true}, {"repo.read", "", true},
		{"issue.read", "", false}, {"issue.comment", "", false},
	} {
		if got := policy.ConnectionAllowed(&sm, "github", tt.op, tt.target); got != tt.want {
			t.Errorf("%s %q: got %v want %v; clauses %+v", tt.op, tt.target, got, tt.want, sm.Capabilities)
		}
	}
	if got := len(policy.Connections(&sm)["github"]); got != 4 {
		t.Fatalf("got %d clauses, want 4", got)
	}
	// Repeated compilation and serialization must keep all independent
	// clauses and deterministic revisions, including zero-target clauses.
	before, _ := json.Marshal(sm)
	for range 8 {
		again, err := compile.Compile(s, compile.DefaultCatalog())
		if err != nil {
			t.Fatal(err)
		}
		after, _ := json.Marshal(again.Seats["reviewer"])
		if string(before) != string(after) {
			t.Fatal("clause ordering is nondeterministic")
		}
	}
	var roundtrip compile.SeatManifest
	if err := json.Unmarshal(before, &roundtrip); err != nil {
		t.Fatal(err)
	}
	if policy.ConnectionAllowed(&roundtrip, "github", "issue.comment", "acme/a") {
		t.Fatal("serialization widened authority")
	}
}

func TestEqualTargetSetsCoalesceSafely(t *testing.T) {
	s := withAccess(t)
	s.Grants["same_targets_read"] = spec.Grant{Subject: "seat:reviewer", Resource: "connection:github", Operations: []string{"issue.read"}, Targets: []string{"acme/b", "acme/a", "acme/a"}}
	s.Grants["same_targets_write"] = spec.Grant{Subject: "seat:reviewer", Resource: "connection:github", Operations: []string{"issue.comment"}, Targets: []string{"acme/a", "acme/b"}}
	m, err := compile.Compile(s, compile.DefaultCatalog())
	if err != nil {
		t.Fatal(err)
	}
	sm := m.Seats["reviewer"]
	clauses := policy.Connections(&sm)["github"]
	if len(clauses) != 2 {
		t.Fatalf("want implicit sandbox clause and merged explicit clause; got %+v", clauses)
	}
	for _, clause := range clauses {
		if slices.Equal(clause.Targets, []string{"acme/a", "acme/b"}) &&
			slices.Equal(clause.Operations, []string{"issue.comment", "issue.read"}) &&
			slices.Equal(clause.Sources, []string{"grant:same_targets_read", "grant:same_targets_write"}) {
			return
		}
	}
	t.Fatalf("equal target clauses were not safely coalesced: %+v", clauses)
}

func TestAccessProfilesKeepPermissionDeliveryPairs(t *testing.T) {
	s := withAccess(t)
	s.AccessProfiles["platform"] = spec.AccessProfile{GitHub: &spec.GitHubAccess{Connection: "github", Repos: []string{"acme/a"}, Permissions: map[string]string{"issues": "read"}, Delivery: "platform"}}
	s.AccessProfiles["github"] = spec.AccessProfile{GitHub: &spec.GitHubAccess{Connection: "github", Repos: []string{"acme/b"}, Permissions: map[string]string{"issues": "write"}, Delivery: "sandbox"}}
	tm := s.Teams["engineering"]
	tm.AccessProfiles = append(tm.AccessProfiles, "platform")
	s.Teams["engineering"] = tm
	m, err := compile.Compile(s, compile.DefaultCatalog())
	if err != nil {
		t.Fatal(err)
	}
	sm := m.Seats["reviewer"]
	if !policy.ConnectionAllowed(&sm, "github", "issue.read", "acme/a") || !policy.ConnectionAllowed(&sm, "github", "issue.comment", "acme/b") || policy.ConnectionAllowed(&sm, "github", "issue.comment", "acme/a") {
		t.Fatalf("profiles widened capabilities: %+v", sm.Capabilities)
	}
	g, err := sm.Access.GitHubSandboxGrant("github")
	if err != nil || g == nil || !slices.Equal(g.Repos, []string{"acme/b"}) || g.Permissions["issues"] != "write" {
		t.Fatalf("sandbox scope includes platform grant: %+v %v", g, err)
	}
}
