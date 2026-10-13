package api

import (
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/darcys22/steadmesh/pkg/runtimeapi"
	"github.com/darcys22/steadmesh/pkg/taskworker"
	"github.com/darcys22/steadmesh/services/tools"
)

func (s *server) registerTaskRuns(seat func(string, seatHandler)) {
	seat("POST "+runtimeapi.PathTaskRuns, s.createTaskRun)
	seat("GET "+runtimeapi.PathTaskRuns, s.listTaskRuns)
	seat("GET "+runtimeapi.PathTaskRuns+"/{task}", s.getTaskRun)
	seat("POST "+runtimeapi.PathTaskRuns+"/{task}/cancel", s.cancelTaskRun)
	seat("POST "+runtimeapi.PathTaskWorker+"/claim", s.claimTaskRun)
	seat("POST "+runtimeapi.PathTaskWorker+"/finish", s.finishTaskRun)
}

func taskToolAllowed(t *runtimeapi.TaskRun, name string) bool {
	return slices.Contains(t.Request.ToolNames, name)
}

func (s *server) taskTools(q *seatReq) []runtimeapi.ToolDescriptor {
	all := s.Tools.List(q.seat, q.org)
	if q.task == nil {
		return all
	}
	out := []runtimeapi.ToolDescriptor{}
	for _, t := range all {
		if taskToolAllowed(q.task, t.Name) {
			out = append(out, t)
		}
	}
	return out
}

// authorizeTaskWorker is fail-closed for every existing and future seat route.
// A worker token never authenticates as a lease holder or persistent identity.
func (s *server) authorizeTaskWorker(w http.ResponseWriter, r *http.Request, q *seatReq) bool {
	t, err := s.Store.TaskRun(r.Context(), q.seat.ID, q.principal.TaskRunID)
	if err != nil {
		s.storeError(w, r, err)
		return false
	}
	valid, err := s.Store.TaskRunAuthority(r.Context(), q.seat.ID, t.ID)
	if err != nil {
		s.storeError(w, r, err)
		return false
	}
	if !valid {
		writeError(w, http.StatusForbidden, "forbidden", "task worker authority has expired")
		return false
	}
	claim := r.Method == http.MethodPost && r.URL.Path == runtimeapi.PathTaskWorker+"/claim"
	allowed := claim || r.Method == http.MethodPost && r.URL.Path == runtimeapi.PathTaskWorker+"/finish" ||
		r.Method == http.MethodGet && r.URL.Path == runtimeapi.PathTools ||
		r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, runtimeapi.PathToolCall) ||
		strings.HasPrefix(r.URL.Path, runtimeapi.PathModelProxy)
	if !allowed || t.Terminal() || !time.Now().Before(t.Deadline) || t.ConfigRevision != q.seat.ConfigRevision || q.principal.PodUID == "" ||
		(!claim && (t.State != runtimeapi.TaskRunning || t.PodUID != q.principal.PodUID)) {
		writeError(w, http.StatusForbidden, "forbidden", "task worker authority is not valid for this request")
		return false
	}
	q.task = t
	q.gen = t.ParentGeneration
	q.hasGen = true
	// Keep existing execution-event accounting attached to the real parent row.
	q.execution = t.ParentExecutionID
	q.budgetExecution = t.ParentExecutionID
	q.budgetChild = true
	q.seat.Manifest = taskworker.Manifest(q.seat.Manifest, t.Request.ToolNames)
	annotate(r.Context(), "task_run_id", t.ID, "parent_execution_id", t.ParentExecutionID)
	return true
}

func (s *server) createTaskRun(w http.ResponseWriter, r *http.Request, q *seatReq) {
	f, ok := q.fence()
	if !ok {
		fenced(w)
		return
	}
	var req runtimeapi.TaskRunRequest
	if !decodeBody(w, r, &req) {
		return
	}
	t, err := s.Store.CreateTaskRun(r.Context(), f, q.execution, req)
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, t)
}
func (s *server) listTaskRuns(w http.ResponseWriter, r *http.Request, q *seatReq) {
	ts, err := s.Store.ListTaskRuns(r.Context(), q.seat.ID, false)
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	// History is metadata-only; fetch one run for bounded input/result files.
	summaries := make([]map[string]any, 0, len(ts))
	for _, t := range ts {
		summaries = append(summaries, map[string]any{"id": t.ID, "parent_execution_id": t.ParentExecutionID, "state": t.State, "created_at": t.CreatedAt, "deadline": t.Deadline, "finished_at": t.FinishedAt, "cleaned_at": t.CleanedAt})
	}
	writeJSON(w, http.StatusOK, summaries)
}
func (s *server) getTaskRun(w http.ResponseWriter, r *http.Request, q *seatReq) {
	t, err := s.Store.TaskRun(r.Context(), q.seat.ID, r.PathValue("task"))
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}
func (s *server) cancelTaskRun(w http.ResponseWriter, r *http.Request, q *seatReq) {
	f, ok := q.fence()
	if !ok {
		fenced(w)
		return
	}
	if err := s.Store.CancelTaskRun(r.Context(), f, r.PathValue("task")); err != nil {
		s.storeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
func (s *server) claimTaskRun(w http.ResponseWriter, r *http.Request, q *seatReq) {
	if q.task == nil {
		writeError(w, http.StatusForbidden, "forbidden", "worker identity required")
		return
	}
	t, err := s.Store.ClaimTaskRun(r.Context(), q.seat.ID, q.task.ID, q.principal.PodUID)
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	boot := runtimeapi.Bootstrap{Self: tools.Self(q.seat, q.org), Guidance: "You are a disposable task worker for this parent seat. Work only on the explicit task and input files. Your workspace and session are temporary. Return a result; do not claim to have changed the parent's workspace. Only explicitly delegated read-only tools are available."}
	writeJSON(w, http.StatusOK, taskworker.Bootstrap{Run: *t, Manifest: q.seat.Manifest, Identity: boot})
}
func (s *server) finishTaskRun(w http.ResponseWriter, r *http.Request, q *seatReq) {
	if q.task == nil {
		writeError(w, http.StatusForbidden, "forbidden", "worker identity required")
		return
	}
	var req runtimeapi.TaskRunFinish
	if !decodeBody(w, r, &req) {
		return
	}
	for _, f := range req.Result.Artifacts {
		if !slices.Contains(q.task.Request.ArtifactPaths, f.Path) {
			writeError(w, http.StatusBadRequest, "invalid", "artifact was not requested")
			return
		}
	}
	if err := s.Store.FinishTaskRun(r.Context(), q.seat.ID, q.task.ID, q.principal.PodUID, req); err != nil {
		s.storeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
func (s *server) internalTaskRuns(w http.ResponseWriter, r *http.Request) {
	ts, err := s.Store.ListTaskRuns(r.Context(), r.PathValue("id"), true)
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, ts)
}
func (s *server) internalTaskAction(w http.ResponseWriter, r *http.Request) {
	if err := s.Store.ReconcileTaskRun(r.Context(), r.PathValue("id"), r.PathValue("task"), r.PathValue("action")); err != nil {
		s.storeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
