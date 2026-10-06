package linear

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/darcys22/steadmesh/connectors"
)

const (
	projectFields = `id name description content url state teams { nodes { id key } }`
	issueFields   = `id identifier title description url priority state { id name } project { id } assignee { id } team { id key }`
	commentFields = `id body url issue { id identifier }`

	qViewer        = `query Viewer { viewer { id name email organization { id name urlKey } } }`
	qProject       = `query Project($id: String!) { project(id: $id) { ` + projectFields + ` } }`
	qProjects      = `query Projects($first: Int!, $filter: ProjectFilter) { projects(first: $first, filter: $filter) { nodes { ` + projectFields + ` } } }`
	mProjectCreate = `mutation ProjectCreate($input: ProjectCreateInput!) { projectCreate(input: $input) { success project { ` + projectFields + ` } } }`
	qIssue         = `query Issue($id: String!) { issue(id: $id) { ` + issueFields + ` } }`
	qIssues        = `query Issues($first: Int!, $filter: IssueFilter) { issues(first: $first, filter: $filter) { nodes { ` + issueFields + ` } } }`
	qSearchIssues  = `query SearchIssues($term: String!, $first: Int!) { searchIssues(term: $term, first: $first) { nodes { ` + issueFields + ` } } }`
	mIssueCreate   = `mutation IssueCreate($input: IssueCreateInput!) { issueCreate(input: $input) { success issue { ` + issueFields + ` } } }`
	mIssueUpdate   = `mutation IssueUpdate($id: String!, $input: IssueUpdateInput!) { issueUpdate(id: $id, input: $input) { success issue { ` + issueFields + ` } } }`
	mCommentCreate = `mutation CommentCreate($input: CommentCreateInput!) { commentCreate(input: $input) { success comment { ` + commentFields + ` } } }`
	qComments      = `query Comments($first: Int!, $filter: CommentFilter) { comments(first: $first, filter: $filter) { nodes { ` + commentFields + ` } } }`
)

const (
	defaultFirst          = 25
	maxFirst              = 100
	maxProjectDescription = 255
	// findScan bounds read-back queries.
	findScan = 50
)

// ---- markers -------------------------------------------------------------------

// MarkerPrefix precedes the operation id embedded in created records.
const MarkerPrefix = "steadmesh-op:"

// Marker returns the marker text for an operation id.
func Marker(operationID string) string { return MarkerPrefix + operationID }

func withMarker(text, operationID string) string {
	if operationID == "" {
		return text
	}
	if text == "" {
		return Marker(operationID)
	}
	return text + "\n\n" + Marker(operationID)
}

// hasMarker reports whether text contains exactly this operation's marker
// (not one for an id that merely shares a prefix).
func hasMarker(text, operationID string) bool {
	re := regexp.MustCompile(`(^|\s)` + regexp.QuoteMeta(Marker(operationID)) + `(\s|$)`)
	return re.MatchString(text)
}

// ---- params ----------------------------------------------------------------------

func decode(params json.RawMessage, dst any) error {
	if len(bytes.TrimSpace(params)) == 0 {
		params = json.RawMessage("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(params))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return invalid("params: %v", err)
	}
	if dec.More() {
		return invalid("params: trailing data")
	}
	return nil
}

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: invalid params: %s", connectors.ErrPermanent, fmt.Sprintf(format, args...))
}

func pageSize(n int) (int, error) {
	switch {
	case n == 0:
		return defaultFirst, nil
	case n < 0 || n > maxFirst:
		return 0, invalid("first must be between 1 and %d", maxFirst)
	}
	return n, nil
}

type projectReadParams struct {
	ID    string `json:"id"`
	Query string `json:"query"`
	First int    `json:"first"`
}

type projectCreateParams struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Content     string   `json:"content"`
	TeamIDs     []string `json:"team_ids"`
}

type taskReadParams struct {
	ID        string `json:"id"`
	Query     string `json:"query"`
	ProjectID string `json:"project_id"`
	First     int    `json:"first"`
}

type taskWriteParams struct {
	ID          string  `json:"id"`
	Title       *string `json:"title"`
	Description *string `json:"description"`
	TeamID      string  `json:"team_id"`
	ProjectID   *string `json:"project_id"`
	StateID     *string `json:"state_id"`
	AssigneeID  *string `json:"assignee_id"`
	Priority    *int    `json:"priority"`
}

type commentWriteParams struct {
	IssueID string `json:"issue_id"`
	Body    string `json:"body"`
}

