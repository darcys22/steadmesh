package tools

import (
	"context"

	"github.com/darcys22/steadmesh/pkg/runtimeapi"
)

func (r *Registry) branchTools() []*tool {
	return []*tool{
		{name: "branches.start", description: "Start 2–4 independent alternatives from an explicit shared handoff and optional text files. Creates all alternatives atomically under the current execution. Reuse request_id on a retry. No parent session, workspace, secrets or tool grants are copied. Each result stays separate until explicitly selected; selection never applies files.",
			schema:   `{"type":"object","properties":{"request_id":{"type":"string","format":"uuid"},"handoff":{"type":"string","minLength":1,"maxLength":8192},"input_files":{"type":"array","maxItems":64,"items":{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"}},"required":["path","content"],"additionalProperties":false}},"artifact_paths":{"type":"array","maxItems":64,"items":{"type":"string"}},"timeout_seconds":{"type":"integer","minimum":1,"maximum":1200},"alternatives":{"type":"array","minItems":2,"maxItems":4,"items":{"type":"object","properties":{"name":{"type":"string","minLength":1,"maxLength":64},"prompt":{"type":"string","minLength":1,"maxLength":8192}},"required":["name","prompt"],"additionalProperties":false}}},"required":["request_id","handoff","alternatives"],"additionalProperties":false}`,
			mutating: true, allowed: always, handle: r.branchStart},
		{name: "branches.compare", description: "Compare your branch group's independent alternatives: status, bounded output/error previews, artifact counts and complete-result hashes. Retrieve full output/artifacts with the task-run API before choosing. all_terminal and selection are independent; choosing does not cancel siblings.",
			schema:  `{"type":"object","properties":{"id":{"type":"string","format":"uuid"}},"required":["id"],"additionalProperties":false}`,
			allowed: always, handle: r.branchCompare},
		{name: "branches.select", description: "Explicitly record a completed alternative as the chosen result. Supply the group revision and the exact result_sha256 returned by compare after reviewing the result. Records an auditable choice only: never merges, overwrites the working tree, cancels other runs or grants permission for external actions.",
			schema:   `{"type":"object","properties":{"id":{"type":"string","format":"uuid"},"task_run_id":{"type":"string","format":"uuid"},"expected_revision":{"type":"integer","minimum":1},"expected_result_sha256":{"type":"string","pattern":"^[a-f0-9]{64}$"}},"required":["id","task_run_id","expected_revision","expected_result_sha256"],"additionalProperties":false}`,
			mutating: true, allowed: always, handle: r.branchSelect},
	}
}

func (r *Registry) branchStart(ctx context.Context, c *Call) (any, error) {
	var req runtimeapi.RunBranchRequest
	if err := decode(c, &req); err != nil {
		return nil, err
	}
	return r.d.Store.CreateRunBranch(ctx, c.fence(), c.ExecutionID, req)
}

func (r *Registry) branchCompare(ctx context.Context, c *Call) (any, error) {
	var req struct {
		ID string `json:"id"`
	}
	if err := decode(c, &req); err != nil {
		return nil, err
	}
	return r.d.Store.CompareRunBranch(ctx, c.Seat.ID, req.ID)
}

func (r *Registry) branchSelect(ctx context.Context, c *Call) (any, error) {
	var req runtimeapi.RunBranchSelectRequest
	if err := decode(c, &req); err != nil {
		return nil, err
	}
	return r.d.Store.SelectRunBranch(ctx, c.fence(), req)
}
