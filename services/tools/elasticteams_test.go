package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/darcys22/steadmesh/pkg/runtimeapi"
	"strings"
	"testing"

	"github.com/darcys22/steadmesh/pkg/spec"
	"github.com/darcys22/steadmesh/services/internal/orgfixture"
	"github.com/darcys22/steadmesh/services/metrics"
	"github.com/darcys22/steadmesh/services/store"
)

func TestElasticToolsSelectedSeatsAndFence(t *testing.T) {
	r := New(Deps{Metrics: metrics.NewUnregistered()})
	m := orgfixture.Manifest(t)
	c := &Call{Seat: &store.Seat{Manifest: m.Seats["engineer"]}, Org: &store.Organization{Manifest: *m}, Args: json.RawMessage(`{}`)}
	if elasticAllowed(&c.Seat.Manifest, &c.Org.Manifest) {
		t.Fatal("enabled without policy")
	}
	c.Org.Manifest.Spec.ElasticTeams = &spec.ElasticTeams{EnabledSeats: []string{"lead"}}
	if elasticAllowed(&c.Seat.Manifest, &c.Org.Manifest) {
		t.Fatal("unselected seat enabled")
	}
	c.Org.Manifest.Spec.ElasticTeams.EnabledSeats = []string{"engineer"}
	if !elasticAllowed(&c.Seat.Manifest, &c.Org.Manifest) {
		t.Fatal("selected seat unavailable")
	}
	for _, name := range []string{"teams.expand", "teams.cancel"} {
		if _, err := r.Call(context.Background(), name, c); !errors.Is(err, ErrFenced) {
			t.Errorf("%s: %v", name, err)
		}
	}
	for _, tool := range r.elasticTeamTools() {
		if !json.Valid([]byte(tool.schema)) {
			t.Fatalf("invalid schema %s", tool.name)
		}
	}
}

func TestElasticTemplatesBoundedAndPaginated(t *testing.T) {
	r := New(Deps{Metrics: metrics.NewUnregistered()})
	m := orgfixture.Manifest(t)
	p := &spec.ElasticTeams{EnabledSeats: []string{"engineer"}, Templates: map[string]spec.WorkerTemplate{}}
	for i := 0; i < 70; i++ {
		p.Templates[fmt.Sprintf("worker%02d", i)] = spec.WorkerTemplate{Instructions: strings.Repeat("<", 8192)}
	}
	m.Spec.ElasticTeams = p
	c := &Call{Seat: &store.Seat{Manifest: m.Seats["engineer"]}, Org: &store.Organization{Manifest: *m}, Args: json.RawMessage(`{}`)}
	for _, args := range []string{`{}`, `{"after":"worker63"}`, `{"template":"worker00"}`} {
		c.Args = json.RawMessage(args)
		got, err := r.Call(context.Background(), "teams.templates", c)
		if err != nil || got.IsError || len(got.Content) > MaxResultBytes {
			t.Fatalf("%s: %s %v", args, got.Content, err)
		}
	}
	team := &runtimeapi.ElasticTeam{ID: strings.Repeat("a", 36), RequestID: strings.Repeat("b", 36), ParentExecutionID: strings.Repeat("c", 36)}
	for i := 0; i < 32; i++ {
		team.Workers = append(team.Workers, runtimeapi.ElasticTeamWorker{Name: strings.Repeat("a", 80), Template: strings.Repeat("b", 80), Run: runtimeapi.TaskRun{ID: strings.Repeat("c", 36), State: runtimeapi.TaskPending}})
	}
	got, err := r.result(elasticSummary(team), nil)
	if err != nil || got.IsError {
		t.Fatalf("team summary unbounded: %s %v", got.Content, err)
	}
}
