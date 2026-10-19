package compile

import (
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/darcys22/steadmesh/pkg/spec"
)

func (c *compiler) checkTeamPolicies(o *spec.OrganizationSpec) {
	stores := map[string]string{}
	for _, key := range sortedKeys(o.Teams) {
		team := o.Teams[key]
		path := "teams." + key
		if p := team.Coordination; p != nil {
			if p.Mode == "" {
				p.Mode = "declared"
			}
			if p.ReviewPolicy == "" {
				p.ReviewPolicy = "none"
			}
			if p.MaxMessagesPerExecution == 0 {
				p.MaxMessagesPerExecution = 20
			}
			if p.Mode != "declared" && p.Mode != "self_organising" {
				c.errf(path+".coordination.mode", "must be declared or self_organising")
			}
			if p.ReviewPolicy != "none" && p.ReviewPolicy != "required" {
				c.errf(path+".coordination.review_policy", "must be none or required")
			}
			if len(p.Mission) > 8192 || !utf8.ValidString(p.Mission) {
				c.errf(path+".coordination.mission", "must be UTF-8 and at most 8192 bytes")
			}
			if p.MaxMessagesPerExecution < 1 || p.MaxMessagesPerExecution > 1000 {
				c.errf(path+".coordination.max_messages_per_execution", "must be between 1 and 1000")
			}
			seen := map[string]bool{}
			for _, subject := range p.MissionEditors {
				if seen[subject] {
					c.errf(path+".coordination.mission_editors", "duplicate subject %q", subject)
				}
				seen[subject] = true
				if subject == "team:"+key {
					continue
				}
				sk, ok := strings.CutPrefix(subject, "seat:")
				seat, exists := o.Seats[sk]
				if !ok || !exists || !slices.Contains(seat.Teams, key) {
					c.errf(path+".coordination.mission_editors", "subject %q must be this team or an existing member seat", subject)
				}
			}
			if p.WorkStore != "" {
				if owner, ok := stores[p.WorkStore]; ok {
					c.errf(path+".coordination.work_store", "already used by team %q", owner)
				}
				stores[p.WorkStore] = key
				if _, ok := o.MemoryStores[p.WorkStore]; !ok {
					c.errf(path+".coordination.work_store", "unknown memory store %q", p.WorkStore)
				}
				ops := append([]string{}, team.SharedMemory[p.WorkStore]...)
				for _, g := range o.Grants {
					if g.Subject == "team:"+key && g.Resource == "memory:"+p.WorkStore && len(g.Targets) == 0 {
						ops = append(ops, g.Operations...)
					}
				}
				for _, op := range []string{"read", "write", "revise"} {
					if !slices.Contains(ops, op) {
						c.errf(path+".coordination.work_store", "requires explicit team %s grant on shared store", op)
					}
				}
			} else if p.ReviewPolicy == "required" {
				c.errf(path+".coordination.work_store", "required review needs an explicitly granted work store")
			}
		}
		if p := team.Context; p != nil {
			if p.RequireInstructionApproval && (!p.EditableInstructions || len(p.InstructionApprovers) == 0) {
				c.errf(path+".context.require_instruction_approval", "requires editable_instructions and nonempty instruction_approvers")
			}
			seen := map[string]bool{}
			for _, subject := range p.InstructionApprovers {
				if seen[subject] {
					c.errf(path+".context.instruction_approvers", "duplicate subject %q", subject)
				}
				seen[subject] = true
				if subject == "team:"+key {
					continue
				}
				sk, ok := strings.CutPrefix(subject, "seat:")
				seat, exists := o.Seats[sk]
				if !ok || !exists || !slices.Contains(seat.Teams, key) {
					c.errf(path+".context.instruction_approvers", "subject %q must be this team or an existing member seat", subject)
				}
			}
		}
		p := team.Pool
		if p == nil {
			continue
		}
		if p.IdleTimeoutSeconds == 0 {
			p.IdleTimeoutSeconds = 900
		}
		if p.DrainTimeoutSeconds == 0 {
			p.DrainTimeoutSeconds = 300
		}
		for _, limit := range []struct {
			name            string
			value, min, max int
		}{{"min_members", p.MinMembers, 0, 1024}, {"max_members", p.MaxMembers, 1, 1024}, {"idle_timeout_seconds", p.IdleTimeoutSeconds, 1, 86400}, {"drain_timeout_seconds", p.DrainTimeoutSeconds, 1, 3600}, {"retention_seconds", p.RetentionSeconds, 0, 2592000}} {
			if limit.value < limit.min || limit.value > limit.max {
				c.errf(path+".pool."+limit.name, "must be between %d and %d", limit.min, limit.max)
			}
		}
		if p.MinMembers > p.MaxMembers {
			c.errf(path+".pool.min_members", "must not exceed max_members")
		}
		if len(p.Templates) == 0 {
			c.errf(path+".pool.templates", "at least one template is required")
		}
		if o.ModelBudgets == nil || o.ModelBudgets.OrganizationDailyOutputTokens <= 0 || o.ModelBudgets.ExecutionOutputTokens <= 0 {
			c.errf(path+".pool", "requires positive organization and execution output-token budgets")
		}
		for _, name := range sortedKeys(p.Templates) {
			t := p.Templates[name]
			tp := path + ".pool.templates." + name
			if !keyPattern.MatchString(name) {
				c.errf(tp, "invalid template key")
			}
			if t.Seat == nil {
				c.errf(tp+".seat", "explicit seat configuration is required")
				continue
			}
			if strings.TrimSpace(t.Instructions) == "" || len(t.Instructions) > 8192 || !utf8.ValidString(t.Instructions) {
				c.errf(tp+".instructions", "must be nonempty UTF-8 and at most 8192 bytes")
			}
			if len(t.ToolNames) > 0 || t.HarnessProfile != "" || t.ExecutionProfile != "" {
				c.errf(tp, "native pools use seat profiles and explicit grants, not legacy tool/profile selectors")
			}
			if t.Seat.PersonalMemory != "" || t.Seat.AdoptFrom != "" {
				c.errf(tp+".seat", "pool members cannot share fixed personal memory or adopt retained identities")
			}
			// Validate full normal authority without adding runtime membership to output.
			probe := deepCopySpec(*o)
			for tk, tt := range probe.Teams {
				tt.Pool = nil
				probe.Teams[tk] = tt
			}
			sk := "pool_validation"
			for {
				if _, ok := probe.Seats[sk]; !ok {
					break
				}
				sk += "x"
			}
			seat := *t.Seat.DeepCopy()
			if !slices.Contains(seat.Teams, key) {
				seat.Teams = append(seat.Teams, key)
			}
			if probe.Seats == nil {
				probe.Seats = map[string]spec.Seat{}
			}
			probe.Seats[sk] = seat
			if _, err := Compile(probe, c.cat); err != nil {
				c.errf(tp+".seat", "invalid participant: %v", err)
			}
		}
	}
}

