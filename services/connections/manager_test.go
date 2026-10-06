package connections_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/darcys22/steadmesh/connectors"
	"github.com/darcys22/steadmesh/pkg/spec"
	"github.com/darcys22/steadmesh/services/connections"
	"github.com/darcys22/steadmesh/services/internal/fakeconn"
	"github.com/darcys22/steadmesh/services/internal/orgfixture"
)

type nopSink struct{}

func (nopSink) Accept(context.Context, string, connectors.InboundEvent) error { return nil }

func TestSyncVerifyAndRebuild(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	comm := fakeconn.NewComm(orgfixture.UserA)
	builds := 0
	secrets := &fakeconn.Secrets{Err: errors.New("vault sealed")}
	m := connections.New(ctx, connections.Factories{
		Communication: map[string]func(connectors.Config) (connectors.Communication, error){"slack": comm.Factory()},
		Tracker: map[string]func(connectors.Config) (connectors.Tracker, error){"linear": func(cfg connectors.Config) (connectors.Tracker, error) {
			builds++
			if cfg.Secret["api_key"] != "test" {
				t.Errorf("secret not injected: %v", cfg.Secret)
			}
			return &fakeconn.Tracker{}, nil
		}},
	}, secrets, func(string) connectors.IngressSink { return nopSink{} }, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer m.Close()

	manifest := orgfixture.Manifest(t)
	v := m.Verify(ctx, "org", manifest)
	if v.Connections["tracker"].OK || !strings.Contains(v.Connections["tracker"].Detail, "vault sealed") || v.Bindings["alice"].OK {
		t.Fatalf("verify with sealed secrets = %+v", v)
	}
	// Verify retries failed builds, so a recovered dependency is picked up (A25).
	secrets.Err = nil
	deadline := time.Now().Add(2 * time.Second)
	for ok, _ := comm.Healthy(); !ok && time.Now().Before(deadline); ok, _ = comm.Healthy() {
		m.Sync(ctx, "org", manifest.Spec.Connections)
		time.Sleep(10 * time.Millisecond)
	}
	v = m.Verify(ctx, "org", manifest)
	if !v.Connections["tracker"].OK || !v.Connections["slack"].OK || !v.Ingress["slack"].OK || !v.Bindings["alice"].OK || v.Bindings["bob"].OK {
		t.Fatalf("verify = %+v", v)
	}
	if _, err := m.Tracker("org", "slack"); !errors.Is(err, connections.ErrNotConfigured) {
		t.Fatalf("slack as tracker: %v", err)
	}
	before := builds
	m.Sync(ctx, "org", manifest.Spec.Connections)
	if builds != before {
		t.Fatal("unchanged connection rebuilt")
	}
	conns := map[string]spec.Connection{}
	for k, c := range manifest.Spec.Connections {
		conns[k] = c
	}
	tr := conns["tracker"]
	tr.Config = map[string]string{"team_id": "T2"}
	conns["tracker"] = tr
	m.Sync(ctx, "org", conns)
	if builds != before+1 {
		t.Fatal("changed connection not rebuilt")
	}
	conns["other"] = spec.Connection{Adapter: "jira"}
	m.Sync(ctx, "org", conns)
	if _, err := m.Tracker("org", "other"); err == nil || !strings.Contains(err.Error(), "jira") {
		t.Fatalf("unknown adapter: %v", err)
	}
	m.Remove("org")
	if _, err := m.Tracker("org", "tracker"); !errors.Is(err, connections.ErrNotConfigured) {
		t.Fatalf("removed org still has adapters: %v", err)
	}
}
