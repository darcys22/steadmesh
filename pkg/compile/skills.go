package compile

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/darcys22/steadmesh/pkg/spec"
)

var (
	skillRepositoryPattern = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,37}[A-Za-z0-9])?/[A-Za-z0-9_.-]{1,100}$`)
	skillCommitPattern     = regexp.MustCompile(`^[a-f0-9]{40}$`)
	skillIdentifierPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,127}$`)
)

func (c *compiler) checkSkills(o *spec.OrganizationSpec) {
	if len(o.SkillMarketplaces) > spec.MaxOrganizationSkillMarketplaces {
		c.errf("skill_marketplaces", "must contain at most %d sources", spec.MaxOrganizationSkillMarketplaces)
	}
	for _, key := range sortedKeys(o.SkillMarketplaces) {
		source := o.SkillMarketplaces[key]
		path := "skill_marketplaces." + key
		_, repo, _ := strings.Cut(source.Repository, "/")
		if !skillRepositoryPattern.MatchString(source.Repository) || repo == "." || repo == ".." {
			c.errf(path+".repository", "must be a public GitHub owner/repo, not a URL or path")
		}
		if !skillCommitPattern.MatchString(source.Ref) {
			c.errf(path+".ref", "must be a full lowercase 40-hex commit SHA; branches and tags are not allowed")
		}
	}
	for _, key := range sortedKeys(o.Seats) {
		seat := o.Seats[key]
		p := seat.Skills
		if p == nil {
			continue
		}
		path := "seats." + key + ".skills"
		switch o.HarnessProfiles[seat.HarnessProfile].Adapter {
		case "claude-code", "codex", "pi":
		default:
			c.errf(path, "marketplace skills require a claude-code, codex or pi harness")
		}
		if p.Mode == "" {
			p.Mode = spec.SkillModeExplicit
		}
		switch p.Mode {
		case spec.SkillModeExplicit, spec.SkillModeDiscover:
			if p.MaxDynamicSkills != 0 {
				c.errf(path+".max_dynamic_skills", "is only valid in agent_choice mode")
			}
		case spec.SkillModeAgentChoice:
			if p.MaxDynamicSkills == 0 {
				p.MaxDynamicSkills = spec.DefaultMaxDynamicSkills
			}
			if p.MaxDynamicSkills < 1 || p.MaxDynamicSkills > spec.MaxDynamicSkills {
				c.errf(path+".max_dynamic_skills", "must be between 1 and %d", spec.MaxDynamicSkills)
			}
		default:
			c.errf(path+".mode", "must be explicit, discover or agent_choice")
		}
		if len(p.Marketplaces) == 0 {
			c.errf(path+".marketplaces", "at least one explicitly allowed marketplace is required")
		}
		if len(p.Marketplaces) > spec.MaxSeatSkillMarketplaces {
			c.errf(path+".marketplaces", "must contain at most %d marketplaces", spec.MaxSeatSkillMarketplaces)
		}
		if len(p.Install) > spec.MaxStaticSkills {
			c.errf(path+".install", "must contain at most %d individual skills", spec.MaxStaticSkills)
		}
		allowed := map[string]bool{}
		for i, name := range p.Marketplaces {
			field := fmt.Sprintf("%s.marketplaces[%d]", path, i)
			if allowed[name] {
				c.errf(field, "duplicate marketplace %q", name)
			}
			allowed[name] = true
			if _, ok := o.SkillMarketplaces[name]; !ok {
				c.errf(field, "unknown marketplace %q", name)
			}
		}
		seen := map[spec.SkillRef]bool{}
		for i, ref := range p.Install {
			field := fmt.Sprintf("%s.install[%d]", path, i)
			if !allowed[ref.Marketplace] {
				c.errf(field+".marketplace", "marketplace %q must be in this seat's marketplaces allowlist", ref.Marketplace)
			}
			if !skillIdentifierPattern.MatchString(ref.Plugin) {
				c.errf(field+".plugin", "must match %s", skillIdentifierPattern.String())
			}
			if !skillIdentifierPattern.MatchString(ref.Skill) {
				c.errf(field+".skill", "must match %s", skillIdentifierPattern.String())
			}
			if seen[ref] {
				c.errf(field, "duplicate skill reference")
			}
			seen[ref] = true
		}
	}
}
