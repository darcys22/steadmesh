package linear

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/darcys22/steadmesh/connectors"
	fakelinear "github.com/darcys22/steadmesh/tests/fakes/linear"
)

type readBackRequest struct {
	field string
	vars  string
	data  any
}

// Ignore the requested filters when returning fixtures: recovery must validate
// the response's destination too, even when a marker exists on another record.
func readBackAdapter(t *testing.T, requests ...readBackRequest) *Adapter {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i := int(calls.Add(1)) - 1
		if i >= len(requests) {
			t.Errorf("unexpected request %d", i+1)
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		want := requests[i]
		var req struct {
			Query     string `json:"query"`
			Variables any    `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		if got := fakelinear.RootField(req.Query); got != want.field {
			t.Errorf("request %d field = %q, want %q", i+1, got, want.field)
		}
		var vars any
		if err := json.Unmarshal([]byte(want.vars), &vars); err != nil {
			t.Error(err)
			return
		}
		if !reflect.DeepEqual(req.Variables, vars) {
			t.Errorf("request %d variables = %#v, want %#v", i+1, req.Variables, vars)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{want.field: want.data}})
	}))
	t.Cleanup(func() {
		srv.Close()
		if got := int(calls.Load()); got != len(requests) {
			t.Errorf("requests = %d, want %d", got, len(requests))
		}
	})
	a, err := newAdapter(connectors.Config{
		Endpoint: srv.URL,
		Secret:   map[string]string{"api_key": "fixture-only"},
		Extra:    map[string]string{"team_id": "team-eng"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func recoveryProject(teams ...string) map[string]any {
	nodes := make([]any, 0, len(teams))
	for _, id := range teams {
		nodes = append(nodes, map[string]any{"id": id})
	}
	return map[string]any{
		"id": "project-created", "name": "New project", "content": Marker("recover"),
		"teams": map[string]any{"nodes": nodes, "pageInfo": map[string]any{"hasNextPage": false}},
	}
}

func recoveryIssue(team, projectID string) map[string]any {
	var project any
	if projectID != "" {
		project = map[string]any{"id": projectID}
	}
	return map[string]any{
		"id": "issue-created", "identifier": "ENG-1", "title": "New issue", "description": Marker("recover"),
		"team": map[string]any{"id": team}, "project": project,
	}
}

func assertReadBack(t *testing.T, a *Adapter, op, params string, want bool) {
	t.Helper()
	r, err := a.FindByOperation(context.Background(), op, json.RawMessage(params), "recover")
	if err != nil || (r != nil) != want {
		t.Fatalf("FindByOperation = %+v, %v; want found=%v", r, err, want)
	}
	if want && r.Receipt == "" {
		t.Fatal("recovery has no receipt")
	}
}

func TestProjectReadBackValidatesExactTeams(t *testing.T) {
	for _, tc := range []struct {
		name   string
		teams  []string
		mutate func(map[string]any)
		want   bool
	}{
		{name: "same teams reordered", teams: []string{"team-ops", "team-eng"}, want: true},
		{name: "another team", teams: []string{"team-other"}},
		{name: "additional team", teams: []string{"team-eng", "team-ops", "team-other"}},
		{name: "missing requested team", teams: []string{"team-eng"}},
		{name: "missing all teams"},
		{name: "unknown team", teams: []string{"team-eng", ""}},
		{name: "truncated teams", teams: []string{"team-eng", "team-ops"}, mutate: func(p map[string]any) {
			p["teams"].(map[string]any)["pageInfo"] = map[string]any{"hasNextPage": true}
		}},
		{name: "unknown team completeness", teams: []string{"team-eng", "team-ops"}, mutate: func(p map[string]any) {
			delete(p["teams"].(map[string]any), "pageInfo")
		}},
		{name: "unknown next page", teams: []string{"team-eng", "team-ops"}, mutate: func(p map[string]any) {
			p["teams"].(map[string]any)["pageInfo"] = map[string]any{}
		}},
		{name: "missing project identity", teams: []string{"team-eng", "team-ops"}, mutate: func(p map[string]any) { delete(p, "id") }},
		{name: "different name", teams: []string{"team-eng", "team-ops"}, mutate: func(p map[string]any) { p["name"] = "Copied" }},
		{name: "marker prefix", teams: []string{"team-eng", "team-ops"}, mutate: func(p map[string]any) { p["content"] = Marker("recover-other") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := recoveryProject(tc.teams...)
			if tc.mutate != nil {
				tc.mutate(p)
			}
			a := readBackAdapter(t, readBackRequest{"projects",
				`{"first":50,"filter":{"name":{"eq":"New project"},"accessibleTeams":{"id":{"in":["team-eng","team-ops"]}}}}`,
				map[string]any{"nodes": []any{p}}})
			assertReadBack(t, a, OpProjectCreate, `{"name":"New project","team_ids":["team-eng","team-ops"]}`, tc.want)
		})
	}
}

func TestTaskCreateReadBackValidatesTeamAndProject(t *testing.T) {
	for _, tc := range []struct {
		name, params, projectFilter, team, project string
		mutate                                     func(map[string]any)
		want                                       bool
	}{
		{name: "default team", team: "team-eng", want: true},
		{name: "another team", team: "team-other"},
		{name: "missing team"},
		{name: "unexpected project", team: "team-eng", project: "project-other"},
		{name: "explicit empty project", params: `,"project_id":""`, team: "team-eng", want: true},
		{name: "requested project", params: `,"project_id":"project-allowed"`, projectFilter: `{"id":{"eq":"project-allowed"}}`, team: "team-eng", project: "project-allowed", want: true},
		{name: "different project", params: `,"project_id":"project-allowed"`, projectFilter: `{"id":{"eq":"project-allowed"}}`, team: "team-eng", project: "project-other"},
		{name: "missing project", params: `,"project_id":"project-allowed"`, projectFilter: `{"id":{"eq":"project-allowed"}}`, team: "team-eng"},
		{name: "missing identity", team: "team-eng", mutate: func(is map[string]any) { delete(is, "id") }},
		{name: "missing receipt", team: "team-eng", mutate: func(is map[string]any) { delete(is, "identifier") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			is := recoveryIssue(tc.team, tc.project)
			if tc.mutate != nil {
				tc.mutate(is)
			}
			filter := tc.projectFilter
			if filter == "" {
				filter = `{"null":true}`
			}
			a := readBackAdapter(t, readBackRequest{"issues",
				`{"first":50,"filter":{"description":{"contains":"steadmesh-op:recover"},"team":{"id":{"eq":"team-eng"}},"project":` + filter + `}}`,
				map[string]any{"nodes": []any{is}}})
			assertReadBack(t, a, OpTaskWrite, `{"title":"New issue"`+tc.params+`}`, tc.want)
		})
	}
}

func TestTaskUpdateReadBackValidatesIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, id string
		mutate   func(map[string]any)
		want     bool
	}{
		{name: "canonical ID", id: "issue-created", want: true},
		{name: "identifier", id: "ENG-1", want: true},
		{name: "different issue", id: "ENG-2"},
		{name: "no issue", id: "ENG-1", mutate: func(is map[string]any) { delete(is, "id") }},
		{name: "fields not applied", id: "ENG-1", mutate: func(is map[string]any) { is["title"] = "Old title" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			is := recoveryIssue("team-eng", "")
			if tc.mutate != nil {
				tc.mutate(is)
			}
			a := readBackAdapter(t, readBackRequest{"issue", `{"id":"` + tc.id + `"}`, is})
			assertReadBack(t, a, OpTaskWrite, `{"id":"`+tc.id+`","title":"New issue"}`, tc.want)
		})
	}
}

func TestCommentReadBackValidatesOriginalIssue(t *testing.T) {
	for _, tc := range []struct {
		name, id, returnedIssue string
		mutate                  func(map[string]any)
		want                    bool
	}{
		{name: "canonical ID", id: "issue-created", returnedIssue: "issue-created", want: true},
		{name: "identifier", id: "ENG-1", returnedIssue: "issue-created", want: true},
		{name: "different issue", id: "ENG-1", returnedIssue: "issue-other"},
		{name: "missing issue", id: "ENG-1"},
		{name: "missing comment ID", id: "ENG-1", returnedIssue: "issue-created", mutate: func(c map[string]any) { delete(c, "id") }},
		{name: "marker prefix", id: "ENG-1", returnedIssue: "issue-created", mutate: func(c map[string]any) { c["body"] = Marker("recover-other") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := map[string]any{"id": "comment-created", "body": Marker("recover"), "issue": map[string]any{"id": tc.returnedIssue}}
			if tc.mutate != nil {
				tc.mutate(c)
			}
			a := readBackAdapter(t,
				readBackRequest{"issue", `{"id":"` + tc.id + `"}`, recoveryIssue("team-eng", "")},
				readBackRequest{"comments", `{"first":50,"filter":{"body":{"contains":"steadmesh-op:recover"},"issue":{"id":{"eq":"issue-created"}}}}`, map[string]any{"nodes": []any{c}}},
			)
			assertReadBack(t, a, OpCommentWrite, `{"issue_id":"`+tc.id+`","body":"Comment"}`, tc.want)
		})
	}
}

func TestCommentReadBackRejectsUnprovenIssue(t *testing.T) {
	a := readBackAdapter(t, readBackRequest{"issue", `{"id":"ENG-2"}`, recoveryIssue("team-eng", "")})
	assertReadBack(t, a, OpCommentWrite, `{"issue_id":"ENG-2","body":"Comment"}`, false)
}

func TestCommentReadBackValidatesParamsBeforeReading(t *testing.T) {
	for _, params := range []string{`{}`, `{"issue_id":"ENG-1"}`, `{"body":"Comment"}`, `{"issue_id":"ENG-1","body":"Comment","unknown":true}`} {
		t.Run(params, func(t *testing.T) {
			a := readBackAdapter(t)
			r, err := a.FindByOperation(context.Background(), OpCommentWrite, json.RawMessage(params), "recover")
			if r != nil || !errors.Is(err, connectors.ErrPermanent) {
				t.Fatalf("invalid params = %+v, %v", r, err)
			}
		})
	}
}