// CompilePoolSeat compiles one independently authorized runtime identity. The
// caller may include other live members in in.Seats for ordinary route expansion.
// The input and declared membership are never mutated.
func CompilePoolSeat(in spec.OrganizationSpec, cat Catalog, teamKey, templateKey, seatKey string) (*SeatManifest, error) {
	if !keyPattern.MatchString(seatKey) {
		return nil, fmt.Errorf("invalid pool seat key %q", seatKey)
	}
	if _, exists := in.Seats[seatKey]; exists {
		return nil, fmt.Errorf("pool seat key %q already exists", seatKey)
	}
	team, ok := in.Teams[teamKey]
	if !ok || team.Pool == nil {
		return nil, fmt.Errorf("unknown pool %q", teamKey)
	}
	t, ok := team.Pool.Templates[templateKey]
	if !ok || t.Seat == nil {
		return nil, fmt.Errorf("unknown pool seat template %q", templateKey)
	}
	out := deepCopySpec(in)
	seat := *t.Seat.DeepCopy()
	if !slices.Contains(seat.Teams, teamKey) {
		seat.Teams = append(seat.Teams, teamKey)
	}
	if out.Seats == nil {
		out.Seats = map[string]spec.Seat{}
	}
	out.Seats[seatKey] = seat
	m, err := Compile(out, cat)
	if err != nil {
		return nil, err
	}
	sm := m.Seats[seatKey]
	sm.PoolTeam = teamKey
	sm.ConfigRevision = Revision(sm)
	return &sm, nil
}
