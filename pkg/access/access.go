// Package access resolves access profiles (spec.AccessProfile) into what a
// seat may do from its sandbox, through plugins. A plugin owns one part of a
// profile: it validates that part and contributes to the seat's resolved
// SeatAccess, which every enforcement point reads:
//
//   - the seat runtime renders the Pod and NetworkPolicy from PodPart;
//   - the seat runner checks binaries and wires the proxy, credential
//     helpers and browser;
//   - the platform answers the egress gateway and issues credentials from
//     the live parts.
//
// Parts that need a new Pod (binaries, network rules, browser, credential
// wiring) are in PodPart and so in the config revision. Egress hosts and
// repository grants apply live, without restarting the seat.
package access

import (
	"fmt"
	"net"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/darcys22/steadmesh/pkg/spec"
)

// SeatAccess is a seat's resolved access: the union of its profiles.
type SeatAccess struct {
	Profiles []string      `json:"profiles,omitempty"`
	Binaries []string      `json:"binaries,omitempty"`
	Network  []NetworkRule `json:"network,omitempty"`
	Browser  *Browser      `json:"browser,omitempty"`
	GitHub   []GitHubGrant `json:"github,omitempty"`
	Egress   []EgressRule  `json:"egress,omitempty"`
}

// EgressRule allows a host (or *.suffix) on ports through the gateway.
type EgressRule struct {
	Host   string `json:"host"`
	Ports  []int  `json:"ports"`
	Source string `json:"source"`
}

// NetworkRule allows direct egress to a CIDR.
type NetworkRule struct {
	CIDR     string  `json:"cidr"`
	Ports    []int64 `json:"ports,omitempty"`
	Protocol string  `json:"protocol"`
	Source   string  `json:"source"`
}

// GitHubGrant is repository access through a github connection.
type GitHubGrant struct {
	Connection  string            `json:"connection"`
	Repos       []string          `json:"repos,omitempty"`
	Permissions map[string]string `json:"permissions,omitempty"`
	Delivery    string            `json:"delivery"`
	// Host is the git host of the connection: github.com, or the
	// connection's endpoint host (GitHub Enterprise, a test server).
	Host   string `json:"host"`
	Source string `json:"source"`
}

// Browser is the seat's browser tool.
type Browser struct {
	SessionConnection string `json:"session_connection,omitempty"`
	Source            string `json:"source"`
}

// Delivery modes of a credential grant.
const (
	DeliveryPlatform = "platform"
	DeliverySandbox  = "sandbox"
)

// Empty reports whether the seat has no access beyond the platform.
func (a *SeatAccess) Empty() bool {
	return a == nil || (len(a.Binaries) == 0 && len(a.Network) == 0 && a.Browser == nil && len(a.GitHub) == 0 && len(a.Egress) == 0)
}

// PodPart is the part of the access that needs a new Pod to change: it is
// what the config revision covers. Egress hosts and repository grants are
// left out because they apply live.
func (a *SeatAccess) PodPart() *SeatAccess {
	if a.Empty() {
		return nil
	}
	p := &SeatAccess{Binaries: a.Binaries, Network: a.Network, Browser: a.Browser}
	for _, g := range a.GitHub {
		if g.Delivery == DeliverySandbox {
			p.GitHub = append(p.GitHub, GitHubGrant{Connection: g.Connection, Delivery: g.Delivery, Host: g.Host})
		}
	}
	if a.UsesGateway() {
		// The proxy settings depend only on whether the gateway is used.
		p.Egress = []EgressRule{{Host: "*", Source: "gateway"}}
	}
	return p
}

// UsesGateway reports whether the seat's traffic goes through the egress gateway.
func (a *SeatAccess) UsesGateway() bool {
	return a != nil && (len(a.Egress) > 0 || a.Browser != nil)
}

