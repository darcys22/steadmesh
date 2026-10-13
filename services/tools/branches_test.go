package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/darcys22/steadmesh/pkg/runtimeapi"
	"github.com/darcys22/steadmesh/services/internal/orgfixture"
	"github.com/darcys22/steadmesh/services/metrics"
	"github.com/darcys22/steadmesh/services/store"
)

func TestBranchMutationsRequireGeneration(t *testing.T) {
	r := New(Deps{Metrics: metrics.NewUnregistered()})
	manifest := orgfixture.Manifest(t)
	c := &Call{Seat: &store.Seat{Manifest: manifest.Seats["lead"]}, Org: &store.Organization{Manifest: *manifest}, Args: json.RawMessage(`{}`)}
	for _, name := range []string{"branches.start", "branches.select"} {
		if _, err := r.Call(context.Background(), name, c); !errors.Is(err, ErrFenced) {
			t.Errorf("%s missing generation: %v", name, err)
		}
	}
}

func TestBranchComparisonFitsToolResultEvenWithEscapedText(t *testing.T) {
	// JSON expands control bytes sixfold. Previews are byte-bounded before
	// serialization, so a worst-case four-way comparison still fits.
	out := runtimeapi.RunBranchComparison{ID: strings.Repeat("a", 36), RequestID: strings.Repeat("b", 36),
		ParentExecutionID: strings.Repeat("c", 36), SnapshotSHA256: strings.Repeat("d", 64), Revision: 1, CreatedAt: time.Now()}
	for range runtimeapi.MaxRunAlternatives {
		out.Alternatives = append(out.Alternatives, runtimeapi.RunAlternativeState{Name: strings.Repeat("\x01", 64), TaskRunID: strings.Repeat("e", 36),
			State: runtimeapi.TaskFailed, OutputPreview: strings.Repeat("\x01", 256), ErrorPreview: strings.Repeat("\x01", 128), ResultSHA256: strings.Repeat("f", 64)})
	}
	r := New(Deps{Metrics: metrics.NewUnregistered()})
	got, err := r.result(out, nil)
	if err != nil || got.IsError || len(got.Content) > MaxResultBytes {
		t.Fatalf("bounded comparison: bytes=%d error=%v %s", len(got.Content), err, got.Content)
	}
}
