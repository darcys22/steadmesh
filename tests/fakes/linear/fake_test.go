package fakelinear

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func gql(t *testing.T, base, key, query string, vars map[string]any) (int, map[string]any, error) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"query": query, "variables": vars})
	req, _ := http.NewRequest(http.MethodPost, base+"/graphql", strings.NewReader(string(body)))
	req.Header.Set("Authorization", key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out, nil
}

func TestRootField(t *testing.T) {
	for q, want := range map[string]string{
		`query Viewer { viewer { id } }`: "viewer",
		`mutation M($input: IssueCreateInput!) { issueCreate(input: $input) { success } }`: "issueCreate",
		`{ projects(first: 1) { nodes { id } } }`:                                          "projects",
		`nonsense`: "",
	} {
		if got := RootField(q); got != want {
			t.Errorf("RootField(%q) = %q, want %q", q, got, want)
		}
	}
}

func TestAuthAndState(t *testing.T) {
	f := New(Options{APIKey: "lin_api_k"})
	srv := httptest.NewServer(f)
	defer srv.Close()

	if code, _, _ := gql(t, srv.URL, "wrong", `query { viewer { id } }`, nil); code != http.StatusUnauthorized {
		t.Fatalf("bad key: %d", code)
	}
	_, out, err := gql(t, srv.URL, "lin_api_k", `mutation ($input: IssueCreateInput!) { issueCreate(input: $input) { success } }`,
		map[string]any{"input": map[string]any{"teamId": "team-eng", "title": "T", "description": "steadmesh-op:1"}})
	if err != nil {
		t.Fatal(err)
	}
	issue := out["data"].(map[string]any)["issueCreate"].(map[string]any)["issue"].(map[string]any)
	if issue["identifier"] != "ENG-1" {
		t.Fatalf("issue %v", issue)
	}

	resp, err := http.Get(srv.URL + "/_test/state")
	if err != nil {
		t.Fatal(err)
	}
	var st State
	_ = json.NewDecoder(resp.Body).Decode(&st)
	resp.Body.Close()
	if len(st.Issues) != 1 || st.Issues[0].Description != "steadmesh-op:1" {
		t.Fatalf("state %+v", st)
	}
}

func TestFailControl(t *testing.T) {
	f := New(Options{})
	srv := httptest.NewServer(f)
	defer srv.Close()

	resp, _ := http.Post(srv.URL+"/_test/fail", "application/json", strings.NewReader(`{"operation":"issueCreate","mode":"bogus"}`))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown mode accepted: %d", resp.StatusCode)
	}
	resp, _ = http.Post(srv.URL+"/_test/fail", "application/json",
		strings.NewReader(`{"operation":"projectCreate","mode":"drop_response_after_commit","count":1}`))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("fail: %d", resp.StatusCode)
	}
	q := `mutation ($input: ProjectCreateInput!) { projectCreate(input: $input) { success } }`
	vars := map[string]any{"input": map[string]any{"name": "P", "teamIds": []string{"team-eng"}}}
	if _, _, err := gql(t, srv.URL, "k", q, vars); err == nil {
		t.Fatal("response should be dropped")
	}
	if n := len(f.State().Projects); n != 1 {
		t.Fatalf("mutation should be committed, projects=%d", n)
	}
	// The rule is consumed: the next call succeeds normally.
	if code, out, err := gql(t, srv.URL, "k", q, vars); err != nil || code != 200 || out["errors"] != nil {
		t.Fatalf("second call: %d %v %v", code, out, err)
	}
}

func TestKeyRotation(t *testing.T) {
	f := New(Options{APIKey: "lin_old"})
	srv := httptest.NewServer(f)
	defer srv.Close()
	const q = `query { viewer { id } }`
	if code, _, _ := gql(t, srv.URL, "lin_old", q, nil); code != http.StatusOK {
		t.Fatalf("configured key: %d", code)
	}
	f.SetKeys([]string{"lin_new"})
	if code, _, _ := gql(t, srv.URL, "lin_old", q, nil); code != http.StatusUnauthorized {
		t.Fatalf("revoked key: %d", code)
	}
	if code, _, _ := gql(t, srv.URL, "lin_new", q, nil); code != http.StatusOK {
		t.Fatalf("rotated key: %d", code)
	}
	resp, err := http.Post(srv.URL+"/_test/keys", "application/json", strings.NewReader(`{"valid":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if code, _, _ := gql(t, srv.URL, "lin_old", q, nil); code != http.StatusOK {
		t.Fatalf("configured key not restored: %d", code)
	}
}
