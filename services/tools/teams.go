package tools

import (
	"context"
	"slices"
	"strings"

	"github.com/darcys22/steadmesh/pkg/compile"
	"github.com/darcys22/steadmesh/pkg/runtimeapi"
	"github.com/darcys22/steadmesh/services/policy"
)

func nativeTeamAllowed(sm *compile.SeatManifest, org *compile.Manifest) bool {
	for _, key := range sm.Teams {
		if _, ok := policy.TeamCoordination(sm, org, key); ok {
			return true
		}
	}
	return false
}

func (r *Registry) nativeTeamTools() []*tool {
	return []*tool{
		{name: "teams.discover", description: "List your opted-in teams, or discover participants of one team. Only your team's members are visible; can_message reflects explicit routes. A role or membership does not confer additional tools or private memory access.", schema: `{"type":"object","properties":{"team":{"type":"string"},"after":{"type":"string"},"limit":{"type":"integer","minimum":1,"maximum":50}},"additionalProperties":false}`, allowed: nativeTeamAllowed, handle: r.teamDiscover},
		{name: "teams.mission", description: "Read the team's current mission and revision. Runtime refinements remain subordinate to operator policy; shared work uses the named existing store.", schema: `{"type":"object","properties":{"team":{"type":"string"}},"required":["team"],"additionalProperties":false}`, allowed: nativeTeamAllowed, handle: r.teamMission},
		{name: "teams.refine_mission", description: "Refine a team's mission with a reason and expected revision. Only explicitly configured mission editors may do this. It cannot alter membership, grants, routes or operator policy.", schema: `{"type":"object","properties":{"team":{"type":"string"},"expected_revision":{"type":"integer","minimum":0},"expected_policy_revision":{"type":"string"},"mission":{"type":"string","maxLength":8192},"reason":{"type":"string","maxLength":4096}},"required":["team","expected_revision","expected_policy_revision","mission","reason"],"additionalProperties":false}`, allowed: nativeTeamAllowed, mutating: true, handle: r.teamRefineMission},
		{name: "teams.pool", description: "List native runtime pool participants of your team. They have their own identity, inbox, context and memory; minimum membership means availability, not continuous model activity.", schema: `{"type":"object","properties":{"team":{"type":"string"},"cursor":{"type":"string"},"limit":{"type":"integer","minimum":1,"maximum":50}},"required":["team"],"additionalProperties":false}`, allowed: nativeTeamAllowed, handle: r.teamPool},
		{name: "teams.grow", description: "Request one native team participant from an explicitly configured template. Admission is bounded and idempotent (request_id UUID); existing grants and shared budgets remain in force. This adds a peer, not a child with inherited authority.", schema: `{"type":"object","properties":{"team":{"type":"string"},"template":{"type":"string"},"request_id":{"type":"string"}},"required":["team","template","request_id"],"additionalProperties":false}`, allowed: nativeTeamAllowed, mutating: true, handle: r.teamGrow},
		{name: "teams.drain", description: "Drain a native pool participant within the configured membership limits: stop accepting new work, preserve handoff and memory, and release unfinished claims. Retirement never discards shared evidence.", schema: `{"type":"object","properties":{"team":{"type":"string"},"member_id":{"type":"string"}},"required":["team","member_id"],"additionalProperties":false}`, allowed: nativeTeamAllowed, mutating: true, handle: r.teamDrain},
	}
}

