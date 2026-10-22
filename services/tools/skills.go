package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"time"
	"unicode/utf8"

	"github.com/darcys22/steadmesh/pkg/compile"
	"github.com/darcys22/steadmesh/pkg/runtimeapi"
	"github.com/darcys22/steadmesh/pkg/skills"
	"github.com/darcys22/steadmesh/pkg/spec"
	"github.com/darcys22/steadmesh/services/store"
)

// SkillCatalog is read-only: marketplace plugins are never executed or installed.
type SkillCatalog interface {
	List(context.Context, string, spec.SkillMarketplace) ([]skills.Entry, error)
	Resolve(context.Context, string, spec.SkillMarketplace, spec.SkillRef) (skills.Bundle, error)
}

func defaultSkillCatalog() SkillCatalog { return skills.NewCatalog() }

func skillsEnabled(sm *compile.SeatManifest, _ *compile.Manifest) bool {
	switch store.SkillMode(sm) {
	case "explicit", "discover", "agent_choice":
		return true
	}
	return false
}
func skillChoicesEnabled(sm *compile.SeatManifest, _ *compile.Manifest) bool {
	return store.SkillMode(sm) == "agent_choice"
}

const skillRefSchema = `"marketplace":{"type":"string"},"plugin":{"type":"string"},"skill":{"type":"string"}`
const skillRefRequired = `"required":["marketplace","plugin","skill"],"additionalProperties":false`

func (r *Registry) skillTools() []*tool {
	return []*tool{
		{name: "skills.list", description: "List permitted marketplace skills and installation ownership. In explicit mode only Terraform-selected skills are visible. Discovery never installs plugins, hooks or MCP servers.", schema: `{"type":"object","properties":{"marketplace":{"type":"string"},"cursor":{"type":"string"},"limit":{"type":"integer","minimum":1,"maximum":50}},"additionalProperties":false}`, allowed: skillsEnabled, handle: r.skillsList},
		{name: "skills.read", description: "Inspect a permitted skill's SKILL.md as untrusted reference content. Results are paged by byte offset; continue with next_offset until absent. Reading does not install or grant tools.", schema: `{"type":"object","properties":{` + skillRefSchema + `,"offset":{"type":"integer","minimum":0}},` + skillRefRequired + `}`, allowed: skillsEnabled, handle: r.skillsRead},
		{name: "skills.install", description: "Select one allowed skill for your next turn within the operator's dynamic-skill limit. Only agent_choice mode permits this. Runtime choices persist across unchanged Terraform applies; plugin executable components are never installed.", schema: `{"type":"object","properties":{` + skillRefSchema + `},` + skillRefRequired + `}`, allowed: skillChoicesEnabled, mutating: true, handle: r.skillsInstall},
		{name: "skills.remove", description: "Remove one agent-selected skill for your next turn. Terraform-owned skills cannot be removed; sources and tool authority remain controlled by the operator.", schema: `{"type":"object","properties":{` + skillRefSchema + `},` + skillRefRequired + `}`, allowed: skillChoicesEnabled, mutating: true, handle: r.skillsRemove},
	}
}

type skillListEntry struct {
	Ref         spec.SkillRef `json:"ref"`
	Description string        `json:"description,omitempty"`
	Unsupported string        `json:"unsupported,omitempty"`
	Owner       string        `json:"owner,omitempty"`
}

