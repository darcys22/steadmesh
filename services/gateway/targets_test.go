package gateway_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/darcys22/steadmesh/connectors"
	gh "github.com/darcys22/steadmesh/connectors/github"
	"github.com/darcys22/steadmesh/connectors/linear"
	"github.com/darcys22/steadmesh/pkg/compile"
	"github.com/darcys22/steadmesh/services/gateway"
	"github.com/darcys22/steadmesh/services/internal/fakeconn"
	"github.com/darcys22/steadmesh/services/store"
)

type boundTracker struct {
	*fakeconn.Tracker
	resolver connectors.TargetResolver
}

func (t *boundTracker) OperationTargets(op string, raw json.RawMessage) ([]string, error) {
	return t.resolver.OperationTargets(op, raw)
}

func grantTargets(seat *store.Seat, op string, targets []string) {
	seat.Manifest.Capabilities = []compile.Capability{{Resource: "connection:tracker", Operations: []string{op}, Targets: targets}}
}

func TestGitHubGatewayBindsExecutionRepository(t *testing.T) {
	for _, tc := range []struct {
		name, params string
		want         error
	}{
		{"allowed", `{"repo":"acme/allowed"}`, nil},
		{"ungranted", `{"repo":"acme/private"}`, gateway.ErrForbidden},
		{"forged target", `{"repo":"acme/private","target":"acme/allowed"}`, gateway.ErrInvalid},
		{"forged project", `{"repo":"acme/private","project_id":"acme/allowed"}`, gateway.ErrInvalid},
		{"case alias", `{"repo":"acme/allowed","Repo":"acme/private"}`, gateway.ErrInvalid},
		{"duplicate key", `{"repo":"acme/allowed","repo":"acme/private"}`, gateway.ErrInvalid},
		{"wrong type", `{"repo":["acme/allowed"]}`, gateway.ErrInvalid},
		{"null", `null`, gateway.ErrInvalid},
		{"array", `[]`, gateway.ErrInvalid},
		{"trailing object", `{"repo":"acme/allowed"} {}`, gateway.ErrInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.URL.Path != "/repos/acme/allowed" {
					t.Errorf("executed wrong repository: %s", r.URL.Path)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"full_name":"acme/allowed"}`))
			}))
			defer srv.Close()
			tr, err := gh.New(connectors.Config{Endpoint: srv.URL, Secret: map[string]string{"token": "fixture"}})
			if err != nil {
				t.Fatal(err)
			}
			g, seat := setup(t, &fakeconn.Tracker{}, "lead")
			g.Trackers = trackers{tr}
			grantTargets(seat, "repo.read", []string{"acme/allowed"})
			op, err := invoke(g, seat, "repo.read", tc.params)
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v want %v", err, tc.want)
			}
			if tc.want != nil {
				if calls != 0 || len(g.Ledger.(*ledger).ops) != 0 {
					t.Fatalf("denied call reached external API or ledger: %d", calls)
				}
			} else if op.Status != store.OpSucceeded || op.Target != "acme/allowed" || calls != 1 {
				t.Fatalf("op=%+v calls=%d", op, calls)
			}
		})
	}
}

func TestLinearGatewayBindsAllExecutionTargets(t *testing.T) {
	real, err := linear.New(connectors.Config{Secret: map[string]string{"api_key": "fixture"}, Extra: map[string]string{"team_id": "default-team"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, op, params string
		targets          []string
		want             error
		recorded         string
	}{
		{"read exact id", "project.read", `{"id":"p1"}`, []string{"p1"}, nil, "p1"},
		{"read wrong id", "project.read", `{"id":"p2"}`, []string{"p1"}, gateway.ErrForbidden, ""},
		{"forged read target", "project.read", `{"id":"p2","target":"p1"}`, []string{"p1"}, gateway.ErrInvalid, ""},
		{"unbounded list", "project.read", `{}`, []string{"*"}, gateway.ErrForbidden, ""},
		{"unbounded search", "task.read", `{"query":"all"}`, []string{"*"}, gateway.ErrForbidden, ""},
		{"unscoped search", "task.read", `{"query":"all"}`, nil, nil, ""},
		{"task read id", "task.read", `{"id":"ENG-1"}`, []string{"ENG-1"}, nil, "ENG-1"},
		{"project bounded task list", "task.read", `{"project_id":"p1"}`, []string{"p1"}, nil, "p1"},
		{"wrong task with allowed project", "task.write", `{"id":"ENG-2","project_id":"p1"}`, []string{"p1"}, gateway.ErrForbidden, ""},
		{"allowed task wrong project", "task.write", `{"id":"ENG-1","project_id":"p2"}`, []string{"ENG-1"}, gateway.ErrForbidden, ""},
		{"task and project allowed", "task.write", `{"id":"ENG-1","project_id":"p1"}`, []string{"ENG-1", "p1"}, nil, `["ENG-1","p1"]`},
		{"create default team", "task.write", `{"title":"New"}`, []string{"default-team"}, nil, "default-team"},
		{"create wrong default team", "task.write", `{"title":"New"}`, []string{"other-team"}, gateway.ErrForbidden, ""},
		{"create in unauthorized team", "task.write", `{"title":"New","team_id":"other-team","project_id":"p1"}`, []string{"p1"}, gateway.ErrForbidden, ""},
		{"project default team", "project.create", `{"name":"New"}`, []string{"default-team"}, nil, "default-team"},
		{"project one team disallowed", "project.create", `{"name":"New","team_ids":["t1","t2"]}`, []string{"t1"}, gateway.ErrForbidden, ""},
		{"project all teams allowed", "project.create", `{"name":"New","team_ids":["t1","t2"]}`, []string{"t1", "t2"}, nil, `["t1","t2"]`},
		{"comment read issue", "comment.read", `{"issue_id":"ENG-1"}`, []string{"ENG-1"}, nil, "ENG-1"},
		{"comment wrong issue", "comment.write", `{"issue_id":"ENG-2","body":"Hi"}`, []string{"ENG-1"}, gateway.ErrForbidden, ""},
		{"comment forged target", "comment.write", `{"issue_id":"ENG-2","body":"Hi","target":"ENG-1"}`, []string{"ENG-1"}, gateway.ErrInvalid, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeconn.Tracker{}
			tr := &boundTracker{Tracker: fake, resolver: real.(connectors.TargetResolver)}
			g, seat := setup(t, fake, "lead")
			g.Trackers = trackers{tr}
			grantTargets(seat, tc.op, tc.targets)
			op, err := invoke(g, seat, tc.op, tc.params)
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v want %v", err, tc.want)
			}
			if tc.want != nil {
				if len(fake.Calls()) != 0 || len(g.Ledger.(*ledger).ops) != 0 {
					t.Fatal("denied call reached adapter or ledger")
				}
			} else if op.Status != store.OpSucceeded || op.Target != tc.recorded || len(fake.Calls()) != 1 {
				t.Fatalf("op=%+v calls=%v", op, fake.Calls())
			}
		})
	}
}

func TestUnknownAdapterTargetFailsClosed(t *testing.T) {
	for _, targets := range [][]string{{"allowed"}, {"*"}, nil} {
		tr := &fakeconn.Tracker{}
		g, seat := setup(t, tr, "lead")
		grantTargets(seat, "project.create", targets)
		op, err := invoke(g, seat, "project.create", `{"target":"allowed"}`)
		if len(targets) > 0 {
			if !errors.Is(err, gateway.ErrForbidden) || len(tr.Calls()) != 0 {
				t.Fatalf("unverifiable target allowed: %v", err)
			}
		} else if err != nil || op.Status != store.OpSucceeded || op.Target != "" {
			t.Fatalf("legacy unscoped tracker: %+v %v", op, err)
		}
	}
}

func TestRefreshCannotChangeExecutionTarget(t *testing.T) {
	resolver := func(team string) connectors.TargetResolver {
		tr, err := linear.New(connectors.Config{Secret: map[string]string{"api_key": "fixture"}, Extra: map[string]string{"team_id": team}})
		if err != nil {
			t.Fatal(err)
		}
		return tr.(connectors.TargetResolver)
	}
	revoked := &boundTracker{Tracker: &fakeconn.Tracker{Script: []error{connectors.ErrUnauthorized}}, resolver: resolver("team-one")}
	rotated := &boundTracker{Tracker: &fakeconn.Tracker{}, resolver: resolver("team-two")}
	g, seat := setup(t, revoked.Tracker, "lead")
	g.Trackers = &rotatingTrackers{cur: revoked, next: rotated}
	grantTargets(seat, "project.create", []string{"team-*"})
	op, err := invoke(g, seat, "project.create", `{"name":"New"}`)
	if err != nil || op.Status != store.OpFailed || !strings.Contains(op.Error, "targets changed") || len(rotated.Calls()) != 0 {
		t.Fatalf("op=%+v err=%v calls=%v", op, err, rotated.Calls())
	}
}

type capturingTracker struct {
	*fakeconn.Tracker
	params json.RawMessage
}

func (t *capturingTracker) Invoke(ctx context.Context, op string, p json.RawMessage, id string) (connectors.Result, error) {
	t.params = append(json.RawMessage(nil), p...)
	return t.Tracker.Invoke(ctx, op, p, id)
}

func TestCanonicalParamsPreserveExecutionInteger(t *testing.T) {
	fake := &fakeconn.Tracker{}
	tr := &capturingTracker{Tracker: fake}
	g, seat := setup(t, fake, "lead")
	g.Trackers = trackers{tr}
	_, err := invoke(g, seat, "project.create", `{"number":9007199254740993}`)
	if err != nil || !strings.Contains(string(tr.params), "9007199254740993") {
		t.Fatalf("params=%s err=%v", tr.params, err)
	}
}

func TestCanonicalParamsKeepNestedKeyOrderIdempotent(t *testing.T) {
	tr := &fakeconn.Tracker{}
	g, seat := setup(t, tr, "lead")
	first, err := invoke(g, seat, "project.create", `{"nested":{"b":2,"a":1}}`)
	if err != nil {
		t.Fatal(err)
	}
	second, err := invoke(g, seat, "project.create", `{"nested":{"a":1,"b":2}}`)
	if err != nil || second.ID != first.ID || len(tr.Calls()) != 1 {
		t.Fatalf("duplicate effect: first=%+v second=%+v err=%v", first, second, err)
	}
}
