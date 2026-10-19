package policy

import (
	"slices"
	"strings"

	"github.com/darcys22/steadmesh/pkg/compile"
	"github.com/darcys22/steadmesh/pkg/spec"
)

// MatchesSubject checks an explicit policy subject, never a role/display label.
func MatchesSubject(sm *compile.SeatManifest, subject string) bool {
	kind, key, ok := strings.Cut(subject, ":")
	if !ok || key == "" {
		return false
	}
	switch kind {
	case "seat":
		return sm.Key == key
	case "team":
		return slices.Contains(sm.Teams, key)
	}
	return false
}

// TeamCoordination is visible only to declared members of an opted-in team.
func TeamCoordination(sm *compile.SeatManifest, org *compile.Manifest, team string) (*spec.TeamCoordination, bool) {
	t, ok := org.Spec.Teams[team]
	if !ok || t.Coordination == nil || !slices.Contains(sm.Teams, team) {
		return nil, false
	}
	return t.Coordination, true
}

func CanEditMission(sm *compile.SeatManifest, org *compile.Manifest, team string) bool {
	p, ok := TeamCoordination(sm, org, team)
	if !ok {
		return false
	}
	return slices.ContainsFunc(p.MissionEditors, func(s string) bool { return MatchesSubject(sm, s) })
}

// WorkTeam identifies the (unique) governance policy of a coordinated work store.
func WorkTeam(org *compile.Manifest, store string) (string, *spec.TeamCoordination) {
	for key, t := range org.Spec.Teams {
		if t.Coordination != nil && store != "" && t.Coordination.WorkStore == store {
			return key, t.Coordination
		}
	}
	return "", nil
}

// MessageLimit is the strictest opted-in team ceiling. Zero preserves legacy behavior.
func MessageLimit(sm *compile.SeatManifest, org *compile.Manifest) int {
	limit := 0
	for _, team := range sm.Teams {
		if p, ok := TeamCoordination(sm, org, team); ok && p.MaxMessagesPerExecution > 0 && (limit == 0 || p.MaxMessagesPerExecution < limit) {
			limit = p.MaxMessagesPerExecution
		}
	}
	return limit
}
