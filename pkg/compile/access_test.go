package compile_test

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/darcys22/steadmesh/pkg/compile"
	"github.com/darcys22/steadmesh/pkg/spec"
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
