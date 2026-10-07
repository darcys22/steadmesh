// Package fakelinear is a deterministic, in-memory fake of the Linear GraphQL
// API subset used by connectors/linear. Requests are matched loosely by the
// root field name of the query or mutation; variables carry all inputs.
//
// Supported root fields: viewer, project, projects, projectCreate, issue,
// issues, searchIssues, issueCreate, issueUpdate, commentCreate, comments.
//
// Test control:
//
//	GET  /_test/state  projects, issues and comments
//	POST /_test/fail   {operation, mode, count}; mode is
//	                   "drop_response_after_commit" (apply, then close the connection),
//	                   "error" (GraphQL error, not applied),
//	                   "rate_limited" (HTTP 429, not applied) or
//	                   "server_error" (HTTP 500, not applied)
//	POST /_test/reset  clear records and failures
package fakelinear

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
)

// Options configure the fake.
type Options struct {
	// APIKey, when set, must equal the Authorization header (with or without
	// "Bearer "). Otherwise any non-empty value is accepted.
	APIKey string
	// OrganizationID is the viewer's organisation id. Default org-fake.
	OrganizationID string
	// Teams maps team id to key. Default: team-eng ENG.
	Teams map[string]string
}

// Team is a Linear team.
type Team struct {
	ID  string `json:"id"`
	Key string `json:"key"`
}

// Project is a stored project.
type Project struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Content     string   `json:"content"`
	URL         string   `json:"url"`
	State       string   `json:"state"`
	TeamIDs     []string `json:"team_ids"`
}

// Issue is a stored issue.
type Issue struct {
	ID          string `json:"id"`
	Identifier  string `json:"identifier"`
	Title       string `json:"title"`
	Description string `json:"description"`
	URL         string `json:"url"`
	Priority    int    `json:"priority"`
	TeamID      string `json:"team_id"`
	ProjectID   string `json:"project_id,omitempty"`
	StateID     string `json:"state_id,omitempty"`
	AssigneeID  string `json:"assignee_id,omitempty"`
}

// Comment is a stored comment.
type Comment struct {
	ID      string `json:"id"`
	IssueID string `json:"issue_id"`
	Body    string `json:"body"`
	URL     string `json:"url"`
}

// State is the full fake state.
type State struct {
	Projects []Project `json:"projects"`
	Issues   []Issue   `json:"issues"`
	Comments []Comment `json:"comments"`
}

type failRule struct {
	mode      string
	remaining int
}

// Server is the fake. It implements http.Handler.
type Server struct {
	opts Options
	mux  *http.ServeMux

	mu       sync.Mutex
	seq      int
	teamSeq  map[string]int
	projects []*Project
	issues   []*Issue
	comments []*Comment
	fails    map[string][]*failRule
	// validKeys, when non-nil, replaces Options.APIKey: only listed keys are
	// accepted (set with POST /_test/keys).
	validKeys map[string]bool
}

// New creates the fake.
func New(opts Options) *Server {
	if opts.OrganizationID == "" {
		opts.OrganizationID = "org-fake"
	}
	if opts.Teams == nil {
		opts.Teams = map[string]string{"team-eng": "ENG"}
	}
	s := &Server{opts: opts, mux: http.NewServeMux(), teamSeq: map[string]int{}, fails: map[string][]*failRule{}}
	s.mux.HandleFunc("POST /graphql", s.handleGraphQL)
	s.mux.HandleFunc("POST /{$}", s.handleGraphQL)
	s.mux.HandleFunc("GET /_test/state", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, s.State()) })
	s.mux.HandleFunc("POST /_test/fail", s.handleTestFail)
	s.mux.HandleFunc("POST /_test/keys", s.handleTestKeys)
	s.mux.HandleFunc("POST /_test/reset", func(w http.ResponseWriter, _ *http.Request) {
		s.Reset()
		writeJSON(w, 200, map[string]bool{"ok": true})
	})
	return s
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

// State returns a copy of all records.
func (s *Server) State() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := State{Projects: []Project{}, Issues: []Issue{}, Comments: []Comment{}}
	for _, p := range s.projects {
		st.Projects = append(st.Projects, *p)
	}
	for _, i := range s.issues {
		st.Issues = append(st.Issues, *i)
	}
	for _, c := range s.comments {
		st.Comments = append(st.Comments, *c)
	}
	return st
}

// Fail makes the next count requests for a root field fail with mode.
func (s *Server) Fail(operation, mode string, count int) {
	if count <= 0 {
		count = 1
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fails[operation] = append(s.fails[operation], &failRule{mode: mode, remaining: count})
}

// Reset clears records and failure rules.
func (s *Server) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq = 0
	s.teamSeq = map[string]int{}
	s.projects, s.issues, s.comments = nil, nil, nil
	s.fails = map[string][]*failRule{}
}

