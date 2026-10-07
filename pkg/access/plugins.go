package access

import (
	"net"
	"net/netip"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/darcys22/steadmesh/pkg/spec"
)

func init() {
	Register(toolsPlugin{})
	Register(egressPlugin{})
	Register(networkPlugin{})
	Register(githubPlugin{})
	Register(browserPlugin{})
}

// ---- tools -------------------------------------------------------------------

// toolsPlugin requires binaries in the seat image. The seat runner checks
// them before starting the harness, so readiness never claims a tool the
// image lacks.
type toolsPlugin struct{}

var binaryRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]*$`)

func (toolsPlugin) Name() string { return "tools" }

func (toolsPlugin) Check(_ Context, path string, p spec.AccessProfile) []FieldError {
	if p.Tools == nil {
		return nil
	}
	var out []FieldError
	if len(p.Tools.Binaries) == 0 {
		out = append(out, errf(path+".tools.binaries", "name at least one binary"))
	}
	for i, b := range p.Tools.Binaries {
		if !binaryRe.MatchString(b) {
			out = append(out, errf(path+".tools.binaries["+strconv.Itoa(i)+"]", "%q is not a binary name", b))
		}
	}
	return out
}

func (toolsPlugin) Apply(_ Context, p spec.AccessProfile, _ string, a *SeatAccess) {
	if p.Tools != nil {
		a.Binaries = append(a.Binaries, p.Tools.Binaries...)
	}
}

// ---- egress ------------------------------------------------------------------

// egressPlugin allows hosts through the egress gateway, which matches the
// CONNECT host and the TLS server name. It is the only plugin that enforces
// host names.
type egressPlugin struct{}

func (egressPlugin) Name() string { return "egress" }

func (egressPlugin) Check(_ Context, path string, p spec.AccessProfile) []FieldError {
	if p.Egress == nil {
		return nil
	}
	var out []FieldError
	if len(p.Egress.Hosts) == 0 {
		out = append(out, errf(path+".egress.hosts", "name at least one host"))
	}
	for i, h := range p.Egress.Hosts {
		if _, _, err := ParseHost(h); err != nil {
			out = append(out, errf(path+".egress.hosts["+strconv.Itoa(i)+"]", "%v", err))
		}
	}
	return out
}

func (egressPlugin) Apply(_ Context, p spec.AccessProfile, source string, a *SeatAccess) {
	if p.Egress == nil {
		return
	}
	for _, h := range p.Egress.Hosts {
		if host, ports, err := ParseHost(h); err == nil {
			a.Egress = append(a.Egress, EgressRule{Host: host, Ports: ports, Source: source})
		}
	}
}

// ---- network -----------------------------------------------------------------

// networkPlugin allows direct egress to IP ranges with NetworkPolicy ipBlock
// rules, which every NetworkPolicy implementation enforces.
type networkPlugin struct{}

func (networkPlugin) Name() string { return "network" }

func (networkPlugin) Check(_ Context, path string, p spec.AccessProfile) []FieldError {
	if p.Network == nil {
		return nil
	}
	var out []FieldError
	if len(p.Network.Rules) == 0 {
		out = append(out, errf(path+".network.rules", "name at least one rule"))
	}
	for i, r := range p.Network.Rules {
		rp := path + ".network.rules[" + strconv.Itoa(i) + "]"
		if _, err := netip.ParsePrefix(r.CIDR); err != nil {
			out = append(out, errf(rp+".cidr", "%q is not a CIDR", r.CIDR))
		}
		switch strings.ToLower(r.Protocol) {
		case "", "tcp", "udp", "sctp":
		default:
			out = append(out, errf(rp+".protocol", "must be tcp, udp or sctp"))
		}
		for _, port := range r.Ports {
			if port < 1 || port > 65535 {
				out = append(out, errf(rp+".ports", "%d is not a port", port))
			}
		}
	}
	return out
}

func (networkPlugin) Apply(_ Context, p spec.AccessProfile, source string, a *SeatAccess) {
	if p.Network == nil {
		return
	}
	for _, r := range p.Network.Rules {
		proto := strings.ToLower(r.Protocol)
		if proto == "" {
			proto = "tcp"
		}
		pfx, err := netip.ParsePrefix(r.CIDR)
		if err != nil {
			continue
		}
		a.Network = append(a.Network, NetworkRule{CIDR: pfx.Masked().String(), Ports: slices.Sorted(slices.Values(r.Ports)), Protocol: proto, Source: source})
	}
}

// ---- github ------------------------------------------------------------------

// githubPlugin grants repository access through a github connection. With
// platform delivery the seat calls repository operations through the
// gateway and never holds the credential. With sandbox delivery, git and gh
// in the sandbox receive a short-lived, repository-scoped credential (or the
// PAT itself), and the GitHub hosts are allowed through the egress gateway.
type githubPlugin struct{}

// GitHubPermissions are the permission names a grant may set.
var GitHubPermissions = []string{"contents", "pull_requests", "issues", "metadata"}

// GitHubHosts are allowed through the egress gateway for sandbox delivery to
// github.com.
var GitHubHosts = []string{"github.com", "api.github.com", "codeload.github.com", "*.githubusercontent.com"}

var repoRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

func (githubPlugin) Name() string { return "github" }

func (githubPlugin) Check(ctx Context, path string, p spec.AccessProfile) []FieldError {
	g := p.GitHub
	if g == nil {
		return nil
	}
	path += ".github"
	var out []FieldError
	if c, ok := ctx.Connections[g.Connection]; !ok {
		out = append(out, errf(path+".connection", "unknown connection %q", g.Connection))
	} else if c.Adapter != "github" {
		out = append(out, errf(path+".connection", "connection %q uses adapter %q, not github", g.Connection, c.Adapter))
	}
	if len(g.Repos) == 0 {
		out = append(out, errf(path+".repos", "name at least one owner/name repository"))
	}
	for i, r := range g.Repos {
		if !repoRe.MatchString(r) {
			out = append(out, errf(path+".repos["+strconv.Itoa(i)+"]", "%q is not owner/name", r))
		}
	}
	if len(g.Permissions) == 0 {
		out = append(out, errf(path+".permissions", "grant at least one of %s", strings.Join(GitHubPermissions, ", ")))
	}
	for k, v := range g.Permissions {
		if !slices.Contains(GitHubPermissions, k) {
			out = append(out, errf(path+".permissions."+k, "unknown permission (known: %s)", strings.Join(GitHubPermissions, ", ")))
		}
		if v != "read" && v != "write" {
			out = append(out, errf(path+".permissions."+k, "must be read or write"))
		}
	}
	switch g.Delivery {
	case "", DeliveryPlatform, DeliverySandbox:
	default:
		out = append(out, errf(path+".delivery", "must be platform or sandbox"))
	}
	return out
}

func (githubPlugin) Apply(ctx Context, p spec.AccessProfile, source string, a *SeatAccess) {
	g := p.GitHub
	if g == nil {
		return
	}
	delivery := g.Delivery
	if delivery == "" {
		delivery = DeliveryPlatform
	}
	perms := map[string]string{}
	for k, v := range g.Permissions {
		perms[k] = v
	}
	a.GitHub = append(a.GitHub, GitHubGrant{Connection: g.Connection, Repos: g.Repos, Permissions: perms, Delivery: delivery,
		Host: GitHost(ctx.Connections[g.Connection]), Source: source})
	if delivery != DeliverySandbox {
		return
	}
	for _, h := range githubHosts(ctx.Connections[g.Connection]) {
		host, ports, err := ParseHost(h)
		if err == nil {
			a.Egress = append(a.Egress, EgressRule{Host: host, Ports: ports, Source: source + "/github"})
		}
	}
}

// githubHosts are the hosts sandbox delivery needs: github.com's, or the
// connection's own endpoint (GitHub Enterprise or a test server).
func githubHosts(c spec.Connection) []string {
	if c.EndpointRef == "" {
		return GitHubHosts
	}
	u, err := url.Parse(c.EndpointRef)
	if err != nil || u.Host == "" {
		return nil
	}
	if u.Port() != "" {
		return []string{u.Host}
	}
	port := "443"
	if u.Scheme == "http" {
		port = "80"
	}
	return []string{net.JoinHostPort(u.Hostname(), port)}
}

// GitHost is the host git uses for a github connection.
func GitHost(c spec.Connection) string {
	if c.EndpointRef == "" {
		return "github.com"
	}
	if u, err := url.Parse(c.EndpointRef); err == nil && u.Host != "" {
		return u.Host
	}
	return "github.com"
}

// GitHubOperations maps permissions to the connection operations a
// platform-delivered grant allows.
func GitHubOperations(perms map[string]string) []string {
	var ops []string
	if perms["contents"] != "" || perms["metadata"] != "" {
		ops = append(ops, "repo.read")
	}
	switch perms["pull_requests"] {
	case "write":
		ops = append(ops, "pull_request.read", "pull_request.create")
	case "read":
		ops = append(ops, "pull_request.read")
	}
	switch perms["issues"] {
	case "write":
		ops = append(ops, "issue.read", "issue.comment")
	case "read":
		ops = append(ops, "issue.read")
	}
	return ops
}

// ---- browser -----------------------------------------------------------------

// browserPlugin gives the seat a headless browser (Playwright MCP) wired into
// its harness as a tool. Its traffic goes through the egress gateway, so the
// seat's egress rules decide what it can open.
type browserPlugin struct{}

func (browserPlugin) Name() string { return "browser" }

func (browserPlugin) Check(ctx Context, path string, p spec.AccessProfile) []FieldError {
	if p.Browser == nil || p.Browser.Session == nil {
		return nil
	}
	c, ok := ctx.Connections[p.Browser.Session.Connection]
	switch {
	case !ok:
		return []FieldError{errf(path+".browser.session.connection", "unknown connection %q", p.Browser.Session.Connection)}
	case c.Adapter != "browser_session":
		return []FieldError{errf(path+".browser.session.connection", "connection %q uses adapter %q, not browser_session", p.Browser.Session.Connection, c.Adapter)}
	}
	return nil
}

func (browserPlugin) Apply(_ Context, p spec.AccessProfile, source string, a *SeatAccess) {
	if p.Browser == nil {
		return
	}
	b := &Browser{Source: source}
	if p.Browser.Session != nil {
		b.SessionConnection = p.Browser.Session.Connection
	}
	if a.Browser == nil || (a.Browser.SessionConnection == "" && b.SessionConnection != "") {
		a.Browser = b
	}
}
