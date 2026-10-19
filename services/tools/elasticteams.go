package tools

import (
	"context"
	"slices"

	"github.com/darcys22/steadmesh/pkg/compile"
	"github.com/darcys22/steadmesh/pkg/runtimeapi"
)

func elasticAllowed(sm *compile.SeatManifest, org *compile.Manifest) bool {
	p := org.Spec.ElasticTeams
	return sm.PoolTeam == "" && p != nil && slices.Contains(p.EnabledSeats, sm.Key)
}

func (r *Registry) elasticTeamTools() []*tool {
	return []*tool{
		{name: "teams.templates", description: "List approved template names and pool limits (64 per page; use after for the next page). Set template to inspect its tool ceiling and instruction preview. Templates inherit this seat's model/runtime and cannot launch children.", allowed: elasticAllowed, schema: `{"type":"object","properties":{"template":{"type":"string"},"after":{"type":"string"}},"additionalProperties":false}`, handle: func(ctx context.Context, c *Call) (any, error) {
			var a struct {
				Template string `json:"template"`
				After    string `json:"after"`
			}
			if err := decode(c, &a); err != nil {
				return nil, err
			}
			p := c.Org.Manifest.Spec.ElasticTeams
			if a.Template != "" {
				t, ok := p.Templates[a.Template]
				if !ok {
					return nil, toolErr("not_found", "template not found")
				}
				return map[string]any{"template": a.Template, "instructions_preview": clipTaskOutput(t.Instructions, 1024), "tool_names": t.ToolNames, "harness_profile": c.Seat.Manifest.HarnessProfile, "execution_profile": c.Seat.Manifest.ExecutionProfile}, nil
			}
			names := []string{}
			for k := range p.Templates {
				if k > a.After {
					names = append(names, k)
				}
			}
			slices.Sort(names)
			next := ""
			if len(names) > 64 {
				names = names[:64]
				next = names[63]
			}
			return map[string]any{"templates": names, "next_after": next, "draining": p.Draining, "max_active_workers": p.MaxActiveWorkers, "max_active_per_execution": p.MaxActivePerExecution, "max_launches_per_execution": p.MaxLaunchesPerExecution, "max_timeout_seconds": p.MaxTimeoutSeconds}, nil
		}},
		{name: "teams.expand", description: "Atomically request a required team of temporary workers from approved templates. All members are admitted or the request is refused; no partial teams or waiting queue. request_id is a UUID idempotency key. Use teams.templates first. Each member needs a stable name (letters/digits/hyphens/underscores), template and task (prompt, optional timeout_seconds, tool_names, input_files, artifact_paths). Task tools narrow the template ceiling; omitted means no tools. Workers share this execution's cumulative output-token budget and lifetime launch count, including failed attempts. This does not cap dollars, input tokens or cloud costs.", mutating: true, allowed: elasticAllowed,
			schema: `{"type":"object","properties":{"request_id":{"type":"string"},"workers":{"type":"array","minItems":1,"maxItems":32,"items":{"type":"object","properties":{"name":{"type":"string","maxLength":80},"template":{"type":"string","maxLength":80},"task":{"type":"object","properties":{"prompt":{"type":"string","maxLength":32768},"timeout_seconds":{"type":"integer","minimum":1,"maximum":1200},"tool_names":{"type":"array","items":{"type":"string"}},"input_files":{"type":"array","maxItems":64,"items":{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"}},"required":["path","content"],"additionalProperties":false}},"artifact_paths":{"type":"array","maxItems":64,"items":{"type":"string"}}},"required":["prompt"],"additionalProperties":false}},"required":["name","template","task"],"additionalProperties":false}}},"required":["request_id","workers"],"additionalProperties":false}`,
			handle: func(ctx context.Context, c *Call) (any, error) {
				var req runtimeapi.ElasticTeamRequest
				if err := decode(c, &req); err != nil {
					return nil, err
				}
				team, err := r.d.Store.CreateElasticTeam(ctx, c.fence(), c.ExecutionID, req)
				if err != nil {
					return nil, err
				}
				return elasticSummary(team), nil
			}},
		{name: "teams.get", description: "Read a durable team's worker IDs and states. Use tasks.get for individual results and artifacts. Results never apply themselves to the permanent seat.", allowed: elasticAllowed, schema: `{"type":"object","properties":{"id":{"type":"string"}},"required":["id"],"additionalProperties":false}`, handle: func(ctx context.Context, c *Call) (any, error) {
			var a struct {
				ID string `json:"id"`
			}
			if err := decode(c, &a); err != nil {
				return nil, err
			}
			team, err := r.d.Store.GetElasticTeam(ctx, c.Seat.ID, a.ID)
			if err != nil {
				return nil, err
			}
			return elasticSummary(team), nil
		}},
		{name: "teams.cancel", description: "Cancel every unfinished member of this seat's team. Access is revoked immediately; pool capacity is retained until disposable runtime cleanup is confirmed. Durable results are retained.", mutating: true, allowed: elasticAllowed, schema: `{"type":"object","properties":{"id":{"type":"string"}},"required":["id"],"additionalProperties":false}`, handle: func(ctx context.Context, c *Call) (any, error) {
			var a struct {
				ID string `json:"id"`
			}
			if err := decode(c, &a); err != nil {
				return nil, err
			}
			return map[string]string{"id": a.ID, "status": "cancel_requested"}, r.d.Store.CancelElasticTeam(ctx, c.fence(), a.ID)
		}},
	}
}

func elasticSummary(t *runtimeapi.ElasticTeam) any {
	workers := make([]map[string]any, 0, len(t.Workers))
	for _, w := range t.Workers {
		workers = append(workers, map[string]any{"name": w.Name, "template": w.Template, "id": w.Run.ID, "state": w.Run.State, "deadline": w.Run.Deadline, "cleaned_at": w.Run.CleanedAt})
	}
	return map[string]any{"id": t.ID, "request_id": t.RequestID, "parent_execution_id": t.ParentExecutionID, "workers": workers}
}
