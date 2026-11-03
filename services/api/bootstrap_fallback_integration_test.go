//go:build integration

package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/darcys22/steadmesh/pkg/compile"
	"github.com/darcys22/steadmesh/pkg/runtimeapi"
	"github.com/darcys22/steadmesh/pkg/spec"
	"github.com/darcys22/steadmesh/services/auth"
	"github.com/darcys22/steadmesh/services/internal/orgfixture"
	"github.com/darcys22/steadmesh/services/internal/pgtest"
	"github.com/darcys22/steadmesh/services/metrics"
	"github.com/darcys22/steadmesh/services/store"
	"github.com/darcys22/steadmesh/services/tools"
	"github.com/prometheus/client_golang/prometheus"
)

func TestBootstrapSecondaryCheckpointAPIRoundTripAndRevocation(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, pgtest.URL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	specification := orgfixture.Spec()
	image := "example/seat-bundle@sha256:" + strings.Repeat("a", 64)
	specification.HarnessProfiles["primary"] = spec.HarnessProfile{Adapter: "fake", ImageDigest: image, StartupFallback: &spec.StartupFallbackPolicy{Profiles: []string{"secondary"}, MaxAttempts: 2, TotalTimeout: "30s"}}
	specification.HarnessProfiles["secondary"] = spec.HarnessProfile{Adapter: "pi", ImageDigest: image, Model: &spec.ModelSelection{Connection: "model", ID: "secondary-model"}}
	specification.Connections["model"] = spec.Connection{Adapter: "openai", SecretRef: "k8s:model"}
	lead := specification.Seats["lead"]
	lead.HarnessProfile = "primary"
	specification.Seats["lead"] = lead
	sync := func() (*compile.Manifest, *runtimeapi.SyncResponse) {
		t.Helper()
		m := orgfixture.Compile(t, specification)
		response, err := db.SyncOrganization(ctx, store.SyncInput{Namespace: "acme", Key: "acme", SourceUID: "uid", Manifest: m})
		if err != nil {
			t.Fatal(err)
		}
		return m, response
	}
	manifest, synced := sync()
	id := synced.Seats["lead"].SeatID
	lease, err := db.AcquireLease(ctx, id, "pod")
	if err != nil {
		t.Fatal(err)
	}
	authn := auth.NewFake("controller")
	authn.AddSeat("seat-token", auth.Seat{SeatID: id, PodUID: "pod"})
	registry := prometheus.NewRegistry()
	met := metrics.New(registry)
	h := New(Config{Store: db, Auth: authn, Tools: tools.New(tools.Deps{Store: db, Metrics: met}), Metrics: met, Gatherer: registry, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	request := func(method, path string, body any) *httptest.ResponseRecorder {
		t.Helper()
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest(method, path, strings.NewReader(string(data)))
		r.Header.Set("Authorization", "Bearer seat-token")
		r.Header.Set(runtimeapi.HeaderGeneration, strconv.FormatInt(lease.Generation, 10))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	seat := manifest.Seats["lead"]
	primary := runtimeapi.Checkpoint{HarnessAdapter: seat.Harness.Adapter, HarnessProfile: seat.HarnessProfile, RouteBinding: compile.StartupCandidateBinding(seat.HarnessProfile, seat.Harness), FormatVersion: "test", CheckpointRef: "older-primary", Guarantee: "application_checkpoint"}
	candidate := seat.StartupFallbacks[0]
	secondary := runtimeapi.Checkpoint{HarnessAdapter: candidate.Harness.Adapter, HarnessProfile: candidate.Profile, RouteBinding: compile.StartupCandidateBinding(candidate.Profile, candidate.Harness), FormatVersion: "test", CheckpointRef: "active-secondary", Guarantee: "application_checkpoint"}
	for _, cp := range []runtimeapi.Checkpoint{primary, secondary} {
		if w := request(http.MethodPut, runtimeapi.PathCheckpoint, cp); w.Code != http.StatusNoContent {
			t.Fatalf("save checkpoint: %d %s", w.Code, w.Body.String())
		}
	}
	assertRecovery := func() {
		t.Helper()
		w := request(http.MethodGet, runtimeapi.PathBootstrap, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("bootstrap: %d %s", w.Code, w.Body.String())
		}
		var boot runtimeapi.Bootstrap
		if err := json.Unmarshal(w.Body.Bytes(), &boot); err != nil {
			t.Fatal(err)
		}
		if boot.Recovery.Session == nil || *boot.Recovery.Session != secondary {
			t.Fatalf("secondary route did not survive bootstrap: %+v", boot.Recovery.Session)
		}
	}
	assertRecovery()
	// A changed model binding must not erase recovery state, which the runner
	// needs in order to refuse starting either a different session or primary.
	specification.HarnessProfiles["secondary"].Model.ID = "changed-model"
	sync()
	assertRecovery()
	// Removing the fallback policy must not revert recovery to the older primary.
	profile := specification.HarnessProfiles["primary"]
	profile.StartupFallback = nil
	specification.HarnessProfiles["primary"] = profile
	sync()
	assertRecovery()
	if w := request(http.MethodPut, runtimeapi.PathCheckpoint, secondary); w.Code != http.StatusForbidden {
		t.Fatalf("revoked route checkpoint accepted: %d %s", w.Code, w.Body.String())
	}
}