func (s *Server) takeFail(op string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	rules := s.fails[op]
	for len(rules) > 0 && rules[0].remaining <= 0 {
		rules = rules[1:]
	}
	s.fails[op] = rules
	if len(rules) == 0 {
		return ""
	}
	rules[0].remaining--
	return rules[0].mode
}

var rootField = regexp.MustCompile(`^\s*([A-Za-z_][A-Za-z0-9_]*)`)

// RootField returns the first selected field of a GraphQL document.
func RootField(query string) string {
	i := strings.Index(query, "{")
	if i < 0 {
		return ""
	}
	m := rootField.FindStringSubmatch(query[i+1:])
	if m == nil {
		return ""
	}
	return m[1]
}

type gqlError struct {
	Message    string            `json:"message"`
	Extensions map[string]string `json:"extensions,omitempty"`
}

type gqlFailure struct{ gqlError }

func fail(code, format string, args ...any) *gqlFailure {
	return &gqlFailure{gqlError{Message: fmt.Sprintf(format, args...), Extensions: map[string]string{"code": code}}}
}

func (s *Server) handleGraphQL(w http.ResponseWriter, r *http.Request) {
	auth := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !s.keyOK(auth) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"errors": []gqlError{{
			Message: "Authentication required", Extensions: map[string]string{"code": "AUTHENTICATION_ERROR"},
		}}})
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var req struct {
		Query     string                     `json:"query"`
		Variables map[string]json.RawMessage `json:"variables"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"errors": []gqlError{{Message: "invalid JSON body"}}})
		return
	}
	field := RootField(req.Query)

	mode := s.takeFail(field)
	switch mode {
	case "rate_limited":
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"errors": []gqlError{{
			Message: "Rate limit exceeded", Extensions: map[string]string{"code": "RATELIMITED"},
		}}})
		return
	case "server_error":
		w.WriteHeader(http.StatusInternalServerError)
		return
	case "error":
		writeJSON(w, http.StatusOK, map[string]any{"errors": []gqlError{{
			Message: "injected failure", Extensions: map[string]string{"code": "INVALID_INPUT"},
		}}})
		return
	}

	data, ferr := s.execute(field, req.Variables)
	if mode == "drop_response_after_commit" {
		hijackClose(w)
		return
	}
	if ferr != nil {
		writeJSON(w, http.StatusOK, map[string]any{"data": nil, "errors": []gqlError{ferr.gqlError}})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{field: data}})
}

func (s *Server) execute(field string, vars map[string]json.RawMessage) (any, *gqlFailure) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch field {
	case "viewer":
		return map[string]any{
			"id": "user-viewer", "name": "steadmesh", "email": "steadmesh@example.invalid",
			"organization": map[string]any{"id": s.opts.OrganizationID, "name": "Fake", "urlKey": "fake"},
		}, nil
	case "project":
		var id string
		_ = json.Unmarshal(vars["id"], &id)
		for _, p := range s.projects {
			if p.ID == id {
				return s.projectJSON(p), nil
			}
		}
		return nil, fail("ENTITY_NOT_FOUND", "Entity not found: Project")
	case "projects":
		var filter struct {
			Name map[string]string `json:"name"`
		}
		_ = json.Unmarshal(vars["filter"], &filter)
		nodes := []any{}
		for i := len(s.projects) - 1; i >= 0 && len(nodes) < first(vars); i-- {
			p := s.projects[i]
			if matchString(p.Name, filter.Name) {
				nodes = append(nodes, s.projectJSON(p))
			}
		}
		return map[string]any{"nodes": nodes}, nil
	case "projectCreate":
		var in struct {
			Name        string   `json:"name"`
			Description string   `json:"description"`
			Content     string   `json:"content"`
			TeamIDs     []string `json:"teamIds"`
		}
		if err := json.Unmarshal(vars["input"], &in); err != nil {
			return nil, fail("INVALID_INPUT", "invalid input: %v", err)
		}
		if in.Name == "" || len(in.TeamIDs) == 0 {
			return nil, fail("INVALID_INPUT", "name and teamIds are required")
		}
		if len(in.Description) > 255 {
			return nil, fail("INVALID_INPUT", "description must be at most 255 characters")
		}
		for _, t := range in.TeamIDs {
			if _, ok := s.opts.Teams[t]; !ok {
				return nil, fail("ENTITY_NOT_FOUND", "Entity not found: Team %s", t)
			}
		}
		s.seq++
		p := &Project{
			ID: fmt.Sprintf("proj-%04d", s.seq), Name: in.Name, Description: in.Description, Content: in.Content,
			State: "planned", TeamIDs: in.TeamIDs,
		}
		p.URL = "https://linear.app/fake/project/" + p.ID
		s.projects = append(s.projects, p)
		return map[string]any{"success": true, "project": s.projectJSON(p)}, nil
	case "issue":
		var id string
		_ = json.Unmarshal(vars["id"], &id)
		if i := s.findIssue(id); i != nil {
			return s.issueJSON(i), nil
		}
		return nil, fail("ENTITY_NOT_FOUND", "Entity not found: Issue")
	case "issues":
		var filter struct {
			Description map[string]string `json:"description"`
			Project     struct {
				ID map[string]string `json:"id"`
			} `json:"project"`
		}
		_ = json.Unmarshal(vars["filter"], &filter)
		nodes := []any{}
		for i := len(s.issues) - 1; i >= 0 && len(nodes) < first(vars); i-- {
			is := s.issues[i]
			if matchString(is.Description, filter.Description) && matchString(is.ProjectID, filter.Project.ID) {
				nodes = append(nodes, s.issueJSON(is))
			}
		}
		return map[string]any{"nodes": nodes}, nil
	case "searchIssues":
		var term string
		_ = json.Unmarshal(vars["term"], &term)
		term = strings.ToLower(term)
		nodes := []any{}
		for i := len(s.issues) - 1; i >= 0 && len(nodes) < first(vars); i-- {
			is := s.issues[i]
			if strings.Contains(strings.ToLower(is.Title+" "+is.Description), term) {
				nodes = append(nodes, s.issueJSON(is))
			}
		}
		return map[string]any{"nodes": nodes}, nil
	case "issueCreate", "issueUpdate":
		var in struct {
			TeamID      *string `json:"teamId"`
			Title       *string `json:"title"`
			Description *string `json:"description"`
			ProjectID   *string `json:"projectId"`
			StateID     *string `json:"stateId"`
			AssigneeID  *string `json:"assigneeId"`
			Priority    *int    `json:"priority"`
		}
		if err := json.Unmarshal(vars["input"], &in); err != nil {
			return nil, fail("INVALID_INPUT", "invalid input: %v", err)
		}
		var is *Issue
		if field == "issueCreate" {
			if in.TeamID == nil || in.Title == nil || *in.Title == "" {
				return nil, fail("INVALID_INPUT", "teamId and title are required")
			}
			key, ok := s.opts.Teams[*in.TeamID]
			if !ok {
				return nil, fail("ENTITY_NOT_FOUND", "Entity not found: Team %s", *in.TeamID)
			}
			s.seq++
			s.teamSeq[*in.TeamID]++
			is = &Issue{
				ID: fmt.Sprintf("issue-%04d", s.seq), Identifier: fmt.Sprintf("%s-%d", key, s.teamSeq[*in.TeamID]),
				TeamID: *in.TeamID,
			}
			is.URL = "https://linear.app/fake/issue/" + is.Identifier
		} else {
			var id string
			_ = json.Unmarshal(vars["id"], &id)
			if is = s.findIssue(id); is == nil {
				return nil, fail("ENTITY_NOT_FOUND", "Entity not found: Issue")
			}
		}
		if in.ProjectID != nil && *in.ProjectID != "" && s.findProject(*in.ProjectID) == nil {
			return nil, fail("ENTITY_NOT_FOUND", "Entity not found: Project %s", *in.ProjectID)
		}
		set(&is.Title, in.Title)
		set(&is.Description, in.Description)
		set(&is.ProjectID, in.ProjectID)
		set(&is.StateID, in.StateID)
		set(&is.AssigneeID, in.AssigneeID)
		if in.Priority != nil {
			is.Priority = *in.Priority
		}
		if field == "issueCreate" {
			s.issues = append(s.issues, is)
		}
		return map[string]any{"success": true, "issue": s.issueJSON(is)}, nil
	case "commentCreate":
		var in struct {
			IssueID string `json:"issueId"`
			Body    string `json:"body"`
		}
		if err := json.Unmarshal(vars["input"], &in); err != nil {
			return nil, fail("INVALID_INPUT", "invalid input: %v", err)
		}
		is := s.findIssue(in.IssueID)
		if is == nil {
			return nil, fail("ENTITY_NOT_FOUND", "Entity not found: Issue")
		}
		if in.Body == "" {
			return nil, fail("INVALID_INPUT", "body is required")
		}
		s.seq++
		c := &Comment{ID: fmt.Sprintf("comment-%04d", s.seq), IssueID: is.ID, Body: in.Body}
		c.URL = is.URL + "#comment-" + c.ID
		s.comments = append(s.comments, c)
		return map[string]any{"success": true, "comment": s.commentJSON(c)}, nil
	case "comments":
		var filter struct {
			Body map[string]string `json:"body"`
		}
		_ = json.Unmarshal(vars["filter"], &filter)
		nodes := []any{}
		for i := len(s.comments) - 1; i >= 0 && len(nodes) < first(vars); i-- {
			if matchString(s.comments[i].Body, filter.Body) {
				nodes = append(nodes, s.commentJSON(s.comments[i]))
			}
		}
		return map[string]any{"nodes": nodes}, nil
	}
	return nil, fail("GRAPHQL_VALIDATION_FAILED", "Cannot query field %q", field)
}

func set(dst *string, v *string) {
	if v != nil {
		*dst = *v
	}
}

func first(vars map[string]json.RawMessage) int {
	n := 50
	if raw, ok := vars["first"]; ok {
		_ = json.Unmarshal(raw, &n)
	}
	if n <= 0 || n > 250 {
		n = 50
	}
	return n
}

// matchString applies a Linear StringComparator subset (eq, contains,
// containsIgnoreCase). An empty comparator matches everything.
func matchString(v string, cmp map[string]string) bool {
	for op, want := range cmp {
		switch op {
		case "eq":
			if v != want {
				return false
			}
		case "contains":
			if !strings.Contains(v, want) {
				return false
			}
		case "containsIgnoreCase":
			if !strings.Contains(strings.ToLower(v), strings.ToLower(want)) {
				return false
			}
		}
	}
	return true
}

func (s *Server) findIssue(id string) *Issue {
	for _, i := range s.issues {
		if i.ID == id || i.Identifier == id {
			return i
		}
	}
	return nil
}

func (s *Server) findProject(id string) *Project {
	for _, p := range s.projects {
		if p.ID == id {
			return p
		}
	}
	return nil
}

func (s *Server) projectJSON(p *Project) map[string]any {
	teams := []any{}
	for _, t := range p.TeamIDs {
		teams = append(teams, map[string]any{"id": t, "key": s.opts.Teams[t]})
	}
	return map[string]any{
		"id": p.ID, "name": p.Name, "description": p.Description, "content": p.Content,
		"url": p.URL, "state": p.State, "teams": map[string]any{"nodes": teams},
	}
}

func ref(id string) any {
	if id == "" {
		return nil
	}
	return map[string]any{"id": id}
}

func (s *Server) issueJSON(i *Issue) map[string]any {
	var state any
	if i.StateID != "" {
		state = map[string]any{"id": i.StateID, "name": i.StateID}
	}
	return map[string]any{
		"id": i.ID, "identifier": i.Identifier, "title": i.Title, "description": i.Description,
		"url": i.URL, "priority": i.Priority, "state": state, "project": ref(i.ProjectID),
		"assignee": ref(i.AssigneeID), "team": map[string]any{"id": i.TeamID, "key": s.opts.Teams[i.TeamID]},
	}
}

func (s *Server) commentJSON(c *Comment) map[string]any {
	issue := map[string]any{"id": c.IssueID}
	if is := s.findIssue(c.IssueID); is != nil {
		issue["identifier"] = is.Identifier
	}
	return map[string]any{"id": c.ID, "body": c.Body, "url": c.URL, "issue": issue}
}

func (s *Server) handleTestFail(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Operation string `json:"operation"`
		Mode      string `json:"mode"`
		Count     int    `json:"count"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Operation == "" || req.Mode == "" {
		http.Error(w, "operation and mode are required", http.StatusBadRequest)
		return
	}
	switch req.Mode {
	case "drop_response_after_commit", "error", "rate_limited", "server_error":
	default:
		http.Error(w, "unknown mode "+req.Mode, http.StatusBadRequest)
		return
	}
	s.Fail(req.Operation, req.Mode, req.Count)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// hijackClose drops the connection without a response.
func hijackClose(w http.ResponseWriter) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		panic(http.ErrAbortHandler)
	}
	conn, _, err := hj.Hijack()
	if err != nil {
		panic(http.ErrAbortHandler)
	}
	_ = conn.Close()
}

func (s *Server) keyOK(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case key == "":
		return false
	case s.validKeys != nil:
		return s.validKeys[key]
	case s.opts.APIKey != "":
		return key == s.opts.APIKey
	}
	return true
}

// SetKeys replaces the accepted API keys, as when a key is rotated or
// revoked. An empty list restores Options.APIKey.
func (s *Server) SetKeys(keys []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.validKeys = nil
	if len(keys) > 0 {
		s.validKeys = map[string]bool{}
		for _, k := range keys {
			s.validKeys[k] = true
		}
	}
}

func (s *Server) handleTestKeys(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Valid []string `json:"valid"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	s.SetKeys(body.Valid)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
