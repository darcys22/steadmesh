package access

import (
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/darcys22/steadmesh/pkg/spec"
)

func ctx() Context {
	return Context{Connections: map[string]spec.Connection{
		"github":  {Adapter: "github"},
		"ghe":     {Adapter: "github", EndpointRef: "http://fake-github.test:8093"},
		"session": {Adapter: "browser_session"},
		"tracker": {Adapter: "linear"},
	}}
}

func TestParseHost(t *testing.T) {
	for in, want := range map[string]string{
		"GitHub.com": "github.com 443,80", "*.githubusercontent.com": "*.githubusercontent.com 443,80",
		"example.com:8443": "example.com 8443", "10.0.0.5:5432": "10.0.0.5 5432",
	} {
		h, p, err := ParseHost(in)
		got := h + " " + strings.Trim(strings.ReplaceAll(fmt.Sprint(p), " ", ","), "[]")
		if err != nil || got != want {
			t.Errorf("%s: %q %v, want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "*.com", "http://x.com", "a/b", "x.com:0", "*", ".x.com"} {
		if _, _, err := ParseHost(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestMatchHost(t *testing.T) {
	for _, c := range []struct {
		pattern, host string
		want          bool
	}{
		{"github.com", "github.com", true}, {"github.com", "api.github.com", false},
		{"*.github.com", "api.github.com", true}, {"*.github.com", "github.com", false},
		{"*.github.com", "evilgithub.com", false}, {"*.github.com", "a.b.github.com", true},
	} {
		if got := MatchHost(c.pattern, c.host); got != c.want {
			t.Errorf("%s ~ %s = %v", c.pattern, c.host, got)
		}
	}
}

func TestCheck(t *testing.T) {
	p := spec.AccessProfile{
		Tools:   &spec.ToolsAccess{Binaries: []string{"git", "rm -rf"}},
		Egress:  &spec.EgressAccess{Hosts: []string{"github.com", "*"}},
		Network: &spec.NetworkAccess{Rules: []spec.NetworkRule{{CIDR: "10.0.5.0/24", Ports: []int64{5432}}, {CIDR: "nope", Protocol: "icmp"}}},
		GitHub:  &spec.GitHubAccess{Connection: "tracker", Repos: []string{"acme/sandbox", "bad"}, Permissions: map[string]string{"contents": "admin", "secrets": "read"}, Delivery: "email"},
		Browser: &spec.BrowserAccess{Session: &spec.BrowserSession{Connection: "github"}},
	}
	var got []string
	for _, e := range Check(ctx(), "access_profiles.p", p) {
		got = append(got, e.Path+": "+e.Message)
	}
	all := strings.Join(got, "\n")
	for _, want := range []string{
		`access_profiles.p.tools.binaries[1]: "rm -rf" is not a binary name`,
		`access_profiles.p.egress.hosts[1]`,
		`access_profiles.p.network.rules[1].cidr`, `access_profiles.p.network.rules[1].protocol`,
		`access_profiles.p.github.connection: connection "tracker" uses adapter "linear", not github`,
		`access_profiles.p.github.repos[1]: "bad" is not owner/name`,
		`access_profiles.p.github.permissions.contents: must be read or write`,
		`access_profiles.p.github.permissions.secrets: unknown permission`,
		`access_profiles.p.github.delivery: must be platform or sandbox`,
		`access_profiles.p.browser.session.connection: connection "github" uses adapter "github", not browser_session`,
	} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %q in:\n%s", want, all)
		}
	}
}

func TestResolveClausesAndPodPart(t *testing.T) {
	profiles := map[string]spec.AccessProfile{
		"read": {
			Tools:  &spec.ToolsAccess{Binaries: []string{"git"}},
			GitHub: &spec.GitHubAccess{Connection: "ghe", Repos: []string{"acme/a"}, Permissions: map[string]string{"contents": "read"}},
		},
		"write": {
			Tools:   &spec.ToolsAccess{Binaries: []string{"gh", "git"}},
			Egress:  &spec.EgressAccess{Hosts: []string{"pypi.org"}},
			Network: &spec.NetworkAccess{Rules: []spec.NetworkRule{{CIDR: "10.0.5.7/24", Ports: []int64{5432}}}},
			GitHub:  &spec.GitHubAccess{Connection: "ghe", Repos: []string{"acme/b"}, Permissions: map[string]string{"contents": "write", "pull_requests": "write"}, Delivery: "sandbox"},
		},
	}
	a := Resolve(ctx(), profiles, []string{"write", "read", "write"})
	if !slices.Equal(a.Profiles, []string{"read", "write"}) || !slices.Equal(a.Binaries, []string{"gh", "git"}) {
		t.Fatalf("profiles/binaries %+v", a)
	}
	if len(a.GitHub) != 2 {
		t.Fatalf("github grants %+v", a.GitHub)
	}
	read, write := a.GitHub[0], a.GitHub[1]
	if !slices.Equal(read.Repos, []string{"acme/a"}) || read.Permissions["contents"] != "read" || read.Delivery != DeliveryPlatform ||
		!slices.Equal(write.Repos, []string{"acme/b"}) || write.Permissions["contents"] != "write" || write.Delivery != DeliverySandbox {
		t.Fatalf("grant clauses lost: %+v", a.GitHub)
	}
	if a.GitHubFor("ghe") != nil {
		t.Fatal("ambiguous legacy lookup returned a partial grant")
	}
	g, err := a.GitHubSandboxGrant("ghe")
	if err != nil || g == nil || !slices.Equal(g.Repos, []string{"acme/b"}) || !maps.Equal(g.Permissions, write.Permissions) {
		t.Fatalf("platform access leaked into sandbox: %+v, %v", g, err)
	}
	if a.Network[0].CIDR != "10.0.5.0/24" || a.Network[0].Protocol != "tcp" {
		t.Fatalf("network %+v", a.Network)
	}
	// Sandbox delivery adds the connection's endpoint to egress.
	if _, ok := a.Allows("fake-github.test", 8093); !ok {
		t.Fatalf("github endpoint not allowed: %+v", a.Egress)
	}
	if _, ok := a.Allows("pypi.org", 443); !ok {
		t.Fatal("pypi.org not allowed")
	}
	if _, ok := a.Allows("pypi.org", 22); ok {
		t.Fatal("port 22 allowed")
	}
	// The pod part ignores live grants: changing repos or hosts keeps it.
	p1 := a.PodPart()
	profiles["write"].Egress.Hosts = []string{"files.pythonhosted.org"}
	profiles["write"].GitHub.Repos = []string{"acme/c"}
	p2 := Resolve(ctx(), profiles, []string{"read", "write"}).PodPart()
	if !podEqual(p1, p2) {
		t.Fatalf("pod part changed with live grants:\n%+v\n%+v", p1, p2)
	}
	profiles["write"].Tools.Binaries = []string{"curl"}
	if podEqual(p1, Resolve(ctx(), profiles, []string{"read", "write"}).PodPart()) {
		t.Fatal("pod part unchanged when binaries changed")
	}
}

func podEqual(a, b *SeatAccess) bool {
	return slices.Equal(a.Binaries, b.Binaries) && len(a.GitHub) == len(b.GitHub) && len(a.Egress) == len(b.Egress) &&
		slices.EqualFunc(a.Network, b.Network, func(x, y NetworkRule) bool { return x.CIDR == y.CIDR })
}

func TestGitHubDefaultHostsAndOperations(t *testing.T) {
	a := Resolve(ctx(), map[string]spec.AccessProfile{"g": {GitHub: &spec.GitHubAccess{Connection: "github", Repos: []string{"o/r"},
		Permissions: map[string]string{"pull_requests": "write"}, Delivery: "sandbox"}}}, []string{"g"})
	for _, h := range []string{"github.com", "api.github.com", "objects.githubusercontent.com"} {
		if _, ok := a.Allows(h, 443); !ok {
			t.Errorf("%s not allowed", h)
		}
	}
	if got := GitHubOperations(map[string]string{"contents": "read", "pull_requests": "write"}); !slices.Equal(got, []string{"repo.read", "pull_request.read", "pull_request.create"}) {
		t.Fatalf("ops %v", got)
	}
}

func TestGitHubSandboxGrantExactScope(t *testing.T) {
	grant := func(repos []string, perms map[string]string, delivery string) GitHubGrant {
		return GitHubGrant{Connection: "github", Repos: repos, Permissions: perms, Delivery: delivery, Host: "github.com"}
	}
	read := map[string]string{"contents": "read"}
	write := map[string]string{"contents": "write"}
	issues := map[string]string{"issues": "write"}
	for _, tt := range []struct {
		name    string
		grants  []GitHubGrant
		repos   []string
		perms   map[string]string
		wantErr bool
	}{
		{name: "no grant"},
		{name: "platform only", grants: []GitHubGrant{grant([]string{"acme/a"}, write, DeliveryPlatform)}},
		{name: "same permissions disjoint repos", grants: []GitHubGrant{grant([]string{"acme/b"}, read, DeliverySandbox), grant([]string{"acme/a"}, read, DeliverySandbox)}, repos: []string{"acme/a", "acme/b"}, perms: read},
		{name: "different permissions disjoint repos", grants: []GitHubGrant{grant([]string{"acme/a"}, read, DeliverySandbox), grant([]string{"acme/b"}, write, DeliverySandbox)}, wantErr: true},
		{name: "different permission names", grants: []GitHubGrant{grant([]string{"acme/a"}, read, DeliverySandbox), grant([]string{"acme/b"}, issues, DeliverySandbox)}, wantErr: true},
		{name: "partially overlapping repos", grants: []GitHubGrant{grant([]string{"acme/a", "acme/b"}, read, DeliverySandbox), grant([]string{"acme/b"}, write, DeliverySandbox)}, wantErr: true},
		{name: "same repos union permissions", grants: []GitHubGrant{grant([]string{"acme/a", "acme/b"}, read, DeliverySandbox), grant([]string{"acme/b", "acme/a"}, issues, DeliverySandbox)}, repos: []string{"acme/a", "acme/b"}, perms: map[string]string{"contents": "read", "issues": "write"}},
		{name: "overlap collectively uniform", grants: []GitHubGrant{grant([]string{"acme/a", "acme/b"}, read, DeliverySandbox), grant([]string{"acme/a"}, write, DeliverySandbox), grant([]string{"acme/b"}, write, DeliverySandbox)}, repos: []string{"acme/a", "acme/b"}, perms: write},
		{name: "platform write never delivered", grants: []GitHubGrant{grant([]string{"acme/a", "acme/b"}, write, DeliveryPlatform), grant([]string{"acme/a"}, read, DeliverySandbox)}, repos: []string{"acme/a"}, perms: read},
		{name: "other connection ignored", grants: []GitHubGrant{{Connection: "other", Repos: []string{"acme/a"}, Permissions: write, Delivery: DeliverySandbox}}},
		{name: "empty repos refused", grants: []GitHubGrant{grant(nil, read, DeliverySandbox)}, wantErr: true},
		{name: "empty permissions refused", grants: []GitHubGrant{grant([]string{"acme/a"}, nil, DeliverySandbox)}, wantErr: true},
		{name: "malformed repo refused", grants: []GitHubGrant{grant([]string{"acme/*"}, read, DeliverySandbox)}, wantErr: true},
		{name: "malformed permission refused", grants: []GitHubGrant{grant([]string{"acme/a"}, map[string]string{"contents": "admin"}, DeliverySandbox)}, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			for _, reverse := range []bool{false, true} {
				grants := slices.Clone(tt.grants)
				if reverse {
					slices.Reverse(grants)
				}
				a := &SeatAccess{GitHub: grants}
				got, err := a.GitHubSandboxGrant("github")
				if (err != nil) != tt.wantErr {
					t.Fatalf("got %+v, %v; wantErr %v", got, err, tt.wantErr)
				}
				if tt.wantErr {
					if got != nil {
						t.Fatal("unsafe partial scope returned with error")
					}
					continue
				}
				if len(tt.repos) == 0 {
					if got != nil {
						t.Fatalf("unexpected grant %+v", got)
					}
					continue
				}
				if got == nil || !slices.Equal(got.Repos, tt.repos) || !maps.Equal(got.Permissions, tt.perms) || got.Delivery != DeliverySandbox {
					t.Fatalf("got %+v; want repos %v perms %v", got, tt.repos, tt.perms)
				}
			}
		})
	}
	if g, err := (*SeatAccess)(nil).GitHubSandboxGrant("github"); g != nil || err != nil {
		t.Fatalf("nil access: %+v %v", g, err)
	}
}

func TestGitHubSandboxScopeDoesNotAliasGrants(t *testing.T) {
	a := &SeatAccess{GitHub: []GitHubGrant{{Connection: "github", Repos: []string{"acme/a"}, Permissions: map[string]string{"contents": "read"}, Delivery: DeliverySandbox}}}
	g, err := a.GitHubSandboxGrant("github")
	if err != nil {
		t.Fatal(err)
	}
	g.Repos[0], g.Permissions["contents"] = "acme/b", "write"
	if a.GitHub[0].Repos[0] != "acme/a" || a.GitHub[0].Permissions["contents"] != "read" {
		t.Fatal("scope aliases source grants")
	}
}

func TestPodPartIgnoresDuplicateAndReorderedClauses(t *testing.T) {
	a := &SeatAccess{GitHub: []GitHubGrant{
		{Connection: "z", Host: "github.com", Delivery: DeliverySandbox},
		{Connection: "a", Host: "git.example.com", Delivery: DeliverySandbox},
	}}
	before := a.PodPart()
	a.GitHub = append(a.GitHub, GitHubGrant{Connection: "z", Host: "github.com", Delivery: DeliverySandbox, Repos: []string{"acme/another"}, Permissions: map[string]string{"issues": "read"}})
	slices.Reverse(a.GitHub)
	if after := a.PodPart(); !reflect.DeepEqual(before, after) {
		t.Fatalf("wiring changed with live clauses: before %+v after %+v", before, after)
	}
}