func (r *Registry) teamDiscover(ctx context.Context, c *Call) (any, error) {
	var a struct {
		Team  string `json:"team"`
		After string `json:"after"`
		Limit int    `json:"limit"`
	}
	if err := decode(c, &a); err != nil {
		return nil, err
	}
	if a.Team == "" {
		teams := []string{}
		for _, key := range c.Seat.Manifest.Teams {
			if _, ok := policy.TeamCoordination(&c.Seat.Manifest, &c.Org.Manifest, key); ok {
				teams = append(teams, key)
			}
		}
		slices.Sort(teams)
		return map[string]any{"teams": teams}, nil
	}
	if _, ok := policy.TeamCoordination(&c.Seat.Manifest, &c.Org.Manifest, a.Team); !ok {
		return nil, toolErr("not_found", "team not found or not accessible")
	}
	if a.Limit == 0 {
		a.Limit = 20
	}
	if a.Limit < 1 || a.Limit > 50 {
		return nil, toolErr("invalid", "limit must be 1..50")
	}
	peers, err := r.d.Store.TeamPeers(ctx, c.Seat.ID, a.Team, a.After, a.Limit)
	if err != nil {
		return nil, err
	}
	next := ""
	if len(peers) == a.Limit {
		next = peers[len(peers)-1].Seat
	}
	return map[string]any{"team": a.Team, "members": peers, "next_after": next}, nil
}
func (r *Registry) teamMission(ctx context.Context, c *Call) (any, error) {
	var a struct {
		Team string `json:"team"`
	}
	if err := decode(c, &a); err != nil {
		return nil, err
	}
	p, ok := policy.TeamCoordination(&c.Seat.Manifest, &c.Org.Manifest, a.Team)
	if !ok {
		return nil, toolErr("not_found", "team not found or not accessible")
	}
	mission, err := r.d.Store.TeamMission(ctx, c.Seat.ID, a.Team)
	if err != nil {
		return nil, err
	}
	return map[string]any{"mission": mission, "mode": p.Mode, "work_store": p.WorkStore, "review_policy": p.ReviewPolicy, "can_refine": policy.CanEditMission(&c.Seat.Manifest, &c.Org.Manifest, a.Team)}, nil
}
func (r *Registry) teamRefineMission(ctx context.Context, c *Call) (any, error) {
	var a struct {
		Team           string `json:"team"`
		Expected       *int64 `json:"expected_revision"`
		ExpectedPolicy string `json:"expected_policy_revision"`
		Mission        string `json:"mission"`
		Reason         string `json:"reason"`
	}
	if err := decode(c, &a); err != nil {
		return nil, err
	}
	if a.Expected == nil {
		return nil, toolErr("invalid", "expected_revision is required")
	}
	if !policy.CanEditMission(&c.Seat.Manifest, &c.Org.Manifest, a.Team) {
		return nil, toolErr("forbidden", "this seat cannot refine this team's mission")
	}
	return r.d.Store.UpdateTeamMission(ctx, c.fence(), a.Team, *a.Expected, a.ExpectedPolicy, a.Mission, a.Reason)
}
func (r *Registry) teamPool(ctx context.Context, c *Call) (any, error) {
	var a struct {
		Team   string `json:"team"`
		Cursor string `json:"cursor"`
		Limit  int    `json:"limit"`
	}
	if err := decode(c, &a); err != nil {
		return nil, err
	}
	members, err := r.d.Store.ListPoolMembers(ctx, c.Seat.ID, a.Team)
	if err != nil {
		return nil, err
	}
	return poolPage(a.Team, members, a.Cursor, a.Limit)
}
func poolPage(team string, members []runtimeapi.PoolMember, cursor string, requested int) (any, error) {
	offset, limit, err := page(cursor, requested, 20, 50)
	if err != nil {
		return nil, err
	}
	if offset > len(members) {
		return nil, toolErr("invalid", "cursor is beyond available participants")
	}
	end := min(offset+limit, len(members))
	return fit(members[offset:end], offset, end < len(members), func(v []runtimeapi.PoolMember, next string) any {
		return map[string]any{"team": team, "members": nonNilSlice(v), "next_cursor": next}
	}), nil
}

func (r *Registry) teamGrow(ctx context.Context, c *Call) (any, error) {
	var a struct {
		Team      string `json:"team"`
		Template  string `json:"template"`
		RequestID string `json:"request_id"`
	}
	if err := decode(c, &a); err != nil {
		return nil, err
	}
	if strings.TrimSpace(a.Template) == "" || a.RequestID == "" {
		return nil, toolErr("invalid", "template and request_id are required")
	}
	return r.d.Store.SpawnPoolMember(ctx, c.fence(), a.Team, a.Template, a.RequestID)
}
func (r *Registry) teamDrain(ctx context.Context, c *Call) (any, error) {
	var a struct {
		Team     string `json:"team"`
		MemberID string `json:"member_id"`
	}
	if err := decode(c, &a); err != nil {
		return nil, err
	}
	if a.MemberID == "" {
		return nil, toolErr("invalid", "member_id is required")
	}
	if err := r.d.Store.DrainPoolMember(ctx, c.fence(), a.Team, a.MemberID); err != nil {
		return nil, err
	}
	return map[string]any{"member_id": a.MemberID, "status": "draining"}, nil
}