func (a *Adapter) parseProjectCreate(params json.RawMessage) (projectCreateParams, error) {
	var p projectCreateParams
	if err := decode(params, &p); err != nil {
		return p, err
	}
	p.Name = strings.TrimSpace(p.Name)
	if p.Name == "" {
		return p, invalid("name is required")
	}
	if len(p.Description) > maxProjectDescription {
		return p, invalid("description must be at most %d characters (use content for longer text)", maxProjectDescription)
	}
	if len(p.TeamIDs) == 0 {
		if a.teamID == "" {
			return p, invalid("team_ids is required when the connection has no team_id")
		}
		p.TeamIDs = []string{a.teamID}
	}
	return p, nil
}

func (p *taskWriteParams) validate(defaultTeam string) error {
	if p.Priority != nil && (*p.Priority < 0 || *p.Priority > 4) {
		return invalid("priority must be between 0 and 4")
	}
	if p.ID == "" {
		if p.Title == nil || strings.TrimSpace(*p.Title) == "" {
			return invalid("title is required to create a task")
		}
		if p.TeamID == "" {
			p.TeamID = defaultTeam
		}
		if p.TeamID == "" {
			return invalid("team_id is required when the connection has no team_id")
		}
		return nil
	}
	if p.TeamID != "" {
		return invalid("team_id cannot be changed on update")
	}
	if p.Title != nil && strings.TrimSpace(*p.Title) == "" {
		return invalid("title cannot be empty")
	}
	if p.Title == nil && p.Description == nil && p.ProjectID == nil && p.StateID == nil && p.AssigneeID == nil && p.Priority == nil {
		return invalid("update needs at least one field")
	}
	return nil
}

func (p taskWriteParams) input() map[string]any {
	in := map[string]any{}
	put := func(k string, v *string) {
		if v != nil {
			in[k] = *v
		}
	}
	put("title", p.Title)
	put("description", p.Description)
	put("projectId", p.ProjectID)
	put("stateId", p.StateID)
	put("assigneeId", p.AssigneeID)
	if p.Priority != nil {
		in["priority"] = *p.Priority
	}
	return in
}

func parseCommentWrite(params json.RawMessage) (commentWriteParams, error) {
	var p commentWriteParams
	if err := decode(params, &p); err != nil {
		return p, err
	}
	if p.IssueID == "" {
		return p, invalid("issue_id is required")
	}
	if strings.TrimSpace(p.Body) == "" {
		return p, invalid("body is required")
	}
	return p, nil
}

// ---- records -------------------------------------------------------------------

type project struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Content     string `json:"content"`
	URL         string `json:"url"`
	State       string `json:"state"`
	Teams       struct {
		Nodes []struct {
			ID  string `json:"id"`
			Key string `json:"key"`
		} `json:"nodes"`
	} `json:"teams"`
}

type idRef struct {
	ID string `json:"id"`
}

