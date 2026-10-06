package linear

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/darcys22/steadmesh/connectors"
	fakelinear "github.com/darcys22/steadmesh/tests/fakes/linear"
)

func setup(t *testing.T, account string) (*fakelinear.Server, *Adapter) {
	t.Helper()
	fake := fakelinear.New(fakelinear.Options{APIKey: "lin_api_test"})
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	a, err := newAdapter(connectors.Config{
		Key: "linear", AccountID: account, Endpoint: srv.URL,
		Secret: map[string]string{"api_key": "lin_api_test"},
		Extra:  map[string]string{"team_id": "team-eng"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return fake, a
}

func invoke(t *testing.T, a *Adapter, op, params, opID string) connectors.Result {
	t.Helper()
	r, err := a.Invoke(context.Background(), op, json.RawMessage(params), opID)
	if err != nil {
		t.Fatalf("%s %s: %v", op, params, err)
	}
	return r
}

func TestEndpointURL(t *testing.T) {
	for in, want := range map[string]string{
		"":                      DefaultEndpoint,
		"http://fakes:8091":     "http://fakes:8091/graphql",
		"http://fakes:8091/":    "http://fakes:8091/graphql",
		"http://fakes:8091/gql": "http://fakes:8091/gql",
	} {
		got, err := endpointURL(in)
		if err != nil || got != want {
			t.Errorf("endpointURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

func TestVerify(t *testing.T) {
	_, a := setup(t, "org-fake")
	if err := a.Verify(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, wrong := setup(t, "org-other")
	if err := wrong.Verify(context.Background()); !errors.Is(err, connectors.ErrUnauthorized) {
		t.Fatalf("wrong org: %v", err)
	}
	fake := fakelinear.New(fakelinear.Options{APIKey: "lin_api_right"})
	srv := httptest.NewServer(fake)
	defer srv.Close()
	bad, _ := newAdapter(connectors.Config{Endpoint: srv.URL, Secret: map[string]string{"api_key": "lin_api_wrong"}})
	if err := bad.Verify(context.Background()); !errors.Is(err, connectors.ErrUnauthorized) {
		t.Fatalf("bad key: %v", err)
	}
}

func TestReadOnly(t *testing.T) {
	_, a := setup(t, "")
	for op, want := range map[string]bool{
		OpProjectRead: true, OpTaskRead: true, OpProjectCreate: false, OpTaskWrite: false, OpCommentWrite: false,
	} {
		if a.ReadOnly(op) != want {
			t.Errorf("ReadOnly(%s) != %v", op, want)
		}
	}
}

func TestProjectCreateAndRead(t *testing.T) {
	fake, a := setup(t, "")
	r := invoke(t, a, OpProjectCreate, `{"name":"Billing revamp","description":"short","content":"Long plan"}`, "op-p1")
	if r.Receipt == "" {
		t.Fatal("no receipt")
	}
	st := fake.State()
	if len(st.Projects) != 1 {
		t.Fatalf("projects = %+v", st.Projects)
	}
	p := st.Projects[0]
	if p.Description != "short" || !strings.Contains(p.Content, "Long plan") || !strings.Contains(p.Content, "steadmesh-op:op-p1") {
		t.Fatalf("project = %+v", p)
	}
	if len(p.TeamIDs) != 1 || p.TeamIDs[0] != "team-eng" {
		t.Fatalf("default team not applied: %+v", p.TeamIDs)
	}

	got := invoke(t, a, OpProjectRead, `{"id":"`+r.Receipt+`"}`, "")
	var one project
	_ = json.Unmarshal(got.Data, &one)
	if one.Name != "Billing revamp" || got.Receipt != r.Receipt {
		t.Fatalf("read by id = %+v", one)
	}
	list := invoke(t, a, OpProjectRead, `{"query":"billing"}`, "")
	var many struct{ Projects []project }
	_ = json.Unmarshal(list.Data, &many)
	if len(many.Projects) != 1 {
		t.Fatalf("search = %s", list.Data)
	}
	none := invoke(t, a, OpProjectRead, `{"query":"nothing"}`, "")
	if string(none.Data) != `{"projects":[]}` {
		t.Fatalf("empty search = %s", none.Data)
	}
}

func TestTaskWriteCreateUpdateAndRead(t *testing.T) {
	fake, a := setup(t, "")
	proj := invoke(t, a, OpProjectCreate, `{"name":"P"}`, "op-proj")
	created := invoke(t, a, OpTaskWrite, `{"title":"Fix login","description":"Steps","project_id":"`+proj.Receipt+`","priority":2}`, "op-t1")
	if created.Receipt != "ENG-1" {
		t.Fatalf("receipt = %q", created.Receipt)
	}
	is := fake.State().Issues[0]
	if is.Description != "Steps\n\nsteadmesh-op:op-t1" || is.ProjectID != proj.Receipt || is.Priority != 2 {
		t.Fatalf("issue = %+v", is)
	}

	upd := invoke(t, a, OpTaskWrite, `{"id":"ENG-1","title":"Fix login flow","state_id":"state-done"}`, "op-t2")
	var u issue
	_ = json.Unmarshal(upd.Data, &u)
	if u.Title != "Fix login flow" || u.State == nil || u.State.ID != "state-done" {
		t.Fatalf("updated = %s", upd.Data)
	}
	if is := fake.State().Issues[0]; strings.Contains(is.Description, "op-t2") {
		t.Fatal("updates must not rewrite the description")
	}

	byID := invoke(t, a, OpTaskRead, `{"id":"ENG-1"}`, "")
	if byID.Receipt != "ENG-1" {
		t.Fatalf("read = %+v", byID)
	}
	search := invoke(t, a, OpTaskRead, `{"query":"login"}`, "")
	byProject := invoke(t, a, OpTaskRead, `{"project_id":"`+proj.Receipt+`"}`, "")
	for _, r := range []connectors.Result{search, byProject} {
		var out struct{ Issues []issue }
		_ = json.Unmarshal(r.Data, &out)
		if len(out.Issues) != 1 || out.Issues[0].Identifier != "ENG-1" {
			t.Fatalf("list = %s", r.Data)
		}
	}
}

func TestCommentWrite(t *testing.T) {
	fake, a := setup(t, "")
	invoke(t, a, OpTaskWrite, `{"title":"T"}`, "op-t")
	r := invoke(t, a, OpCommentWrite, `{"issue_id":"ENG-1","body":"Looks good"}`, "op-c1")
	c := fake.State().Comments
	if len(c) != 1 || c[0].ID != r.Receipt || c[0].Body != "Looks good\n\nsteadmesh-op:op-c1" {
		t.Fatalf("comments = %+v", c)
	}
}

func TestInvalidParamsArePermanent(t *testing.T) {
	_, a := setup(t, "")
	cases := []struct{ op, params string }{
		{OpProjectCreate, `{}`},
		{OpProjectCreate, `{"name":"x","bogus":1}`},
		{OpProjectCreate, `{"name":"x","description":"` + strings.Repeat("d", 256) + `"}`},
		{OpProjectCreate, `not json`},
		{OpProjectRead, `{"id":"p","query":"q"}`},
		{OpProjectRead, `{"first":1000}`},
		{OpTaskRead, `{}`},
		{OpTaskRead, `{"id":"a","query":"b"}`},
		{OpTaskWrite, `{}`},
		{OpTaskWrite, `{"title":" "}`},
		{OpTaskWrite, `{"id":"ENG-1"}`},
		{OpTaskWrite, `{"id":"ENG-1","team_id":"t"}`},
		{OpTaskWrite, `{"title":"x","priority":9}`},
		{OpCommentWrite, `{"issue_id":"ENG-1"}`},
		{OpCommentWrite, `{"body":"x"}`},
		{"task.delete", `{}`},
	}
	for _, tc := range cases {
		if _, err := a.Invoke(context.Background(), tc.op, json.RawMessage(tc.params), "op"); !errors.Is(err, connectors.ErrPermanent) {
			t.Errorf("%s %s: want ErrPermanent, got %v", tc.op, tc.params, err)
		}
	}
}

func TestRemoteValidationErrorIsPermanent(t *testing.T) {
	_, a := setup(t, "")
	_, err := a.Invoke(context.Background(), OpCommentWrite, json.RawMessage(`{"issue_id":"ENG-404","body":"x"}`), "op")
	if !errors.Is(err, connectors.ErrPermanent) {
		t.Fatalf("want ErrPermanent, got %v", err)
	}
}

func TestErrorMapping(t *testing.T) {
	cases := []struct {
		field, mode string
		op, params  string
		want        error
	}{
		{"issueCreate", "rate_limited", OpTaskWrite, `{"title":"x"}`, connectors.ErrRetryable},
		{"issueCreate", "server_error", OpTaskWrite, `{"title":"x"}`, connectors.ErrAmbiguous},
		{"issueCreate", "error", OpTaskWrite, `{"title":"x"}`, connectors.ErrPermanent},
		{"issueCreate", "drop_response_after_commit", OpTaskWrite, `{"title":"x"}`, connectors.ErrAmbiguous},
		// Reads have no side effects, so an unknown outcome is retryable.
		{"searchIssues", "drop_response_after_commit", OpTaskRead, `{"query":"x"}`, connectors.ErrRetryable},
		{"searchIssues", "server_error", OpTaskRead, `{"query":"x"}`, connectors.ErrRetryable},
	}
	for _, tc := range cases {
		t.Run(tc.field+"/"+tc.mode, func(t *testing.T) {
			fake, a := setup(t, "")
			fake.Fail(tc.field, tc.mode, 1)
			_, err := a.Invoke(context.Background(), tc.op, json.RawMessage(tc.params), "op-x")
			if !errors.Is(err, tc.want) {
				t.Fatalf("want %v, got %v", tc.want, err)
			}
		})
	}
}

func TestFindByOperationAfterLostResponse(t *testing.T) {
	cases := []struct {
		name, field, op, params string
		prepare                 func(t *testing.T, a *Adapter)
	}{
		{"project", "projectCreate", OpProjectCreate, `{"name":"Lost project"}`, nil},
		{"issue", "issueCreate", OpTaskWrite, `{"title":"Lost issue"}`, nil},
		{"comment", "commentCreate", OpCommentWrite, `{"issue_id":"ENG-1","body":"lost"}`, func(t *testing.T, a *Adapter) {
			invoke(t, a, OpTaskWrite, `{"title":"host"}`, "op-host")
		}},
		{"update", "issueUpdate", OpTaskWrite, `{"id":"ENG-1","title":"renamed"}`, func(t *testing.T, a *Adapter) {
			invoke(t, a, OpTaskWrite, `{"title":"original"}`, "op-host")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake, a := setup(t, "")
			if tc.prepare != nil {
				tc.prepare(t, a)
			}
			ctx := context.Background()
			params := json.RawMessage(tc.params)

			// Before any attempt nothing is found.
			if r, err := a.FindByOperation(ctx, tc.op, params, "op-lost"); err != nil || r != nil {
				t.Fatalf("before: %+v %v", r, err)
			}
			fake.Fail(tc.field, "drop_response_after_commit", 1)
			if _, err := a.Invoke(ctx, tc.op, params, "op-lost"); !errors.Is(err, connectors.ErrAmbiguous) {
				t.Fatalf("invoke: want ErrAmbiguous, got %v", err)
			}
			r, err := a.FindByOperation(ctx, tc.op, params, "op-lost")
			if err != nil || r == nil || r.Receipt == "" {
				t.Fatalf("after: %+v %v", r, err)
			}
			// A different operation id with a shared prefix is not a match.
			if tc.name != "update" {
				if r, err := a.FindByOperation(ctx, tc.op, params, "op-los"); err != nil || r != nil {
					t.Fatalf("prefix id matched: %+v %v", r, err)
				}
			}
		})
	}
}

func TestFindByOperationReadOnlyIsNil(t *testing.T) {
	_, a := setup(t, "")
	if r, err := a.FindByOperation(context.Background(), OpTaskRead, json.RawMessage(`{"id":"x"}`), "op"); r != nil || err != nil {
		t.Fatalf("%+v %v", r, err)
	}
}
