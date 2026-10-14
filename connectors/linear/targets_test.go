package linear

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"github.com/darcys22/steadmesh/connectors"
)

func TestOperationTargets(t *testing.T) {
	a := &Adapter{teamID: "team-default"}
	cases := []struct {
		op, params string
		targets    []string
	}{
		{OpProjectRead, `{"id":"p1"}`, []string{"p1"}},
		{OpProjectRead, `{"query":"all"}`, nil},
		{OpProjectRead, `{}`, nil},
		{OpProjectCreate, `{"name":"New"}`, []string{"team-default"}},
		{OpProjectCreate, `{"name":"New","team_ids":["t1","t2"]}`, []string{"t1", "t2"}},
		{OpTaskRead, `{"id":"ENG-1"}`, []string{"ENG-1"}},
		{OpTaskRead, `{"project_id":"p1"}`, []string{"p1"}},
		{OpTaskRead, `{"query":"all"}`, nil},
		{OpTaskWrite, `{"title":"New"}`, []string{"team-default"}},
		{OpTaskWrite, `{"title":"New","team_id":"t1","project_id":"p1"}`, []string{"t1", "p1"}},
		{OpTaskWrite, `{"id":"ENG-1","title":"Update"}`, []string{"ENG-1"}},
		{OpTaskWrite, `{"id":"ENG-1","project_id":"p2"}`, []string{"ENG-1", "p2"}},
		{OpTaskWrite, `{"id":"ENG-1","project_id":""}`, []string{"ENG-1"}},
		{OpCommentRead, `{"issue_id":"ENG-1"}`, []string{"ENG-1"}},
		{OpCommentWrite, `{"issue_id":"ENG-1","body":"Hi"}`, []string{"ENG-1"}},
	}
	for _, tc := range cases {
		t.Run(tc.op+tc.params, func(t *testing.T) {
			got, err := a.OperationTargets(tc.op, json.RawMessage(tc.params))
			if err != nil || !slices.Equal(got, tc.targets) {
				t.Fatalf("targets=%v err=%v want=%v", got, err, tc.targets)
			}
		})
	}
}

func TestTargetAndExecutionParsingAgree(t *testing.T) {
	// Invalid requests must fail before any HTTP call in either path.
	a := &Adapter{teamID: "team-default"}
	cases := []struct{ op, params string }{
		{OpProjectRead, `{"id":"p1","target":"p2"}`},
		{OpProjectRead, `{"id":"p1","ID":"p2"}`},
		{OpProjectRead, `{"id":"p1","id":"p2"}`},
		{OpProjectRead, `{"id":"p1","query":"all"}`},
		{OpProjectRead, `null`},
		{OpProjectRead, `{} {}`},
		{OpProjectCreate, `{"name":"New","team_ids":["t1",""]}`},
		{OpProjectCreate, `{"name":"New","team_id":"t1"}`},
		{OpTaskRead, `{"id":"ENG-1","project_id":"p1"}`},
		{OpTaskWrite, `{"id":"ENG-1","team_id":"t1","title":"New"}`},
		{OpTaskWrite, `{"id":12,"project_id":"p1"}`},
		{OpCommentRead, `{"issue_id":"ENG-1","project_id":"p1"}`},
		{OpCommentWrite, `{"issue_id":"ENG-1","body":""}`},
		{"unknown", `{}`},
	}
	for _, tc := range cases {
		t.Run(tc.op+tc.params, func(t *testing.T) {
			raw := json.RawMessage(tc.params)
			if _, err := a.OperationTargets(tc.op, raw); !errors.Is(err, connectors.ErrPermanent) {
				t.Fatalf("authorization accepted: %v", err)
			}
			if _, err := a.Invoke(context.Background(), tc.op, raw, "op"); !errors.Is(err, connectors.ErrPermanent) {
				t.Fatalf("execution accepted: %v", err)
			}
		})
	}
}
