package api

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/darcys22/steadmesh/pkg/runtimeapi"
	"github.com/darcys22/steadmesh/services/store"
)

// The console API (/console/v1) is the read-only operator view behind the
// Steadmesh Console. It is registered only when Config.Console is set, and
// accepts only the configured console identity.

func (s *server) registerConsole(mux *http.ServeMux) {
	console := func(pattern string, h http.HandlerFunc) { mux.Handle("GET "+pattern, s.consoleAuth(h)) }
	orgs := runtimeapi.PathConsoleOrgs
	console(orgs, s.consoleOrgs)
	console(orgs+"/{id}", s.consoleOrg)
	console(orgs+"/{id}/seats", s.consoleSeats)
	console(orgs+"/{id}/conversations", s.consoleConversations)
	console(orgs+"/{id}/operations", s.consoleOperations)
	console(orgs+"/{id}/artifacts", s.consoleArtifacts)
	console(orgs+"/{id}/work", s.consoleWork)
	console(orgs+"/{id}/executions", s.consoleOrgExecutions)
	console(orgs+"/{id}/activity", s.consoleActivity)
	console(runtimeapi.PathConsoleSeats+"{id}", s.consoleSeat)
	console(runtimeapi.PathConsoleSeats+"{id}/executions", s.consoleSeatExecutions)
	console(runtimeapi.PathConsoleExecutions+"{id}", s.consoleExecution)
	console(runtimeapi.PathConsoleConversations+"{id}", s.consoleConversation)
}

func (s *server) consoleAuth(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := s.Auth.Console(r.Context(), bearer(r)); err != nil {
			authError(w, err)
			return
		}
		next(w, r)
	})
}

func (s *server) consoleOrgs(w http.ResponseWriter, r *http.Request) {
	orgs, err := s.Store.ConsoleOrganizations(r.Context())
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, orgs)
}

func (s *server) consoleOrg(w http.ResponseWriter, r *http.Request) {
	org, err := s.Store.ConsoleOrganization(r.Context(), r.PathValue("id"))
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, org)
}

func (s *server) consoleSeats(w http.ResponseWriter, r *http.Request) {
	seats, err := s.Store.ConsoleSeatStatuses(r.Context(), r.PathValue("id"), nil)
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, runtimeapi.ConsoleSeatList{Seats: seats})
}

func (s *server) consoleSeat(w http.ResponseWriter, r *http.Request) {
	seat, err := s.Store.ConsoleSeat(r.Context(), r.PathValue("id"), limitParam(r, 50, 200))
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, seat)
}

func (s *server) consoleSeatExecutions(w http.ResponseWriter, r *http.Request) {
	before, ok := timeParam(w, r, "before")
	if !ok {
		return
	}
	runs, err := s.Store.ConsoleExecutions(r.Context(), r.PathValue("id"), before, limitParam(r, 50, 200))
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, runtimeapi.ConsoleExecutionList{Executions: runs})
}

func (s *server) consoleExecution(w http.ResponseWriter, r *http.Request) {
	run, err := s.Store.ConsoleExecution(r.Context(), r.PathValue("id"))
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	for i := range run.Events {
		run.Events[i].Data = redact(run.Events[i].Data)
	}
	redactOperations(run.Operations)
	writeJSON(w, http.StatusOK, run)
}

func (s *server) consoleConversations(w http.ResponseWriter, r *http.Request) {
	system := r.URL.Query().Get("system") == "true"
	convs, err := s.Store.ConsoleConversations(r.Context(), r.PathValue("id"), system, limitParam(r, 50, 500))
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, runtimeapi.ConsoleConversationList{Conversations: convs})
}

func (s *server) consoleConversation(w http.ResponseWriter, r *http.Request) {
	conv, err := s.Store.ConsoleConversation(r.Context(), r.PathValue("id"))
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, conv)
}

func (s *server) consoleOperations(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	ops, err := s.Store.ConsoleOperations(r.Context(), r.PathValue("id"),
		store.ConsoleOperationFilter{SeatKey: q.Get("seat"), Status: q.Get("status"), Limit: limitParam(r, 100, 500)})
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	redactOperations(ops)
	writeJSON(w, http.StatusOK, runtimeapi.ConsoleOperationList{Operations: ops})
}

func (s *server) consoleArtifacts(w http.ResponseWriter, r *http.Request) {
	arts, err := s.Store.ConsoleArtifacts(r.Context(), r.PathValue("id"), limitParam(r, 100, 500))
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, runtimeapi.ConsoleArtifactList{Artifacts: arts})
}

func (s *server) consoleActivity(w http.ResponseWriter, r *http.Request) {
	after, ok := timeParam(w, r, "after")
	if !ok {
		return
	}
	items, err := s.Store.ConsoleActivity(r.Context(), r.PathValue("id"), after, limitParam(r, 200, 1000))
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	if items == nil {
		items = []runtimeapi.ConsoleActivityItem{}
	}
	writeJSON(w, http.StatusOK, runtimeapi.ConsoleActivity{Items: items})
}

func limitParam(r *http.Request, def, max int) int {
	n, err := strconv.Atoi(r.URL.Query().Get("limit"))
	if err != nil || n <= 0 {
		return def
	}
	return min(n, max)
}

func timeParam(w http.ResponseWriter, r *http.Request, name string) (time.Time, bool) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return time.Time{}, true
	}
	t, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid", name+" must be an RFC 3339 time")
		return time.Time{}, false
	}
	return t, true
}

func redactOperations(ops []runtimeapi.ConsoleOperation) {
	for i := range ops {
		ops[i].Result = redact(ops[i].Result)
	}
}

// sensitiveKey matches JSON keys whose values are withheld from the console:
// agent tool inputs and connector results are shown, credentials are not.
var sensitiveKey = regexp.MustCompile(`(?i)(token|secret|password|passwd|api[_-]?key|authorization|credential|private[_-]?key|cookie)`)

const redacted = "[redacted]"

// redact replaces the values of credential-like keys anywhere in a JSON
// document. Invalid JSON is returned unchanged; it was stored as received.
func redact(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return raw
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return raw
	}
	changed := false
	var walk func(any) any
	walk = func(v any) any {
		switch t := v.(type) {
		case map[string]any:
			for k, val := range t {
				if sensitiveKey.MatchString(k) {
					if val != nil && val != redacted {
						t[k], changed = redacted, true
					}
					continue
				}
				t[k] = walk(val)
			}
		case []any:
			for i := range t {
				t[i] = walk(t[i])
			}
		}
		return v
	}
	v = walk(v)
	if !changed {
		return raw
	}
	out, err := json.Marshal(v)
	if err != nil {
		return raw
	}
	return out
}

func (s *server) consoleOrgExecutions(w http.ResponseWriter, r *http.Request) {
	runs, err := s.Store.ConsoleOrganizationExecutions(r.Context(), r.PathValue("id"), r.URL.Query().Get("state"), limitParam(r, 50, 500))
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, runtimeapi.ConsoleExecutionList{Executions: runs})
}

func (s *server) consoleWork(w http.ResponseWriter, r *http.Request) {
	items, err := s.Store.ConsoleWork(r.Context(), r.PathValue("id"), limitParam(r, 100, 500))
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	if items == nil {
		items = []runtimeapi.ConsoleWorkItem{}
	}
	writeJSON(w, http.StatusOK, runtimeapi.ConsoleWorkList{Work: items})
}