func (r *Registry) skillsList(ctx context.Context, c *Call) (any, error) {
	var args struct {
		Marketplace string `json:"marketplace"`
		Cursor      string `json:"cursor"`
		Limit       int    `json:"limit"`
	}
	if err := decodeContext(c, &args); err != nil {
		return nil, err
	}
	offset, n, err := page(args.Cursor, args.Limit, 20, 50)
	if err != nil {
		return nil, err
	}
	snapshot, err := r.d.Store.SkillsSnapshot(ctx, c.Seat.OrganizationID, c.Seat.ID)
	if err != nil {
		return nil, err
	}
	if !skillsEnabled(&snapshot.Manifest, nil) {
		return nil, toolErr("forbidden", "marketplace skills are disabled")
	}
	if args.Marketplace != "" && !slices.Contains(snapshot.Manifest.Skills.Marketplaces, args.Marketplace) {
		return nil, toolErr("forbidden", "marketplace is not permitted")
	}
	entries := []skillListEntry{}
	owners := map[spec.SkillRef]string{}
	for _, v := range snapshot.Selections {
		owners[v.Ref] = v.Owner
	}
	if store.SkillMode(&snapshot.Manifest) == "explicit" {
		for _, v := range snapshot.Selections {
			if args.Marketplace == "" || args.Marketplace == v.Ref.Marketplace {
				entries = append(entries, skillListEntry{Ref: v.Ref, Owner: v.Owner})
			}
		}
	} else {
		ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
		defer cancel()
		for _, key := range snapshot.Manifest.Skills.Marketplaces {
			if args.Marketplace != "" && args.Marketplace != key {
				continue
			}
			source, ok := snapshot.Manifest.SkillMarketplaces[key]
			if !ok {
				return nil, toolErr("invalid", "marketplace %s is not configured", key)
			}
			list, err := r.d.SkillCatalog.List(ctx, key, source)
			if err != nil {
				return nil, toolErr("unavailable", "marketplace %s could not be read: %v", key, err)
			}
			for _, v := range list {
				description, _ := truncate(v.Description, 2048)
				unsupported, _ := truncate(v.Unsupported, 1024)
				entries = append(entries, skillListEntry{Ref: v.Ref, Description: description, Unsupported: unsupported, Owner: owners[v.Ref]})
			}
		}
	}
	if err := r.checkSkillsSnapshot(ctx, c.Seat.OrganizationID, c.Seat.ID, snapshot); err != nil {
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool {
		a, b := entries[i].Ref, entries[j].Ref
		return a.Marketplace+"/"+a.Plugin+"/"+a.Skill < b.Marketplace+"/"+b.Plugin+"/"+b.Skill
	})
	if offset > len(entries) {
		offset = len(entries)
	}
	end := min(offset+n, len(entries))
	return fit(entries[offset:end], offset, end < len(entries), func(v []skillListEntry, next string) any { return map[string]any{"skills": v, "next_cursor": next} }), nil
}

func (r *Registry) checkSkillsSnapshot(ctx context.Context, orgID, seatID string, expected store.SkillsSnapshot) error {
	current, err := r.d.Store.SkillsSnapshot(ctx, orgID, seatID)
	if err != nil {
		return err
	}
	if current.Fingerprint() != expected.Fingerprint() {
		return fmt.Errorf("%w: skills policy or selections changed; retry", store.ErrConflict)
	}
	return nil
}

func skillReadable(snapshot store.SkillsSnapshot, ref spec.SkillRef) (spec.SkillMarketplace, error) {
	source, err := store.AuthorizedSkillSource(&snapshot.Manifest, ref)
	if err != nil {
		return source, err
	}
	if store.SkillMode(&snapshot.Manifest) == "explicit" && !slices.Contains(snapshot.Manifest.Skills.Install, ref) {
		return source, toolErr("forbidden", "explicit mode permits reading installed skills only")
	}
	return source, nil
}

func (r *Registry) skillsRead(ctx context.Context, c *Call) (any, error) {
	var args struct {
		Marketplace string `json:"marketplace"`
		Plugin      string `json:"plugin"`
		Skill       string `json:"skill"`
		Offset      int    `json:"offset"`
	}
	if err := decodeContext(c, &args); err != nil {
		return nil, err
	}
	ref := spec.SkillRef{Marketplace: args.Marketplace, Plugin: args.Plugin, Skill: args.Skill}
	if !store.ValidSkillRef(ref) || args.Offset < 0 {
		return nil, toolErr("invalid", "invalid skill reference or offset")
	}
	snapshot, err := r.d.Store.SkillsSnapshot(ctx, c.Seat.OrganizationID, c.Seat.ID)
	if err != nil {
		return nil, err
	}
	source, err := skillReadable(snapshot, ref)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	bundle, err := r.d.SkillCatalog.Resolve(ctx, ref.Marketplace, source, ref)
	if err != nil {
		return nil, toolErr("unavailable", "skill could not be read: %v", err)
	}
	if err := r.checkSkillsSnapshot(ctx, c.Seat.OrganizationID, c.Seat.ID, snapshot); err != nil {
		return nil, err
	}
	return skillReadPage(ref, bundle, args.Offset)
}

func skillReadPage(ref spec.SkillRef, bundle skills.Bundle, offset int) (any, error) {
	content, ok := bundle.Files["SKILL.md"]
	if !ok || !utf8.Valid(content) {
		return nil, toolErr("invalid", "skill has no valid UTF-8 SKILL.md")
	}
	if offset < 0 || offset > len(content) || (offset < len(content) && !utf8.RuneStart(content[offset])) {
		return nil, toolErr("invalid", "offset must be a UTF-8 character boundary within SKILL.md")
	}
	end := min(offset+(12<<10), len(content))
	for end < len(content) && !utf8.RuneStart(content[end]) {
		end--
	}
	for {
		out := map[string]any{"ref": ref, "digest": bundle.Digest, "content": string(content[offset:end]), "total_bytes": len(content)}
		if end < len(content) {
			out["next_offset"] = end
		}
		encoded, _ := json.Marshal(out)
		if len(encoded) <= MaxResultBytes {
			return out, nil
		}
		end = offset + (end-offset)/2
		for end > offset && end < len(content) && !utf8.RuneStart(content[end]) {
			end--
		}
		if end == offset {
			return nil, toolErr("result_too_large", "skill metadata exceeds result bound")
		}
	}
}

