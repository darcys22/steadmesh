//go:build integration

package store

import (
	"context"
	"errors"
	"github.com/darcys22/steadmesh/pkg/runtimeapi"
	"github.com/darcys22/steadmesh/services/internal/orgfixture"
	"testing"
)

func TestCheckpointRoutePersistsAndIsFenced(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	synced := syncOrg(t, s, orgfixture.Manifest(t))
	id := synced.Seats["lead"].SeatID
	f := lease(t, s, id)
	cp := runtimeapi.Checkpoint{HarnessAdapter: "pi", FormatVersion: "test", CheckpointRef: "native", Guarantee: "application_checkpoint", HarnessProfile: "approved-secondary", RouteBinding: "binding"}
	if err := s.SaveCheckpoint(ctx, f, cp); err != nil {
		t.Fatal(err)
	}
	got, err := s.Checkpoints(ctx, id)
	if err != nil || len(got) != 1 || got[0] != cp {
		t.Fatalf("checkpoint did not round trip: %+v %v", got, err)
	}
	stale := f
	stale.Generation++
	cp.RouteBinding = "unauthorized"
	if err := s.SaveCheckpoint(ctx, stale, cp); !errors.Is(err, ErrFenced) {
		t.Fatalf("stale route write: %v", err)
	}
	got, err = s.Checkpoints(ctx, id)
	if err != nil || got[0].RouteBinding != "binding" {
		t.Fatalf("fenced write modified route: %+v %v", got, err)
	}
}
