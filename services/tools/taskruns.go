package tools

import (
	"context"
	"github.com/darcys22/steadmesh/pkg/runtimeapi"
)

func (r *Registry) taskTools() []*tool {
	return []*tool{
		{name: "tasks.start", description: "Start a bounded disposable worker beneath this seat. Fresh workspace/session, inherited model/runtime, default no tools. Provide explicit UTF-8 input files and requested artifact paths; no persistent workspace is mounted. request_id is a UUID idempotency key. At most 4 active children and 16 children per parent execution.", mutating: true, allowed: legacyWorkerAllowed,
			schema: `{"type":"object","properties":{"request_id":{"type":"string"},"prompt":{"type":"string","maxLength":32768},"timeout_seconds":{"type":"integer","minimum":1,"maximum":1200},"tool_names":{"type":"array","items":{"type":"string","enum":["memory.stores","memory.list","memory.read","memory.search","memory.history","work.get","work.list"]}},"input_files":{"type":"array","maxItems":64,"items":{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"}},"required":["path","content"],"additionalProperties":false}},"artifact_paths":{"type":"array","maxItems":64,"items":{"type":"string"}}},"required":["request_id","prompt"],"additionalProperties":false}`,
			handle: func(ctx context.Context, c *Call) (any, error) {
				var req runtimeapi.TaskRunRequest
				if err := decode(c, &req); err != nil {
					return nil, err
				}
				t, err := r.d.Store.CreateTaskRun(ctx, c.fence(), c.ExecutionID, req)
				if err != nil {
					return nil, err
				}
				return taskSummary(*t), nil
			}},
		{name: "tasks.get", description: "Read one disposable worker's status and output. Set artifact_path to fetch one returned file (large files require the runtime API). Results never apply themselves to this seat's workspace.", allowed: legacyWorkerAllowed,
			schema: `{"type":"object","properties":{"id":{"type":"string"},"artifact_path":{"type":"string"}},"required":["id"],"additionalProperties":false}`,
			handle: func(ctx context.Context, c *Call) (any, error) {
				var a struct {
					ID   string `json:"id"`
					Path string `json:"artifact_path"`
				}
				if err := decode(c, &a); err != nil {
					return nil, err
				}
				t, err := r.d.Store.TaskRun(ctx, c.Seat.ID, a.ID)
				if err != nil {
					return nil, err
				}
				if a.Path != "" {
					if t.Result != nil {
						for _, f := range t.Result.Artifacts {
							if f.Path == a.Path {
								return f, nil
							}
						}
					}
					return nil, toolErr("not_found", "artifact not found")
				}
				return taskSummary(*t), nil
			}},
		{name: "tasks.cancel", description: "Cancel one worker owned by this seat; revoke its access and remove its disposable workspace. Terminal results are retained.", mutating: true, allowed: legacyWorkerAllowed,
			schema: `{"type":"object","properties":{"id":{"type":"string"}},"required":["id"],"additionalProperties":false}`,
			handle: func(ctx context.Context, c *Call) (any, error) {
				var a struct {
					ID string `json:"id"`
				}
				if err := decode(c, &a); err != nil {
					return nil, err
				}
				return map[string]string{"id": a.ID, "status": "cancel_requested"}, r.d.Store.CancelTaskRun(ctx, c.fence(), a.ID)
			}},
	}
}

func taskSummary(t runtimeapi.TaskRun) any {
	out := map[string]any{"id": t.ID, "parent_execution_id": t.ParentExecutionID, "state": t.State, "deadline": t.Deadline, "cleaned_at": t.CleanedAt}
	if t.Result != nil {
		out["output"] = clipTaskOutput(t.Result.Output, 8192)
		out["error"] = t.Result.Error
		files := []map[string]any{}
		for _, f := range t.Result.Artifacts {
			files = append(files, map[string]any{"path": f.Path, "bytes": len(f.Content)})
		}
		out["artifacts"] = files
	}
	return out
}
func clipTaskOutput(s string, n int) string {
	if len(s) > n {
		return s[:n] + " [truncated; use runtime API]"
	}
	return s
}
