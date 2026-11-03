package harnesses

import (
	"fmt"
	"maps"
	"slices"
	"strings"
)

// ResolveEffort is a pure configuration translation, not a model invocation.
// It never changes settings supplied by the caller. Empty effort preserves
// native defaults/settings. Conflicting common and native values fail closed.
func ResolveEffort(effort string, settings map[string]string, mapping *EffortMapping) (map[string]string, error) {
	resolved := maps.Clone(settings)
	if effort == "" {
		return resolved, nil
	}
	if mapping == nil || mapping.Setting == "" {
		return nil, fmt.Errorf("harness does not support portable effort")
	}
	native, ok := mapping.Values[effort]
	if !ok || native == "" {
		return nil, fmt.Errorf("unsupported effort %q (supported: %s)", effort, strings.Join(slices.Sorted(maps.Keys(mapping.Values)), ", "))
	}
	if supplied, exists := settings[mapping.Setting]; exists && supplied != native {
		return nil, fmt.Errorf("effort %q conflicts with settings.%s=%q", effort, mapping.Setting, supplied)
	}
	if resolved == nil {
		resolved = make(map[string]string)
	}
	resolved[mapping.Setting] = native
	return resolved, nil
}
