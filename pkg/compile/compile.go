// Package compile turns a declared organisation specification into a concrete,
// resolved specification and an effective manifest (§4.2, §4.6, §6.1).
//
// Template merge rules (§4.2):
//   - A template may extend one other template; cycles are rejected.
//   - Scalars (parameters, roles) are overridden by the more derived definition.
//   - Ordered instruction lists are concatenated base-first, preserving declared
//     order; a reference that appears twice keeps its first position.
//   - Keyed permission records (shared_memory) merge by key. The same key with
//     different operations is an ambiguous conflict and is rejected, because
//     permission grants must remain explicit.
package compile

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
	// Time zones are validated without relying on the host's zoneinfo.
	_ "time/tzdata"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/darcys22/steadmesh/harnesses"
	"github.com/darcys22/steadmesh/pkg/access"
	"github.com/darcys22/steadmesh/pkg/spec"
)

var (
	keyPattern       = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)
	instructionRefRe = regexp.MustCompile(`^(configmap:[a-z0-9]([-a-z0-9.]*[a-z0-9])?/[-._a-zA-Z0-9]+|https://\S+)#sha256:[a-f0-9]{64}$`)
	secretRefRe      = regexp.MustCompile(`^(vault:[-_a-zA-Z0-9/]+|k8s:[a-z0-9]([-a-z0-9.]*[a-z0-9])?)$`)
)

// FieldError is a validation failure at a specification path.
type FieldError struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

func (e FieldError) Error() string { return e.Path + ": " + e.Message }

// Errors is a list of validation failures, sorted by path.
type Errors []FieldError

func (e Errors) Error() string {
	parts := make([]string, len(e))
	for i, fe := range e {
		parts[i] = fe.Error()
	}
	return strings.Join(parts, "; ")
}

// Manifest is the effective, resolved organisation.
type Manifest struct {
	SchemaVersion   string                  `json:"schema_version"`
	DefaultsVersion int                     `json:"defaults_version"`
	Spec            spec.OrganizationSpec   `json:"spec"`
	Seats           map[string]SeatManifest `json:"seats"`
	// TemplateLineage maps team key to the template chain it was resolved from, base first.
	TemplateLineage map[string][]string `json:"template_lineage,omitempty"`
	Digest          string              `json:"digest"`
}

// InstructionSource records one entry of a seat's ordered instruction manifest (§4.3).
type InstructionSource struct {
	Ref   string `json:"ref"`
	Scope string `json:"scope"` // organisation, team:<key>, role, seat
	Order int    `json:"order"`
}

// Capability is one entry in a seat's resolved capability manifest (§5.2).
type Capability struct {
	Resource   string   `json:"resource"` // memory:<k>, connection:<k>, workspace:<k>
	Operations []string `json:"operations"`
	Targets    []string `json:"targets,omitempty"`
	Sources    []string `json:"sources"` // grant:<k>, implicit:personal_memory, team:<k>/shared_memory, ...
}

// RouteEdge is a permitted message direction from the seat that owns it.
type RouteEdge struct {
	Seat   string   `json:"seat"`
	Reply  bool     `json:"reply"`
	Routes []string `json:"routes"`
}

// SeatManifest is the effective configuration of one seat.
type SeatManifest struct {
	Key              string                `json:"key"`
	DisplayName      string                `json:"display_name"`
	RoleRef          string                `json:"role_ref"`
	Teams            []string              `json:"teams,omitempty"`
	HarnessProfile   string                `json:"harness_profile"`
	ExecutionProfile string                `json:"execution_profile"`
	SandboxProfile   string                `json:"sandbox_profile"`
	Harness          spec.HarnessProfile   `json:"harness"`
	Execution        spec.ExecutionProfile `json:"execution"`
	Sandbox          spec.SandboxProfile   `json:"sandbox"`
	PersonalMemory   string                `json:"personal_memory,omitempty"`
	Workspace        spec.WorkspaceBinding `json:"workspace"`
	Instructions     []InstructionSource   `json:"instructions"`
	Capabilities     []Capability          `json:"capabilities"`
	SendTo           []RouteEdge           `json:"send_to,omitempty"`
	ReceiveFrom      []RouteEdge           `json:"receive_from,omitempty"`
	ChannelBindings  []string              `json:"channel_bindings,omitempty"`
	AdoptFrom        string                `json:"adopt_from,omitempty"`
	IsRepresentative bool                  `json:"is_representative"`
	// Timezone is where the seat reads and schedules times of day: its
	// human's for a representative, otherwise the organisation's.
	Timezone string `json:"timezone"`
	// Access is what the seat may do from its sandbox (access profiles).
	Access         *access.SeatAccess `json:"access,omitempty"`
	ConfigRevision string             `json:"config_revision"`
}

// Compile validates and resolves an organisation specification.
func Compile(in spec.OrganizationSpec, cat Catalog) (*Manifest, error) {
	c := &compiler{in: in, cat: cat}
	m := c.run()
	if len(c.errs) > 0 {
		sort.SliceStable(c.errs, func(i, j int) bool { return c.errs[i].Path < c.errs[j].Path })
		return nil, c.errs
	}
	return m, nil
}

type compiler struct {
	in   spec.OrganizationSpec
	cat  Catalog
	errs Errors
}

