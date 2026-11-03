package api

import (
	"testing"

	"github.com/darcys22/steadmesh/harnesses"
	"github.com/darcys22/steadmesh/pkg/compile"
	"github.com/darcys22/steadmesh/pkg/spec"
	"github.com/darcys22/steadmesh/pkg/taskworker"
	"github.com/darcys22/steadmesh/services/policy"
	"github.com/darcys22/steadmesh/services/store"
)

func fallbackProxyManifest() compile.SeatManifest {
	return compile.SeatManifest{
		HarnessProfile: "primary",
		Harness:        spec.HarnessProfile{Model: &spec.ModelSelection{Connection: "primary", ID: "claude", API: harnesses.APIAnthropicMessages}, StartupFallback: &spec.StartupFallbackPolicy{Profiles: []string{"secondary", "third", "same_connection"}, MaxAttempts: 4, TotalTimeout: "30s"}},
		StartupFallbacks: []compile.StartupFallbackCandidate{
			{Profile: "secondary", Harness: spec.HarnessProfile{Model: &spec.ModelSelection{Connection: "secondary", ID: "response-model", API: harnesses.APIOpenAIResponses}}},
			{Profile: "third", Harness: spec.HarnessProfile{Model: &spec.ModelSelection{Connection: "secondary", ID: "chat-model", API: harnesses.APIOpenAIChat}}},
			{Profile: "same_connection", Harness: spec.HarnessProfile{Model: &spec.ModelSelection{Connection: "primary", ID: "other-claude", API: harnesses.APIAnthropicMessages}}},
		},
		Capabilities: []compile.Capability{
			{Resource: "connection:primary", Operations: []string{"model.infer"}},
			{Resource: "connection:secondary", Operations: []string{"model.infer"}},
			{Resource: "connection:explicit", Operations: []string{"model.infer"}},
		},
	}
}

// The proxy checks both connection permission and the exact tuple before any
// request reaches the upstream. Separate IDs/APIs must never form a product.
func TestStartupFallbackExactModelGrants(t *testing.T) {
	q := &seatReq{seat: &store.Seat{Manifest: fallbackProxyManifest()}}
	for _, tc := range []struct {
		name, connection, id, api string
		allowed                   bool
	}{
		{"primary", "primary", "claude", harnesses.APIAnthropicMessages, true},
		{"secondary", "secondary", "response-model", harnesses.APIOpenAIResponses, true},
		{"third", "secondary", "chat-model", harnesses.APIOpenAIChat, true},
		{"same primary connection", "primary", "other-claude", harnesses.APIAnthropicMessages, true},
		{"unknown connection", "unknown", "response-model", harnesses.APIOpenAIResponses, false},
		{"unknown model", "secondary", "unknown", harnesses.APIOpenAIResponses, false},
		{"wrong API", "secondary", "response-model", harnesses.APIAnthropicMessages, false},
		{"Cartesian model API", "secondary", "response-model", harnesses.APIOpenAIChat, false},
		{"Cartesian API model", "secondary", "chat-model", harnesses.APIOpenAIResponses, false},
		{"same model wrong connection", "primary", "response-model", harnesses.APIOpenAIResponses, false},
		{"primary model wrong connection", "secondary", "claude", harnesses.APIAnthropicMessages, false},
		{"empty model", "secondary", "", harnesses.APIOpenAIResponses, false},
		{"unrelated explicit grant", "explicit", "custom-model", harnesses.APIOpenAIChat, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			allowed := policy.Allows(&q.seat.Manifest, "connection:"+tc.connection, "model.infer") && modelAllowed(q, tc.connection, tc.api, tc.id) == ""
			if allowed != tc.allowed {
				t.Fatalf("allowed=%v want=%v", allowed, tc.allowed)
			}
		})
	}
}

func TestStartupFallbackCurrentManifestRevokesStaleTuple(t *testing.T) {
	stale := fallbackProxyManifest()
	stale.ConfigRevision = "old"
	current := fallbackProxyManifest()
	current.ConfigRevision = "current"
	// The candidate connection remains granted, but the committed model changes.
	current.StartupFallbacks[0].Harness.Model.ID = "replacement"
	q := &seatReq{seat: &store.Seat{Manifest: current, ConfigRevision: current.ConfigRevision}}
	old := stale.StartupFallbacks[0].Harness.Model
	if modelAllowed(q, old.Connection, old.API, old.ID) == "" {
		t.Fatal("stale pod tuple was accepted by current manifest")
	}
	if modelAllowed(q, old.Connection, old.API, "replacement") != "" {
		t.Fatal("current tuple was refused")
	}
	// Removing fallback also removes implicit permissions; a stale pod cannot
	// turn the old secondary connection into the explicit-grant path.
	current.StartupFallbacks, current.Harness.StartupFallback = nil, nil
	current.Capabilities = current.Capabilities[:1]
	q.seat.Manifest = current
	if policy.Allows(&q.seat.Manifest, "connection:"+old.Connection, "model.infer") {
		t.Fatal("revoked secondary connection remained granted")
	}
}

func TestTaskWorkerHasOnlyPrimaryModelTuple(t *testing.T) {
	q := &seatReq{seat: &store.Seat{Manifest: taskworker.Manifest(fallbackProxyManifest(), nil)}}
	if modelAllowed(q, "primary", harnesses.APIAnthropicMessages, "other-claude") == "" {
		t.Fatal("same-connection fallback model leaked into worker")
	}
	if policy.Allows(&q.seat.Manifest, "connection:secondary", "model.infer") {
		t.Fatal("fallback connection leaked into worker")
	}
	if policy.Allows(&q.seat.Manifest, "connection:explicit", "model.infer") {
		t.Fatal("unrelated explicit connection leaked into worker")
	}
	if modelAllowed(q, "primary", harnesses.APIAnthropicMessages, "claude") != "" {
		t.Fatal("worker lost primary tuple")
	}
}

func TestNoStartupFallbackPreservesPrimaryRestriction(t *testing.T) {
	m := fallbackProxyManifest()
	m.StartupFallbacks, m.Harness.StartupFallback = nil, nil
	q := &seatReq{seat: &store.Seat{Manifest: m}}
	if modelAllowed(q, "primary", harnesses.APIAnthropicMessages, "claude") != "" {
		t.Fatal("primary refused")
	}
	for _, id := range []string{"", "another"} {
		if modelAllowed(q, "primary", harnesses.APIAnthropicMessages, id) == "" {
			t.Fatal("primary restriction widened")
		}
	}
	if modelAllowed(q, "explicit", harnesses.APIOpenAIChat, "anything") != "" {
		t.Fatal("unrelated explicit grant narrowed")
	}
}