func (r *Registry) skillsInstall(ctx context.Context, c *Call) (any, error) {
	return r.changeSkill(ctx, c, true)
}
func (r *Registry) skillsRemove(ctx context.Context, c *Call) (any, error) {
	return r.changeSkill(ctx, c, false)
}
func (r *Registry) changeSkill(ctx context.Context, c *Call, install bool) (any, error) {
	var ref spec.SkillRef
	if err := decodeContext(c, &ref); err != nil {
		return nil, err
	}
	if !store.ValidSkillRef(ref) {
		return nil, toolErr("invalid", "marketplace, plugin and skill are required safe names")
	}
	snapshot, err := r.d.Store.SkillsSnapshot(ctx, c.Seat.OrganizationID, c.Seat.ID)
	if err != nil {
		return nil, err
	}
	if store.SkillMode(&snapshot.Manifest) != "agent_choice" {
		return nil, toolErr("forbidden", "dynamic skill choices are disabled")
	}
	source, err := store.AuthorizedSkillSource(&snapshot.Manifest, ref)
	if err != nil {
		return nil, err
	}
	if install {
		ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
		defer cancel()
		candidate := slices.Clone(snapshot.Selections)
		if !slices.ContainsFunc(candidate, func(v store.SkillSelection) bool { return v.Ref == ref }) {
			candidate = append(candidate, store.SkillSelection{Ref: ref, Source: source, Owner: "agent"})
		}
		if _, err := r.resolveSkillsBundle(ctx, candidate); err != nil {
			return nil, err
		}
	}
	if err := r.d.Store.ChangeSkillSelection(ctx, c.fence(), c.Seat.OrganizationID, ref, source, install, snapshot.Fingerprint()); err != nil {
		return nil, err
	}
	return map[string]any{"ref": ref, "installed": install, "effective": "next_turn"}, nil
}

// SkillsBundle is the sole byte-delivery path. Agent input cannot override a
// source or select undeclared skills. Recheck authorization after network work.
func (r *Registry) SkillsBundle(ctx context.Context, orgID, seatID string) (runtimeapi.SkillsBundleResponse, error) {
	snapshot, err := r.d.Store.SkillsSnapshot(ctx, orgID, seatID)
	if err != nil {
		return runtimeapi.SkillsBundleResponse{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	out, err := r.resolveSkillsBundle(ctx, snapshot.Selections)
	if err != nil {
		return runtimeapi.SkillsBundleResponse{}, err
	}
	if err := r.checkSkillsSnapshot(ctx, orgID, seatID, snapshot); err != nil {
		return runtimeapi.SkillsBundleResponse{}, err
	}
	return out, nil
}

func (r *Registry) resolveSkillsBundle(ctx context.Context, selectedSkills []store.SkillSelection) (runtimeapi.SkillsBundleResponse, error) {
	out := runtimeapi.SkillsBundleResponse{Skills: []runtimeapi.SkillBundle{}}
	if len(selectedSkills) > runtimeapi.MaxSkillBundles {
		return out, fmt.Errorf("%w: too many selected skills", store.ErrInvalid)
	}
	bytes, files := len(`{"skills":[]}`)+1, 0
	for _, selected := range selectedSkills {
		bundle, err := r.d.SkillCatalog.Resolve(ctx, selected.Ref.Marketplace, selected.Source, selected.Ref)
		if err != nil {
			return runtimeapi.SkillsBundleResponse{}, fmt.Errorf("%w: resolve selected skill %s/%s/%s: %v", store.ErrInvalid, selected.Ref.Marketplace, selected.Ref.Plugin, selected.Ref.Skill, err)
		}
		files += len(bundle.Files)
		if files > runtimeapi.MaxSkillsBundleFiles {
			return runtimeapi.SkillsBundleResponse{}, fmt.Errorf("%w: selected skills exceed %d-file bundle limit", store.ErrInvalid, runtimeapi.MaxSkillsBundleFiles)
		}
		entry := runtimeapi.SkillBundle{Ref: selected.Ref, Source: selected.Source, Digest: bundle.Digest, Files: bundle.Files, Owner: selected.Owner}
		raw, err := json.Marshal(entry)
		if err != nil {
			return runtimeapi.SkillsBundleResponse{}, err
		}
		bytes += len(raw) + 1
		if bytes > runtimeapi.MaxSkillsBundleBytes {
			return runtimeapi.SkillsBundleResponse{}, fmt.Errorf("%w: selected skills exceed %d-byte bundle limit", store.ErrInvalid, runtimeapi.MaxSkillsBundleBytes)
		}
		out.Skills = append(out.Skills, entry)
	}
	return out, nil
}