// Allows reports the rule that permits host:port, if any.
func (a *SeatAccess) Allows(host string, port int) (EgressRule, bool) {
	if a == nil {
		return EgressRule{}, false
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, r := range a.Egress {
		if MatchHost(r.Host, host) && slices.Contains(r.Ports, port) {
			return r, true
		}
	}
	return EgressRule{}, false
}

// MatchHost matches host against a pattern: an exact name, or *.suffix for
// any subdomain of suffix (not suffix itself).
func MatchHost(pattern, host string) bool {
	pattern = strings.ToLower(pattern)
	if suffix, ok := strings.CutPrefix(pattern, "*."); ok {
		return strings.HasSuffix(host, "."+suffix)
	}
	return pattern == host
}

// GitHubFor returns the seat's grant on a github connection.
func (a *SeatAccess) GitHubFor(connection string) *GitHubGrant {
	if a == nil {
		return nil
	}
	for i := range a.GitHub {
		if a.GitHub[i].Connection == connection {
			return &a.GitHub[i]
		}
	}
	return nil
}

// FieldError is a validation error at a spec path.
type FieldError struct {
	Path, Message string
}

func errf(path, format string, args ...any) FieldError {
	return FieldError{Path: path, Message: fmt.Sprintf(format, args...)}
}

// Context is what plugins validate against.
type Context struct {
	Connections map[string]spec.Connection
}

// Plugin is one part of an access profile.
type Plugin interface {
	// Name is the profile field the plugin owns.
	Name() string
	// Check validates the plugin's part of profile p at path.
	Check(ctx Context, path string, p spec.AccessProfile) []FieldError
	// Apply adds the part's contribution to a seat's access.
	Apply(ctx Context, p spec.AccessProfile, source string, a *SeatAccess)
}

var plugins []Plugin

// Register adds a plugin. Built-in plugins register themselves.
func Register(p Plugin) { plugins = append(plugins, p) }

// Plugins lists the registered plugins.
func Plugins() []Plugin { return plugins }

// Check validates a profile with every plugin.
func Check(ctx Context, path string, p spec.AccessProfile) []FieldError {
	var out []FieldError
	for _, pl := range plugins {
		out = append(out, pl.Check(ctx, path, p)...)
	}
	return out
}

// Resolve combines the named profiles into a seat's access. Names must exist.
func Resolve(ctx Context, profiles map[string]spec.AccessProfile, names []string) *SeatAccess {
	names = sortedUnique(names)
	if len(names) == 0 {
		return nil
	}
	a := &SeatAccess{Profiles: names}
	for _, n := range names {
		for _, pl := range plugins {
			pl.Apply(ctx, profiles[n], "access_profile:"+n, a)
		}
	}
	a.normalize()
	return a
}

func (a *SeatAccess) normalize() {
	a.Binaries = sortedUnique(a.Binaries)
	sort.SliceStable(a.Egress, func(i, j int) bool { return a.Egress[i].Host < a.Egress[j].Host })
	sort.SliceStable(a.Network, func(i, j int) bool { return a.Network[i].CIDR < a.Network[j].CIDR })
	// One grant per connection: repositories and permissions combine, the
	// stronger permission and delivery win.
	merged := map[string]*GitHubGrant{}
	var order []string
	for _, g := range a.GitHub {
		m, ok := merged[g.Connection]
		if !ok {
			c := g
			c.Permissions = map[string]string{}
			for k, v := range g.Permissions {
				c.Permissions[k] = v
			}
			merged[g.Connection] = &c
			order = append(order, g.Connection)
			continue
		}
		m.Repos = append(m.Repos, g.Repos...)
		for k, v := range g.Permissions {
			if v == "write" || m.Permissions[k] == "" {
				m.Permissions[k] = v
			}
		}
		if g.Delivery == DeliverySandbox {
			m.Delivery = DeliverySandbox
		}
		m.Source += "," + g.Source
	}
	sort.Strings(order)
	a.GitHub = nil
	for _, c := range order {
		g := merged[c]
		g.Repos = sortedUnique(g.Repos)
		a.GitHub = append(a.GitHub, *g)
	}
}

func sortedUnique(in []string) []string {
	out := slices.Clone(in)
	sort.Strings(out)
	return slices.Compact(out)
}

// ParseHost parses an egress host entry: name, *.name, or either with :port.
func ParseHost(entry string) (host string, ports []int, err error) {
	host = strings.ToLower(strings.TrimSpace(entry))
	if h, p, err := net.SplitHostPort(host); err == nil {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return "", nil, fmt.Errorf("invalid port in %q", entry)
		}
		host, ports = h, []int{n}
	} else {
		ports = []int{443, 80}
	}
	name := strings.TrimPrefix(host, "*.")
	if name == "" || strings.ContainsAny(name, "/*@ ") || strings.HasPrefix(name, ".") || strings.HasSuffix(name, ".") {
		return "", nil, fmt.Errorf("%q is not a host name, *.domain or host:port", entry)
	}
	if !strings.Contains(name, ".") && strings.HasPrefix(host, "*.") {
		return "", nil, fmt.Errorf("%q matches a top-level domain; name a registrable domain", entry)
	}
	return host, ports, nil
}