type issue struct {
	ID          string `json:"id"`
	Identifier  string `json:"identifier"`
	Title       string `json:"title"`
	Description string `json:"description"`
	URL         string `json:"url"`
	Priority    int    `json:"priority"`
	State       *struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"state"`
	Project  *idRef `json:"project"`
	Assignee *idRef `json:"assignee"`
	Team     struct {
		ID  string `json:"id"`
		Key string `json:"key"`
	} `json:"team"`
}

type comment struct {
	ID    string `json:"id"`
	Body  string `json:"body"`
	URL   string `json:"url"`
	Issue struct {
		ID         string `json:"id"`
		Identifier string `json:"identifier"`
	} `json:"issue"`
}

func result(receipt string, v any) (connectors.Result, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return connectors.Result{}, fmt.Errorf("%w: encoding result: %v", connectors.ErrPermanent, err)
	}
	return connectors.Result{Receipt: receipt, Data: data}, nil
}

// ---- operations ------------------------------------------------------------------

func (a *Adapter) projectRead(ctx context.Context, params json.RawMessage) (connectors.Result, error) {
	var p projectReadParams
	if err := decode(params, &p); err != nil {
		return connectors.Result{}, err
	}
	if p.ID != "" {
		if p.Query != "" || p.First != 0 {
			return connectors.Result{}, invalid("id cannot be combined with query or first")
		}
		var out struct {
			Project project `json:"project"`
		}
		if err := a.do(ctx, true, qProject, map[string]any{"id": p.ID}, &out); err != nil {
			return connectors.Result{}, err
		}
		return result(out.Project.ID, out.Project)
	}
	first, err := pageSize(p.First)
	if err != nil {
		return connectors.Result{}, err
	}
	vars := map[string]any{"first": first}
	if q := strings.TrimSpace(p.Query); q != "" {
		vars["filter"] = map[string]any{"name": map[string]any{"containsIgnoreCase": q}}
	}
	var out struct {
		Projects struct {
			Nodes []project `json:"nodes"`
		} `json:"projects"`
	}
	if err := a.do(ctx, true, qProjects, vars, &out); err != nil {
		return connectors.Result{}, err
	}
	return result("", map[string]any{"projects": nonNil(out.Projects.Nodes)})
}

func (a *Adapter) projectCreate(ctx context.Context, params json.RawMessage, operationID string) (connectors.Result, error) {
	p, err := a.parseProjectCreate(params)
	if err != nil {
		return connectors.Result{}, err
	}
	// Project descriptions are capped at 255 characters, so the marker goes in
	// the longer markdown content.
	input := map[string]any{
		"name":    p.Name,
		"teamIds": p.TeamIDs,
		"content": withMarker(p.Content, operationID),
	}
	if p.Description != "" {
		input["description"] = p.Description
	}
	var out struct {
		ProjectCreate struct {
			Success bool    `json:"success"`
			Project project `json:"project"`
		} `json:"projectCreate"`
	}
	if err := a.do(ctx, false, mProjectCreate, map[string]any{"input": input}, &out); err != nil {
		return connectors.Result{}, err
	}
	if !out.ProjectCreate.Success || out.ProjectCreate.Project.ID == "" {
		return connectors.Result{}, fmt.Errorf("%w: projectCreate reported no success", connectors.ErrAmbiguous)
	}
	return result(out.ProjectCreate.Project.ID, out.ProjectCreate.Project)
}

func (a *Adapter) taskRead(ctx context.Context, params json.RawMessage) (connectors.Result, error) {
	var p taskReadParams
	if err := decode(params, &p); err != nil {
		return connectors.Result{}, err
	}
	set := 0
	for _, v := range []string{p.ID, p.Query, p.ProjectID} {
		if v != "" {
			set++
		}
	}
	if set != 1 {
		return connectors.Result{}, invalid("exactly one of id, query or project_id is required")
	}
	if p.ID != "" {
		if p.First != 0 {
			return connectors.Result{}, invalid("id cannot be combined with first")
		}
		var out struct {
			Issue issue `json:"issue"`
		}
		if err := a.do(ctx, true, qIssue, map[string]any{"id": p.ID}, &out); err != nil {
			return connectors.Result{}, err
		}
		return result(out.Issue.Identifier, out.Issue)
	}
	first, err := pageSize(p.First)
	if err != nil {
		return connectors.Result{}, err
	}
	var nodes []issue
	if p.Query != "" {
		var out struct {
			SearchIssues struct {
				Nodes []issue `json:"nodes"`
			} `json:"searchIssues"`
		}
		if err := a.do(ctx, true, qSearchIssues, map[string]any{"term": p.Query, "first": first}, &out); err != nil {
			return connectors.Result{}, err
		}
		nodes = out.SearchIssues.Nodes
	} else {
		var out struct {
			Issues struct {
				Nodes []issue `json:"nodes"`
			} `json:"issues"`
		}
		vars := map[string]any{"first": first, "filter": map[string]any{
			"project": map[string]any{"id": map[string]any{"eq": p.ProjectID}},
		}}
		if err := a.do(ctx, true, qIssues, vars, &out); err != nil {
			return connectors.Result{}, err
		}
		nodes = out.Issues.Nodes
	}
	return result("", map[string]any{"issues": nonNil(nodes)})
}

func (a *Adapter) taskWrite(ctx context.Context, params json.RawMessage, operationID string) (connectors.Result, error) {
	var p taskWriteParams
	if err := decode(params, &p); err != nil {
		return connectors.Result{}, err
	}
	if err := p.validate(a.teamID); err != nil {
		return connectors.Result{}, err
	}
	input := p.input()
	var out struct {
		IssueCreate *struct {
			Success bool  `json:"success"`
			Issue   issue `json:"issue"`
		} `json:"issueCreate"`
		IssueUpdate *struct {
			Success bool  `json:"success"`
			Issue   issue `json:"issue"`
		} `json:"issueUpdate"`
	}
	if p.ID == "" {
		desc := ""
		if p.Description != nil {
			desc = *p.Description
		}
		input["teamId"] = p.TeamID
		input["description"] = withMarker(desc, operationID)
		if err := a.do(ctx, false, mIssueCreate, map[string]any{"input": input}, &out); err != nil {
			return connectors.Result{}, err
		}
	} else {
		// Updates set absolute values, so a read-back compares fields instead of
		// relying on a marker.
		if err := a.do(ctx, false, mIssueUpdate, map[string]any{"id": p.ID, "input": input}, &out); err != nil {
			return connectors.Result{}, err
		}
	}
	payload := out.IssueCreate
	if payload == nil {
		payload = out.IssueUpdate
	}
	if payload == nil || !payload.Success || payload.Issue.ID == "" {
		return connectors.Result{}, fmt.Errorf("%w: issue mutation reported no success", connectors.ErrAmbiguous)
	}
	return result(payload.Issue.Identifier, payload.Issue)
}

func (a *Adapter) commentWrite(ctx context.Context, params json.RawMessage, operationID string) (connectors.Result, error) {
	p, err := parseCommentWrite(params)
	if err != nil {
		return connectors.Result{}, err
	}
	var out struct {
		CommentCreate struct {
			Success bool    `json:"success"`
			Comment comment `json:"comment"`
		} `json:"commentCreate"`
	}
	input := map[string]any{"issueId": p.IssueID, "body": withMarker(p.Body, operationID)}
	if err := a.do(ctx, false, mCommentCreate, map[string]any{"input": input}, &out); err != nil {
		return connectors.Result{}, err
	}
	if !out.CommentCreate.Success || out.CommentCreate.Comment.ID == "" {
		return connectors.Result{}, fmt.Errorf("%w: commentCreate reported no success", connectors.ErrAmbiguous)
	}
	return result(out.CommentCreate.Comment.ID, out.CommentCreate.Comment)
}

// ---- read-back -------------------------------------------------------------------

func (a *Adapter) findProject(ctx context.Context, params json.RawMessage, operationID string) (*connectors.Result, error) {
	p, err := a.parseProjectCreate(params)
	if err != nil {
		return nil, err
	}
	var out struct {
		Projects struct {
			Nodes []project `json:"nodes"`
		} `json:"projects"`
	}
	vars := map[string]any{"first": findScan, "filter": map[string]any{"name": map[string]any{"eq": p.Name}}}
	if err := a.do(ctx, true, qProjects, vars, &out); err != nil {
		return nil, err
	}
	for _, pr := range out.Projects.Nodes {
		if hasMarker(pr.Content, operationID) || hasMarker(pr.Description, operationID) {
			r, err := result(pr.ID, pr)
			return &r, err
		}
	}
	return nil, nil
}

func (a *Adapter) findTask(ctx context.Context, params json.RawMessage, operationID string) (*connectors.Result, error) {
	var p taskWriteParams
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	if err := p.validate(a.teamID); err != nil {
		return nil, err
	}
	if p.ID != "" {
		var out struct {
			Issue issue `json:"issue"`
		}
		if err := a.do(ctx, true, qIssue, map[string]any{"id": p.ID}, &out); err != nil {
			return nil, err
		}
		if !updateApplied(p, out.Issue) {
			return nil, nil
		}
		r, err := result(out.Issue.Identifier, out.Issue)
		return &r, err
	}
	var out struct {
		Issues struct {
			Nodes []issue `json:"nodes"`
		} `json:"issues"`
	}
	vars := map[string]any{"first": findScan, "filter": map[string]any{
		"description": map[string]any{"contains": Marker(operationID)},
	}}
	if err := a.do(ctx, true, qIssues, vars, &out); err != nil {
		return nil, err
	}
	for _, is := range out.Issues.Nodes {
		if hasMarker(is.Description, operationID) {
			r, err := result(is.Identifier, is)
			return &r, err
		}
	}
	return nil, nil
}

// updateApplied reports whether every field requested by an update already
// holds the requested value.
func updateApplied(p taskWriteParams, is issue) bool {
	eq := func(want *string, got string) bool { return want == nil || *want == got }
	ref := func(r *idRef) string {
		if r == nil {
			return ""
		}
		return r.ID
	}
	state := ""
	if is.State != nil {
		state = is.State.ID
	}
	return eq(p.Title, is.Title) && eq(p.Description, is.Description) &&
		eq(p.ProjectID, ref(is.Project)) && eq(p.AssigneeID, ref(is.Assignee)) &&
		eq(p.StateID, state) && (p.Priority == nil || *p.Priority == is.Priority)
}

func (a *Adapter) findComment(ctx context.Context, operationID string) (*connectors.Result, error) {
	var out struct {
		Comments struct {
			Nodes []comment `json:"nodes"`
		} `json:"comments"`
	}
	vars := map[string]any{"first": findScan, "filter": map[string]any{
		"body": map[string]any{"contains": Marker(operationID)},
	}}
	if err := a.do(ctx, true, qComments, vars, &out); err != nil {
		return nil, err
	}
	for _, c := range out.Comments.Nodes {
		if hasMarker(c.Body, operationID) {
			r, err := result(c.ID, c)
			return &r, err
		}
	}
	return nil, nil
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
