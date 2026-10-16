package compile

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/darcys22/steadmesh/pkg/spec"
)

func (c *compiler) checkElasticTeams(o *spec.OrganizationSpec) {
	p := o.ElasticTeams
	if p == nil {
		return
	}
	if o.ModelBudgets == nil || o.ModelBudgets.OrganizationDailyOutputTokens <= 0 || o.ModelBudgets.ExecutionOutputTokens <= 0 {
		c.errf("elastic_teams", "requires positive model_budgets.organization_daily_output_tokens and execution_output_tokens")
	}
	for _, limit := range []struct {
		name       string
		value, max int
	}{
		{"max_active_workers", p.MaxActiveWorkers, 1024},
		{"max_active_per_execution", p.MaxActivePerExecution, 32},
		{"max_launches_per_execution", p.MaxLaunchesPerExecution, 128},
		{"max_timeout_seconds", p.MaxTimeoutSeconds, 1200},
	} {
		if limit.value < 1 || limit.value > limit.max {
			c.errf("elastic_teams."+limit.name, "must be between 1 and %d", limit.max)
		}
	}
	if len(p.EnabledSeats) == 0 {
		c.errf("elastic_teams.enabled_seats", "at least one seat is required")
	}
	seen := map[string]bool{}
	for i, k := range p.EnabledSeats {
		path := fmt.Sprintf("elastic_teams.enabled_seats[%d]", i)
		if _, ok := o.Seats[k]; !ok {
			c.errf(path, "unknown seat %q", k)
		}
		if seen[k] {
			c.errf(path, "duplicate seat %q", k)
		}
		seen[k] = true
	}
	if len(p.Templates) == 0 {
		c.errf("elastic_teams.templates", "at least one template is required")
	}
	for _, k := range sortedKeys(p.Templates) {
		t := p.Templates[k]
		path := "elastic_teams.templates." + k
		if !keyPattern.MatchString(k) || len(k) > 80 {
			c.errf(path, "key must match %s and be at most 80 bytes", keyPattern.String())
		}
		if strings.TrimSpace(t.Instructions) == "" || len(t.Instructions) > 8192 || !utf8.ValidString(t.Instructions) {
			c.errf(path+".instructions", "must be nonempty UTF-8 and at most 8192 bytes")
		}
		if _, ok := o.HarnessProfiles[t.HarnessProfile]; !ok && t.HarnessProfile != "" {
			c.errf(path+".harness_profile", "unknown harness profile %q", t.HarnessProfile)
		}
		if _, ok := o.ExecutionProfiles[t.ExecutionProfile]; !ok && t.ExecutionProfile != "" {
			c.errf(path+".execution_profile", "unknown execution profile %q", t.ExecutionProfile)
		}
		for _, seatKey := range p.EnabledSeats {
			seat, ok := o.Seats[seatKey]
			if !ok {
				continue
			}
			if t.HarnessProfile != "" && t.HarnessProfile != seat.HarnessProfile {
				c.errf(path+".harness_profile", "must match enabled seat %q profile %q; workers inherit the requester model", seatKey, seat.HarnessProfile)
			}
			if t.ExecutionProfile != "" && t.ExecutionProfile != seat.ExecutionProfile {
				c.errf(path+".execution_profile", "must match enabled seat %q profile %q; workers inherit pinned runtime resources", seatKey, seat.ExecutionProfile)
			}
		}
		tools := map[string]bool{}
		for _, name := range t.ToolNames {
			switch name {
			case "memory.stores", "memory.list", "memory.read", "memory.search", "memory.history", "work.get", "work.list":
			default:
				c.errf(path+".tool_names", "tool %q is not read-only delegable", name)
			}
			if tools[name] {
				c.errf(path+".tool_names", "duplicate tool %q", name)
			}
			tools[name] = true
		}
	}
}
