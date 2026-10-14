package store

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/darcys22/steadmesh/pkg/compile"
	"github.com/darcys22/steadmesh/pkg/runtimeapi"
)

func TestSeatConfigPreservesCapabilityTargets(t *testing.T) {
	sm := compile.SeatManifest{Capabilities: []compile.Capability{
		{Resource: "connection:github", Operations: []string{"issue.read"}, Targets: []string{"acme/a"}, Sources: []string{"grant:read_a"}},
		{Resource: "connection:github", Operations: []string{"issue.comment"}, Targets: []string{"acme/b"}, Sources: []string{"grant:comment_b"}},
		{Resource: "connection:github", Operations: []string{"repo.read"}, Sources: []string{"grant:all_repos"}},
	}}
	got := seatConfig("org", "seat", sm, 1, time.Time{}, "")
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var decoded runtimeapi.ConsoleSeatConfig
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatal(err)
	}
	want := []runtimeapi.ConsoleCapability{
		{Resource: "connection:github", Operations: []string{"issue.read"}, Targets: []string{"acme/a"}, Sources: []string{"grant:read_a"}},
		{Resource: "connection:github", Operations: []string{"issue.comment"}, Targets: []string{"acme/b"}, Sources: []string{"grant:comment_b"}},
		{Resource: "connection:github", Operations: []string{"repo.read"}, Sources: []string{"grant:all_repos"}},
	}
	if !reflect.DeepEqual(decoded.Capabilities, want) {
		t.Fatalf("console lost clause targets: %s", b)
	}
}