func (c *compiler) errf(path, format string, args ...any) {
	c.errs = append(c.errs, FieldError{Path: path, Message: fmt.Sprintf(format, args...)})
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func (c *compiler) run() *Manifest {
	out := deepCopySpec(c.in)
	c.checkOrganization(&out)
	c.checkKeys(&out)
	lineage := c.resolveTemplates(&out)
	c.applyDefaults(&out)
	c.checkProfiles(&out)
	c.checkConnections(&out)
	c.checkMemoryAndWorkspaces(&out)
	c.checkSeats(&out)
	c.checkWorkPublication(&out)
	c.checkAccess(&out)
	if len(c.errs) > 0 {
		return nil
	}

	seats := map[string]SeatManifest{}
	for _, k := range sortedKeys(out.Seats) {
		seats[k] = c.seatManifest(&out, k)
	}
	c.applyGrants(&out, seats)
	c.applyRoutes(&out, seats)
	c.applyBindings(&out, seats)
	if len(c.errs) > 0 {
		return nil
	}
	for k, sm := range seats {
		sm.ConfigRevision = Revision(sm)
		seats[k] = sm
	}
	m := &Manifest{
		SchemaVersion:   spec.SchemaVersion,
		DefaultsVersion: spec.DefaultsVersion,
		Spec:            out,
		Seats:           seats,
		TemplateLineage: lineage,
	}
	m.Digest = digest(struct {
		Spec  spec.OrganizationSpec
		Seats map[string]SeatManifest
	}{m.Spec, m.Seats})
	return m
}

func (c *compiler) checkOrganization(o *spec.OrganizationSpec) {
	if !keyPattern.MatchString(strings.ReplaceAll(o.Key, "-", "_")) || len(o.Key) > 40 {
		c.errf("key", "must be 1-40 lowercase letters, digits, '-' or '_' and start with a letter")
	}
	if strings.TrimSpace(o.DisplayName) == "" {
		c.errf("display_name", "is required")
	}
	if o.DataRetention != "" && o.DataRetention != "retain" && o.DataRetention != "delete" {
		c.errf("data_retention", "must be retain or delete")
	}
	c.checkTimezone("timezone", o.Timezone)
	for i, r := range o.CultureRefs {
		c.checkInstructionRef(fmt.Sprintf("culture_refs[%d]", i), r)
	}
	b, _ := json.Marshal(o)
	if len(b) > spec.MaxSpecBytes {
		c.errf("spec", "serialised specification is %d bytes; limit is %d. Reference large instruction bundles instead of inlining them", len(b), spec.MaxSpecBytes)
	}
}

func (c *compiler) checkInstructionRef(path, ref string) {
	if !instructionRefRe.MatchString(ref) {
		c.errf(path, "instruction reference %q must be configmap:<name>/<key>#sha256:<digest> or https://...#sha256:<digest>", ref)
	}
}

func (c *compiler) checkKeys(o *spec.OrganizationSpec) {
	check := func(kind string, keys []string) {
		for _, k := range keys {
			if !keyPattern.MatchString(k) {
				c.errf(kind+"."+k, "key must match %s", keyPattern.String())
			}
		}
	}
	check("team_templates", sortedKeys(o.TeamTemplates))
	check("teams", sortedKeys(o.Teams))
	check("memory_stores", sortedKeys(o.MemoryStores))
	check("shared_workspaces", sortedKeys(o.SharedWorkspaces))
	check("harness_profiles", sortedKeys(o.HarnessProfiles))
	check("execution_profiles", sortedKeys(o.ExecutionProfiles))
	check("sandbox_profiles", sortedKeys(o.SandboxProfiles))
	check("connections", sortedKeys(o.Connections))
	check("seats", sortedKeys(o.Seats))
	check("grants", sortedKeys(o.Grants))
	check("message_routes", sortedKeys(o.MessageRoutes))
	check("channel_bindings", sortedKeys(o.ChannelBindings))
}

// resolveTemplates flattens team templates into concrete teams and removes
// team_templates from the output. Seat role references of the form
// role:<name> are resolved against the templates of the seat's teams.
func (c *compiler) resolveTemplates(o *spec.OrganizationSpec) map[string][]string {
	lineage := map[string][]string{}
	chainOf := func(start, path string) []string {
		var chain []string
		seen := map[string]bool{}
		for cur := start; cur != ""; {
			if seen[cur] {
				c.errf(path, "template inheritance cycle: %s", strings.Join(append(chain, cur), " -> "))
				return nil
			}
			seen[cur] = true
			t, ok := o.TeamTemplates[cur]
			if !ok {
				c.errf(path, "unknown team template %q", cur)
				return nil
			}
			chain = append(chain, cur)
			cur = t.Extends
		}
		slices.Reverse(chain)
		return chain
	}
	// Report cycles even for templates no team uses.
	for _, k := range sortedKeys(o.TeamTemplates) {
		chainOf(k, "team_templates."+k)
	}

	teamRoles := map[string]map[string]string{}
	for _, tk := range sortedKeys(o.Teams) {
		team := o.Teams[tk]
		path := "teams." + tk
		resolved := spec.Team{Parameters: map[string]string{}, SharedMemory: map[string][]string{}, AccessProfiles: team.AccessProfiles}
		roles := map[string]string{}
		var layers []spec.TeamTemplate
		if team.Template != "" {
			chain := chainOf(team.Template, path+".template")
			if chain == nil {
				continue
			}
			lineage[tk] = chain
			for _, name := range chain {
				layers = append(layers, o.TeamTemplates[name])
			}
		}
		layers = append(layers, spec.TeamTemplate{
			InstructionRefs: team.InstructionRefs,
			SharedMemory:    team.SharedMemory,
			Parameters:      team.Parameters,
		})
		for _, l := range layers {
			resolved.InstructionRefs = appendUnique(resolved.InstructionRefs, l.InstructionRefs...)
			for k, v := range l.Parameters {
				resolved.Parameters[k] = v
			}
			for k, v := range l.Roles {
				roles[k] = v
			}
			for k, ops := range l.SharedMemory {
				if prev, ok := resolved.SharedMemory[k]; ok && !sameSet(prev, ops) {
					c.errf(path+".shared_memory."+k, "ambiguous conflict: inherited operations %v differ from %v; declare a single explicit grant", prev, ops)
					continue
				}
				resolved.SharedMemory[k] = sortedUnique(ops)
			}
		}
		for i, r := range resolved.InstructionRefs {
			c.checkInstructionRef(fmt.Sprintf("%s.instruction_refs[%d]", path, i), r)
		}
		if len(resolved.Parameters) == 0 {
			resolved.Parameters = nil
		}
		if len(resolved.SharedMemory) == 0 {
			resolved.SharedMemory = nil
		}
		o.Teams[tk] = resolved
		teamRoles[tk] = roles
	}

	for _, sk := range sortedKeys(o.Seats) {
		seat := o.Seats[sk]
		name, ok := strings.CutPrefix(seat.RoleRef, "role:")
		if !ok {
			continue
		}
		var found string
		var foundIn string
		for _, tk := range seat.Teams {
			if ref, ok := teamRoles[tk][name]; ok {
				if found != "" && ref != found {
					c.errf("seats."+sk+".role_ref", "ambiguous role %q: teams %s and %s define different instructions", name, foundIn, tk)
				}
				if found == "" {
					found, foundIn = ref, tk
				}
			}
		}
		if found == "" {
			c.errf("seats."+sk+".role_ref", "role %q is not defined by the templates of the seat's teams", name)
			continue
		}
		seat.RoleRef = found
		o.Seats[sk] = seat
	}
	o.TeamTemplates = nil
	if len(lineage) == 0 {
		return nil
	}
	return lineage
}

// checkTimezone requires an IANA time zone name, or empty.
func (c *compiler) checkTimezone(path, tz string) {
	if tz == "" {
		return
	}
	if _, err := time.LoadLocation(tz); err != nil || tz == "Local" {
		c.errf(path, "unknown time zone %q; use an IANA name such as Australia/Melbourne or UTC", tz)
	}
}

func (c *compiler) applyDefaults(o *spec.OrganizationSpec) {
	if o.DataRetention == "" {
		o.DataRetention = "retain"
	}
	if o.Timezone == "" {
		o.Timezone = "UTC"
	}
	for k, ms := range o.MemoryStores {
		if ms.Retention == "" {
			ms.Retention = "retain"
		}
		if ms.BackingClass == "" {
			ms.BackingClass = "postgres"
		}
		o.MemoryStores[k] = ms
	}
	for k, w := range o.SharedWorkspaces {
		if w.Retention == "" {
			w.Retention = "retain"
		}
		if w.SizeGB == 0 {
			w.SizeGB = 5
		}
		o.SharedWorkspaces[k] = w
	}
	for k, e := range o.ExecutionProfiles {
		def := func(p *string, v string) {
			if *p == "" {
				*p = v
			}
		}
		def(&e.ServiceClass, "interactive")
		def(&e.IdlePolicy, "warm_then_stop")
		def(&e.IdleTimeout, "15m")
		def(&e.CPURequest, "250m")
		def(&e.CPULimit, "2")
		def(&e.MemoryRequest, "512Mi")
		def(&e.MemoryLimit, "2Gi")
		if e.WorkspaceSizeGB == 0 {
			e.WorkspaceSizeGB = 5
		}
		o.ExecutionProfiles[k] = e
	}
	for k, s := range o.SandboxProfiles {
		if s.NetworkPolicyRef == "" {
			s.NetworkPolicyRef = "deny_all_except_platform"
		}
		if s.FilesystemPolicyRef == "" {
			s.FilesystemPolicyRef = "workspace_only"
		}
		s.RequiredEnforcement = sortedUnique(append(s.RequiredEnforcement, "network_policy", "non_root", "resource_limits"))
		o.SandboxProfiles[k] = s
	}
	// A connection is required by default only when the organisation cannot
	// work without it (see RequiredConnections); trackers and other optional
	// integrations never block readiness unless declared required.
	usedModel := map[string]bool{}
	for _, s := range o.Seats {
		if h, ok := o.HarnessProfiles[s.HarnessProfile]; ok && h.Model != nil {
			usedModel[h.Model.Connection] = true
		}
	}
	bound := map[string]bool{}
	for _, b := range o.ChannelBindings {
		bound[b.Connection] = true
	}
	for k, conn := range o.Connections {
		if conn.Ownership == "" {
			conn.Ownership = "external"
		}
		if conn.Required == nil {
			kind := c.cat.Connectors[conn.Adapter].Kind
			r := (kind == "model" && usedModel[k]) || (kind == "communication" && bound[k])
			conn.Required = &r
		}
		if info := c.cat.Connectors[conn.Adapter]; info.Kind == "model" {
			m := spec.ModelEndpoint{}
			if conn.Model != nil {
				m = *conn.Model
			}
			if len(m.APIs) == 0 {
				m.APIs = append([]string(nil), info.ModelAPIs...)
			}
			if m.Auth == "" {
				m.Auth = info.DefaultAuth
			}
			if m.Verify == "" {
				m.Verify = "request"
			}
			conn.Model = &m
		}
		o.Connections[k] = conn
	}
	for k, s := range o.Seats {
		if s.Workspace == nil {
			s.Workspace = &spec.WorkspaceBinding{Persistent: true}
		}
		if s.DisplayName == "" {
			s.DisplayName = k
		}
		o.Seats[k] = s
	}
	for k, b := range o.ChannelBindings {
		if b.Mode == "" {
			b.Mode = "direct_message"
		}
		o.ChannelBindings[k] = b
	}
}

func (c *compiler) checkProfiles(o *spec.OrganizationSpec) {
	for _, k := range sortedKeys(o.HarnessProfiles) {
		h := o.HarnessProfiles[k]
		path := "harness_profiles." + k
		info, ok := c.cat.Harnesses[h.Adapter]
		if !ok {
			c.errf(path+".adapter", "unsupported harness adapter %q (supported: %s)", h.Adapter, strings.Join(sortedKeys(c.cat.Harnesses), ", "))
			continue
		}
		if h.ImageDigest == "" {
			c.errf(path+".image_digest", "is required; pin the harness image")
		}
		for _, rc := range h.RequiredCapabilities {
			if !slices.Contains(info.Capabilities, rc) {
				c.errf(path+".required_capabilities", "adapter %q does not support capability %q", h.Adapter, rc)
			}
		}
		if info.NeedsModel && h.Model == nil {
			c.errf(path+".model", "adapter %q requires a model", h.Adapter)
		}
		if h.Model != nil {
			c.checkModel(o, k, h, info)
		}
	}
	for _, k := range sortedKeys(o.ExecutionProfiles) {
		e := o.ExecutionProfiles[k]
		path := "execution_profiles." + k
		be, ok := c.cat.Backends[e.Backend]
		if !ok {
			c.errf(path+".backend", "unsupported backend %q", e.Backend)
			continue
		}
		if e.ServiceClass != "interactive" && e.ServiceClass != "background" {
			c.errf(path+".service_class", "must be interactive or background")
		}
		feature, ok := c.cat.IdlePolicies[e.IdlePolicy]
		if !ok {
			c.errf(path+".idle_policy", "unknown idle policy %q", e.IdlePolicy)
		} else if !slices.Contains(be.Features, feature) {
			c.errf(path+".idle_policy", "backend %q does not support %q (requires feature %q); refusing to substitute a weaker policy", e.Backend, e.IdlePolicy, feature)
		}
		for _, f := range e.RequiredFeatures {
			if !slices.Contains(be.Features, f) {
				c.errf(path+".required_features", "backend %q does not support feature %q", e.Backend, f)
			}
		}
		if d, err := time.ParseDuration(e.IdleTimeout); err != nil || d < time.Minute {
			c.errf(path+".idle_timeout", "must be a duration of at least 1m")
		}
		var req, lim [2]resource.Quantity
		for _, q := range []struct {
			name string
			val  string
			dst  *resource.Quantity
		}{
			{"cpu_request", e.CPURequest, &req[0]}, {"cpu_limit", e.CPULimit, &lim[0]},
			{"memory_request", e.MemoryRequest, &req[1]}, {"memory_limit", e.MemoryLimit, &lim[1]},
		} {
			parsed, err := resource.ParseQuantity(q.val)
			if err != nil {
				c.errf(path+"."+q.name, "invalid quantity %q", q.val)
				continue
			}
			*q.dst = parsed
		}
		for i, n := range []string{"cpu", "memory"} {
			if !req[i].IsZero() && !lim[i].IsZero() && req[i].Cmp(lim[i]) > 0 {
				c.errf(path+"."+n+"_request", "exceeds %s_limit", n)
			}
		}
	}
	for _, k := range sortedKeys(o.SandboxProfiles) {
		s := o.SandboxProfiles[k]
		path := "sandbox_profiles." + k
		if s.NetworkPolicyRef != "deny_all_except_platform" {
			c.errf(path+".network_policy_ref", "unsupported network policy %q (supported: deny_all_except_platform)", s.NetworkPolicyRef)
		}
		if s.FilesystemPolicyRef != "workspace_only" {
			c.errf(path+".filesystem_policy_ref", "unsupported filesystem policy %q (supported: workspace_only)", s.FilesystemPolicyRef)
		}
		be := c.cat.Backends["kubernetes"]
		for _, f := range s.RequiredEnforcement {
			switch {
			case f == "microvm" || f == "gvisor":
				if s.RuntimeClass == "" {
					c.errf(path+".required_enforcement", "%q requires runtime_class naming a RuntimeClass that provides it; a basic container is not equivalent", f)
				}
			case !slices.Contains(be.Features, f):
				c.errf(path+".required_enforcement", "enforcement feature %q is not supported", f)
			}
		}
	}
}

// checkModel validates a harness profile's model selection against the
// harness and the connection, chooses the API when none is given and records
// the selection on the connection for readiness.
func (c *compiler) checkModel(o *spec.OrganizationSpec, key string, h spec.HarnessProfile, info HarnessInfo) {
	path := "harness_profiles." + key + ".model"
	m := *h.Model
	if m.ID == "" {
		c.errf(path+".id", "is required")
	}
	conn, ok := o.Connections[m.Connection]
	if !ok {
		c.errf(path+".connection", "unknown connection %q", m.Connection)
		return
	}
	if ci := c.cat.Connectors[conn.Adapter]; ci.Kind != "model" || conn.Model == nil {
		c.errf(path+".connection", "connection %q uses adapter %q, which is not a model adapter", m.Connection, conn.Adapter)
		return
	}
	served := conn.Model.APIs
	if len(conn.Model.Models) > 0 {
		i := slices.IndexFunc(conn.Model.Models, func(e spec.ModelEntry) bool { return e.ID == m.ID })
		if i < 0 {
			ids := make([]string, len(conn.Model.Models))
			for j, e := range conn.Model.Models {
				ids[j] = e.ID
			}
			c.errf(path+".id", "model %q is not one of connection %q's models (%s)", m.ID, m.Connection, strings.Join(ids, ", "))
			return
		}
		if apis := conn.Model.Models[i].APIs; len(apis) > 0 {
			served = apis
		}
	}
	if m.API != "" {
		switch {
		case !slices.Contains(harnesses.ModelAPIs, m.API):
			c.errf(path+".api", "unknown API %q (known: %s)", m.API, strings.Join(harnesses.ModelAPIs, ", "))
			return
		case !slices.Contains(info.APIs, m.API):
			c.errf(path+".api", "harness %q does not speak %s (it speaks %s)", h.Adapter, m.API, strings.Join(info.APIs, ", "))
			return
		case !slices.Contains(served, m.API):
			c.errf(path+".api", "connection %q does not serve %s for model %q (it serves %s)", m.Connection, m.API, m.ID, strings.Join(served, ", "))
			return
		}
	} else {
		for _, api := range info.APIs {
			if slices.Contains(served, api) {
				m.API = api
				break
			}
		}
		if m.API == "" {
			var would []string
			for _, name := range sortedKeys(c.cat.Harnesses) {
				for _, api := range c.cat.Harnesses[name].APIs {
					if slices.Contains(served, api) {
						would = append(would, name)
						break
					}
				}
			}
			hint := "no registered harness speaks them"
			if len(would) > 0 {
				hint = "harnesses that would work: " + strings.Join(would, ", ")
			}
			c.errf(path, "harness %q speaks %s but connection %q serves %s for model %q; %s",
				h.Adapter, strings.Join(info.APIs, ", "), m.Connection, strings.Join(served, ", "), m.ID, hint)
			return
		}
	}
	for _, sk := range sortedKeys(m.Settings) {
		st, ok := info.Settings[sk]
		switch {
		case !ok:
			c.errf(path+".settings."+sk, "harness %q has no setting %q (settings: %s)", h.Adapter, sk, strings.Join(sortedKeys(info.Settings), ", "))
		case m.Settings[sk] == "":
			c.errf(path+".settings."+sk, "must not be empty")
		case len(st.Values) > 0 && !slices.Contains(st.Values, m.Settings[sk]):
			c.errf(path+".settings."+sk, "must be one of %s", strings.Join(st.Values, ", "))
		}
	}
	h.Model = &m
	o.HarnessProfiles[key] = h
}

// authRe matches a model connection's auth setting.
var authRe = regexp.MustCompile(`^(bearer|x-api-key|header:[A-Za-z0-9-]+)$`)

func (c *compiler) checkModelEndpoint(path string, conn spec.Connection, info ConnectorInfo) {
	m := conn.Model
	if info.Kind != "model" {
		if m != nil {
			c.errf(path+".model", "is only valid on model connections")
		}
		return
	}
	if len(info.ModelAPIs) == 0 && conn.EndpointRef == "" {
		c.errf(path+".endpoint_ref", "is required for adapter %q: the endpoint's API base URL, e.g. https://host/v1", conn.Adapter)
	}
	if len(m.APIs) == 0 {
		c.errf(path+".model.apis", "declare the APIs the endpoint serves (%s)", strings.Join(harnesses.ModelAPIs, ", "))
	}
	for _, api := range m.APIs {
		if !slices.Contains(harnesses.ModelAPIs, api) {
			c.errf(path+".model.apis", "unknown API %q (known: %s)", api, strings.Join(harnesses.ModelAPIs, ", "))
		}
	}
	if !authRe.MatchString(m.Auth) {
		c.errf(path+".model.auth", "must be bearer, x-api-key or header:<Name>")
	}
	switch m.Verify {
	case "request", "models", "none":
	default:
		c.errf(path+".model.verify", "must be request, models or none")
	}
	seen := map[string]bool{}
	for i, e := range m.Models {
		ep := fmt.Sprintf("%s.model.models[%d]", path, i)
		if e.ID == "" {
			c.errf(ep+".id", "is required")
		} else if seen[e.ID] {
			c.errf(ep+".id", "duplicate model %q", e.ID)
		}
		seen[e.ID] = true
		for _, api := range e.APIs {
			if !slices.Contains(m.APIs, api) {
				c.errf(ep+".apis", "%s is not one of the connection's APIs (%s)", api, strings.Join(m.APIs, ", "))
			}
		}
	}
}

func (c *compiler) checkConnections(o *spec.OrganizationSpec) {
	for _, k := range sortedKeys(o.Connections) {
		conn := o.Connections[k]
		path := "connections." + k
		info, ok := c.cat.Connectors[conn.Adapter]
		if !ok {
			c.errf(path+".adapter", "unsupported connector adapter %q (supported: %s)", conn.Adapter, strings.Join(sortedKeys(c.cat.Connectors), ", "))
			continue
		}
		if conn.SecretRef == "" {
			c.errf(path+".secret_ref", "is required")
		} else if !secretRefRe.MatchString(conn.SecretRef) {
			c.errf(path+".secret_ref", "must be vault:<path> or k8s:<secret-name>; raw credentials are never accepted")
		}
		c.checkModelEndpoint(path, conn, info)
		switch conn.Ownership {
		case "external":
		case "managed":
			if !info.ManagedOwnership {
				c.errf(path+".ownership", "adapter %q cannot own external resources; use ownership = \"external\"", conn.Adapter)
			}
		default:
			c.errf(path+".ownership", "must be external or managed")
		}
	}
}

func (c *compiler) checkMemoryAndWorkspaces(o *spec.OrganizationSpec) {
	for _, k := range sortedKeys(o.MemoryStores) {
		ms := o.MemoryStores[k]
		if ms.Retention != "retain" && ms.Retention != "delete" {
			c.errf("memory_stores."+k+".retention", "must be retain or delete")
		}
		if ms.BackingClass != "postgres" {
			c.errf("memory_stores."+k+".backing_class", "unsupported backing class %q (supported: postgres)", ms.BackingClass)
		}
	}
	for _, k := range sortedKeys(o.SharedWorkspaces) {
		w := o.SharedWorkspaces[k]
		if w.AccessMode != "ReadWriteMany" && w.AccessMode != "ReadOnlyMany" {
			c.errf("shared_workspaces."+k+".access_mode", "must be ReadWriteMany or ReadOnlyMany; the storage class must support it")
		}
	}
}

func (c *compiler) checkSeats(o *spec.OrganizationSpec) {
	personalOwner := map[string]string{}
	for _, k := range sortedKeys(o.Seats) {
		s := o.Seats[k]
		path := "seats." + k
		if !strings.HasPrefix(s.RoleRef, "role:") {
			c.checkInstructionRef(path+".role_ref", s.RoleRef)
		}
		for i, r := range s.InstructionRefs {
			c.checkInstructionRef(fmt.Sprintf("%s.instruction_refs[%d]", path, i), r)
		}
		seen := map[string]bool{}
		for _, t := range s.Teams {
			if _, ok := o.Teams[t]; !ok {
				c.errf(path+".teams", "unknown team %q", t)
			}
			if seen[t] {
				c.errf(path+".teams", "team %q listed twice", t)
			}
			seen[t] = true
		}
		if _, ok := o.HarnessProfiles[s.HarnessProfile]; !ok {
			c.errf(path+".harness_profile", "unknown harness profile %q", s.HarnessProfile)
		}
		if _, ok := o.ExecutionProfiles[s.ExecutionProfile]; !ok {
			c.errf(path+".execution_profile", "unknown execution profile %q", s.ExecutionProfile)
		}
		if _, ok := o.SandboxProfiles[s.SandboxProfile]; !ok {
			c.errf(path+".sandbox_profile", "unknown sandbox profile %q", s.SandboxProfile)
		}
		if s.PersonalMemory != "" {
			if _, ok := o.MemoryStores[s.PersonalMemory]; !ok {
				c.errf(path+".personal_memory", "unknown memory store %q", s.PersonalMemory)
			} else if prev, dup := personalOwner[s.PersonalMemory]; dup {
				c.errf(path+".personal_memory", "contradictory ownership: store %q is already the personal memory of seat %q", s.PersonalMemory, prev)
			} else {
				personalOwner[s.PersonalMemory] = k
			}
		}
		for _, ws := range s.Workspace.Shared {
			if _, ok := o.SharedWorkspaces[ws]; !ok {
				c.errf(path+".workspace.shared", "unknown shared workspace %q", ws)
			}
		}
	}
	for _, tk := range sortedKeys(o.Teams) {
		for _, store := range sortedKeys(o.Teams[tk].SharedMemory) {
			path := "teams." + tk + ".shared_memory." + store
			if _, ok := o.MemoryStores[store]; !ok {
				c.errf(path, "unknown memory store %q", store)
			} else if owner, ok := personalOwner[store]; ok {
				c.errf(path, "contradictory ownership: store %q is the personal memory of seat %q and cannot be shared with a team", store, owner)
			}
			c.checkOps(path, o.Teams[tk].SharedMemory[store], MemoryOperations)
		}
	}
	for _, gk := range sortedKeys(o.Grants) {
		g := o.Grants[gk]
		if strings.HasPrefix(g.Subject, "team:") && strings.HasPrefix(g.Resource, "memory:") {
			store := strings.TrimPrefix(g.Resource, "memory:")
			if owner, ok := personalOwner[store]; ok {
				c.errf("grants."+gk+".resource", "contradictory ownership: store %q is the personal memory of seat %q and cannot be granted to a team", store, owner)
			}
		}
	}
}

func (c *compiler) checkOps(path string, ops, allowed []string) {
	if len(ops) == 0 {
		c.errf(path, "at least one operation is required")
	}
	for _, op := range ops {
		if !slices.Contains(allowed, op) {
			c.errf(path, "unsupported operation %q (allowed: %s)", op, strings.Join(allowed, ", "))
		}
	}
}

// Revision is a seat's config revision. Access that applies live (egress
// hosts, repository grants) is left out, so changing it never restarts the
// seat.
func Revision(sm SeatManifest) string {
	sm.ConfigRevision = ""
	sm.Access = sm.Access.PodPart()
	return digest(sm)
}

func (c *compiler) accessContext(o *spec.OrganizationSpec) access.Context {
	return access.Context{Connections: o.Connections}
}

// checkAccess validates access profiles and the seats' and teams' references.
func (c *compiler) checkAccess(o *spec.OrganizationSpec) {
	ctx := c.accessContext(o)
	for _, k := range sortedKeys(o.AccessProfiles) {
		for _, e := range access.Check(ctx, "access_profiles."+k, o.AccessProfiles[k]) {
			c.errf(e.Path, "%s", e.Message)
		}
	}
	ref := func(path string, names []string) {
		for i, n := range names {
			if _, ok := o.AccessProfiles[n]; !ok {
				c.errf(fmt.Sprintf("%s.access_profiles[%d]", path, i), "unknown access profile %q", n)
			}
		}
	}
	for _, k := range sortedKeys(o.Teams) {
		ref("teams."+k, o.Teams[k].AccessProfiles)
	}
	for _, k := range sortedKeys(o.Seats) {
		ref("seats."+k, o.Seats[k].AccessProfiles)
	}
}

func (c *compiler) seatManifest(o *spec.OrganizationSpec, k string) SeatManifest {
	s := o.Seats[k]
	sm := SeatManifest{
		Key:              k,
		DisplayName:      s.DisplayName,
		RoleRef:          s.RoleRef,
		Teams:            s.Teams,
		HarnessProfile:   s.HarnessProfile,
		ExecutionProfile: s.ExecutionProfile,
		SandboxProfile:   s.SandboxProfile,
		Harness:          o.HarnessProfiles[s.HarnessProfile],
		Execution:        o.ExecutionProfiles[s.ExecutionProfile],
		Sandbox:          o.SandboxProfiles[s.SandboxProfile],
		Timezone:         o.Timezone,
		PersonalMemory:   s.PersonalMemory,
		Workspace:        *s.Workspace,
		AdoptFrom:        s.AdoptFrom,
	}
	order := 0
	seen := map[string]bool{}
	add := func(ref, scope string) {
		if seen[ref] {
			return
		}
		seen[ref] = true
		sm.Instructions = append(sm.Instructions, InstructionSource{Ref: ref, Scope: scope, Order: order})
		order++
	}
	for _, r := range o.CultureRefs {
		add(r, "organisation")
	}
	for _, t := range s.Teams {
		for _, r := range o.Teams[t].InstructionRefs {
			add(r, "team:"+t)
		}
	}
	add(s.RoleRef, "role")
	for _, r := range s.InstructionRefs {
		add(r, "seat")
	}
	if s.PersonalMemory != "" {
		sm.addCapability("memory:"+s.PersonalMemory, PersonalMemoryOperations, nil, "implicit:personal_memory")
	}
	if m := sm.Harness.Model; m != nil {
		sm.addCapability("connection:"+m.Connection, []string{"model.infer"}, nil, "implicit:harness_model")
	}
	profiles := append([]string(nil), s.AccessProfiles...)
	for _, t := range s.Teams {
		profiles = append(profiles, o.Teams[t].AccessProfiles...)
	}
	sm.Access = access.Resolve(c.accessContext(o), o.AccessProfiles, profiles)
	if sm.Access != nil {
		for _, g := range sm.Access.GitHub {
			// Repository operations through the gateway, limited to the
			// granted repositories, whatever the delivery.
			if ops := access.GitHubOperations(g.Permissions); len(ops) > 0 {
				sm.addCapability("connection:"+g.Connection, ops, g.Repos, "implicit:"+g.Source)
			}
		}
	}
	for _, t := range s.Teams {
		for _, store := range sortedKeys(o.Teams[t].SharedMemory) {
			sm.addCapability("memory:"+store, o.Teams[t].SharedMemory[store], nil, "team:"+t+"/shared_memory")
		}
	}
	return sm
}

func (sm *SeatManifest) addCapability(res string, ops, targets []string, source string) {
	for i := range sm.Capabilities {
		if sm.Capabilities[i].Resource == res {
			c := &sm.Capabilities[i]
			c.Operations = sortedUnique(append(c.Operations, ops...))
			c.Targets = sortedUnique(append(c.Targets, targets...))
			c.Sources = sortedUnique(append(c.Sources, source))
			return
		}
	}
	sm.Capabilities = append(sm.Capabilities, Capability{
		Resource: res, Operations: sortedUnique(ops), Targets: sortedUnique(targets), Sources: []string{source},
	})
	sort.Slice(sm.Capabilities, func(i, j int) bool { return sm.Capabilities[i].Resource < sm.Capabilities[j].Resource })
}

// expandSubject returns the seat keys denoted by seat:<k> or team:<k>.
func (c *compiler) expandSubject(o *spec.OrganizationSpec, path, subject string) []string {
	kind, key, ok := strings.Cut(subject, ":")
	if !ok {
		c.errf(path, "must be seat:<key> or team:<key>")
		return nil
	}
	switch kind {
	case "seat":
		if _, ok := o.Seats[key]; !ok {
			c.errf(path, "unknown seat %q", key)
			return nil
		}
		return []string{key}
	case "team":
		if _, ok := o.Teams[key]; !ok {
			c.errf(path, "unknown team %q", key)
			return nil
		}
		var out []string
		for _, sk := range sortedKeys(o.Seats) {
			if slices.Contains(o.Seats[sk].Teams, key) {
				out = append(out, sk)
			}
		}
		return out
	default:
		c.errf(path, "must be seat:<key> or team:<key>")
		return nil
	}
}

func (c *compiler) applyGrants(o *spec.OrganizationSpec, seats map[string]SeatManifest) {
	for _, gk := range sortedKeys(o.Grants) {
		g := o.Grants[gk]
		path := "grants." + gk
		subjects := c.expandSubject(o, path+".subject", g.Subject)
		kind, key, _ := strings.Cut(g.Resource, ":")
		switch kind {
		case "memory":
			if _, ok := o.MemoryStores[key]; !ok {
				c.errf(path+".resource", "unknown memory store %q", key)
				continue
			}
			c.checkOps(path+".operations", g.Operations, MemoryOperations)
		case "connection":
			conn, ok := o.Connections[key]
			if !ok {
				c.errf(path+".resource", "unknown connection %q", key)
				continue
			}
			c.checkOps(path+".operations", g.Operations, c.cat.Connectors[conn.Adapter].Operations)
		case "workspace":
			if _, ok := o.SharedWorkspaces[key]; !ok {
				c.errf(path+".resource", "unknown shared workspace %q", key)
				continue
			}
			c.checkOps(path+".operations", g.Operations, WorkspaceOperations)
		default:
			c.errf(path+".resource", "must be memory:<key>, connection:<key> or workspace:<key>")
			continue
		}
		for _, sk := range subjects {
			sm := seats[sk]
			sm.addCapability(g.Resource, g.Operations, g.Targets, "grant:"+gk)
			seats[sk] = sm
		}
	}
	// A seat that mounts a shared workspace must hold an explicit grant to it.
	for _, sk := range sortedKeys(seats) {
		for _, ws := range seats[sk].Workspace.Shared {
			if !seats[sk].hasCapability("workspace:" + ws) {
				c.errf("seats."+sk+".workspace.shared", "seat mounts shared workspace %q without a grant; team membership does not imply filesystem access", ws)
			}
		}
	}
}

func (sm SeatManifest) hasCapability(res string) bool {
	for _, c := range sm.Capabilities {
		if c.Resource == res {
			return true
		}
	}
	return false
}

func addEdge(edges []RouteEdge, seat string, reply bool, route string) []RouteEdge {
	for i := range edges {
		if edges[i].Seat == seat {
			edges[i].Reply = edges[i].Reply || reply
			edges[i].Routes = sortedUnique(append(edges[i].Routes, route))
			return edges
		}
	}
	edges = append(edges, RouteEdge{Seat: seat, Reply: reply, Routes: []string{route}})
	sort.Slice(edges, func(i, j int) bool { return edges[i].Seat < edges[j].Seat })
	return edges
}

func (c *compiler) applyRoutes(o *spec.OrganizationSpec, seats map[string]SeatManifest) {
	for _, rk := range sortedKeys(o.MessageRoutes) {
		r := o.MessageRoutes[rk]
		path := "message_routes." + rk
		from := c.expandSubject(o, path+".from", r.From)
		to := c.expandSubject(o, path+".to", r.To)
		link := func(senders, recipients []string, reply bool) {
			for _, s := range senders {
				for _, t := range recipients {
					if s == t {
						continue
					}
					ss, ts := seats[s], seats[t]
					ss.SendTo = addEdge(ss.SendTo, t, reply, rk)
					ts.ReceiveFrom = addEdge(ts.ReceiveFrom, s, reply, rk)
					seats[s], seats[t] = ss, ts
				}
			}
		}
		link(from, to, r.Reply || r.Bidirectional)
		if r.Bidirectional {
			link(to, from, true)
		}
	}
}

func (c *compiler) applyBindings(o *spec.OrganizationSpec, seats map[string]SeatManifest) {
	humanOf := map[string]string{}
	zoneOf := map[string]string{}
	identity := map[string]string{}
	for _, bk := range sortedKeys(o.ChannelBindings) {
		b := o.ChannelBindings[bk]
		path := "channel_bindings." + bk
		conn, ok := o.Connections[b.Connection]
		if !ok {
			c.errf(path+".connection", "unknown connection %q", b.Connection)
			continue
		}
		if c.cat.Connectors[conn.Adapter].Kind != "communication" {
			c.errf(path+".connection", "connection %q is not a communication adapter", b.Connection)
			continue
		}
		if _, ok := o.Seats[b.Seat]; !ok {
			c.errf(path+".seat", "unknown seat %q", b.Seat)
			continue
		}
		if strings.TrimSpace(b.ExternalUserID) == "" {
			c.errf(path+".external_user_id", "is required")
			continue
		}
		if b.Mode != "direct_message" {
			c.errf(path+".mode", "unsupported mode %q (supported: direct_message)", b.Mode)
		}
		id := b.Connection + "/" + b.ExternalUserID
		if prev, ok := identity[id]; ok {
			c.errf(path+".external_user_id", "external identity is already bound by %q", prev)
			continue
		}
		identity[id] = bk
		if prev, ok := humanOf[b.Seat]; ok && prev != b.ExternalUserID {
			c.errf(path+".seat", "seat %q already represents a different human; representatives keep separate private histories", b.Seat)
			continue
		}
		humanOf[b.Seat] = b.ExternalUserID
		c.checkTimezone(path+".timezone", b.Timezone)
		if prev, ok := zoneOf[b.Seat]; ok && b.Timezone != prev {
			c.errf(path+".timezone", "seat %q's other binding gives the time zone %q; one human has one time zone", b.Seat, prev)
			continue
		}
		zoneOf[b.Seat] = b.Timezone
		sm := seats[b.Seat]
		if b.Timezone != "" {
			sm.Timezone = b.Timezone
		}
		sm.ChannelBindings = append(sm.ChannelBindings, bk)
		sm.IsRepresentative = true
		sm.addCapability("connection:"+b.Connection, []string{"channel.reply"}, []string{b.ExternalUserID}, "implicit:channel_binding/"+bk)
		seats[b.Seat] = sm
	}
}

func digest(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func deepCopySpec(in spec.OrganizationSpec) spec.OrganizationSpec {
	var out spec.OrganizationSpec
	b, err := json.Marshal(in)
	if err != nil {
		panic(err)
	}
	if err := json.Unmarshal(b, &out); err != nil {
		panic(err)
	}
	return out
}

func appendUnique(list []string, vals ...string) []string {
	for _, v := range vals {
		if !slices.Contains(list, v) {
			list = append(list, v)
		}
	}
	return list
}

func sortedUnique(vals []string) []string {
	if len(vals) == 0 {
		return nil
	}
	out := slices.Clone(vals)
	sort.Strings(out)
	return slices.Compact(out)
}

func sameSet(a, b []string) bool {
	return slices.Equal(sortedUnique(a), sortedUnique(b))
}

func (c *compiler) checkWorkPublication(o *spec.OrganizationSpec) {
	p := o.WorkPublication
	if p == nil {
		return
	}
	conn, ok := o.Connections[p.Connection]
	switch {
	case !ok:
		c.errf("work_publication.connection", "connection %q is not declared", p.Connection)
	case c.cat.Connectors[conn.Adapter].Kind != "tracker":
		c.errf("work_publication.connection", "connection %q is not a work tracker", p.Connection)
	}
	if len(p.Stores) == 0 {
		c.errf("work_publication.stores", "name at least one shared memory store")
	}
	personal := map[string]string{}
	for k, s := range o.Seats {
		if s.PersonalMemory != "" {
			personal[s.PersonalMemory] = k
		}
	}
	for i, st := range p.Stores {
		path := fmt.Sprintf("work_publication.stores[%d]", i)
		if _, ok := o.MemoryStores[st]; !ok {
			c.errf(path, "memory store %q is not declared", st)
		} else if seat, ok := personal[st]; ok {
			c.errf(path, "%q is the personal store of seat %s; personal memory is never published", st, seat)
		}
	}
}
