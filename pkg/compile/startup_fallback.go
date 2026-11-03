package compile

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/darcys22/steadmesh/harnesses"
	"github.com/darcys22/steadmesh/pkg/spec"
)

// MaxStartupFallbackTimeout bounds the whole startup chain, not each attempt.
const MaxStartupFallbackTimeout = 5 * time.Minute

// StartupFallbackCandidate is a complete, immutable alternative selected by the
// operator. No adapter or agent can add candidates to this list at runtime.
type StartupFallbackCandidate struct {
	Profile string              `json:"profile"`
	Harness spec.HarnessProfile `json:"harness"`
}

// StartupCandidateBinding identifies the complete session configuration of one
// route. The enclosing chain is excluded so an unchanged route survives reorder
// or policy removal, but a model, setting, image or config change does not.
func StartupCandidateBinding(profile string, h spec.HarnessProfile) string {
	h.StartupFallback = nil
	data, _ := json.Marshal(StartupFallbackCandidate{Profile: profile, Harness: h})
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// checkStartupFallbacks runs after ordinary profile/model validation and API
// resolution. Even unused profiles must satisfy the complete fallback contract.
func (c *compiler) checkStartupFallbacks(o *spec.OrganizationSpec) {
	for _, key := range sortedKeys(o.HarnessProfiles) {
		h := o.HarnessProfiles[key]
		p := h.StartupFallback
		if p == nil {
			continue
		}
		path := "harness_profiles." + key + ".startup_fallback"
		if len(p.Profiles) < 1 || len(p.Profiles) >= harnesses.MaxFallbackAttempts {
			c.errf(path+".profiles", "must contain 1 to %d alternative profiles", harnesses.MaxFallbackAttempts-1)
		}
		if p.MaxAttempts < 1 || p.MaxAttempts > harnesses.MaxFallbackAttempts || p.MaxAttempts > 1+len(p.Profiles) {
			c.errf(path+".max_attempts", "must be 1 to %d including the primary and no greater than the approved chain length", harnesses.MaxFallbackAttempts)
		}
		if d, err := time.ParseDuration(p.TotalTimeout); err != nil || d <= 0 || d > MaxStartupFallbackTimeout {
			c.errf(path+".total_timeout", "must be a positive duration no greater than %s", MaxStartupFallbackTimeout)
		}
		seen := map[string]bool{key: true}
		for i, name := range p.Profiles {
			candidatePath := fmt.Sprintf("%s.profiles[%d]", path, i)
			if seen[name] {
				c.errf(candidatePath, "duplicate or self-referencing profile %q", name)
			}
			seen[name] = true
			candidate, ok := o.HarnessProfiles[name]
			if !ok {
				c.errf(candidatePath, "unknown harness profile %q", name)
				continue
			}
			if candidate.StartupFallback != nil {
				c.errf(candidatePath, "nested startup fallback is forbidden for profile %q", name)
			}
			if h.ImageDigest == "" || candidate.ImageDigest != h.ImageDigest {
				c.errf(candidatePath, "profile %q must use the same configured image_digest as the primary; select an explicit multi-harness image", name)
			}
			for _, capability := range h.RequiredCapabilities {
				if !slices.Contains(c.cat.Harnesses[candidate.Adapter].Capabilities, capability) {
					c.errf(candidatePath, "profile %q cannot preserve primary required capability %q", name, capability)
				}
			}
		}
	}
}

func (c *compiler) resolveStartupFallbacks(o *spec.OrganizationSpec, sm *SeatManifest) {
	if sm.Harness.StartupFallback == nil {
		return
	}
	for _, name := range sm.Harness.StartupFallback.Profiles {
		original := o.HarnessProfiles[name]
		h := *original.DeepCopy()
		h.RequiredCapabilities = sortedUnique(append(h.RequiredCapabilities, sm.Harness.RequiredCapabilities...))
		sm.StartupFallbacks = append(sm.StartupFallbacks, StartupFallbackCandidate{Profile: name, Harness: h})
		if h.Model != nil {
			sm.addCapability("connection:"+h.Model.Connection, []string{"model.infer"}, nil, "implicit:startup_fallback:"+name)
		}
	}
}
