package acp

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/darcys22/steadmesh/harnesses"
)

func TestAdapterCannotExecute(t *testing.T) {
	for _, name := range []string{"acp", "acp-experimental"} {
		if _, ok := harnesses.Lookup(name); ok {
			t.Fatalf("experimental adapter registered as %s", name)
		}
	}
	a := New()
	ctx := context.Background()
	env := harnesses.Environment{
		Model:         &harnesses.ModelEndpoint{ID: "model", BaseURL: "http://127.0.0.1:1234", APIKey: "placeholder"},
		HarnessConfig: map[string]string{"approve_all": "true", "enabled": "true"},
	}
	if !errors.Is(a.Prepare(ctx, env), ErrUnverified) {
		t.Fatal("Prepare must fail closed")
	}
	if _, err := a.StartOrResume(ctx, harnesses.RecoveryDescriptor{}); !errors.Is(err, ErrUnverified) {
		t.Fatal(err)
	}
	if r, err := a.Deliver(ctx, harnesses.Delivery{}); !errors.Is(err, ErrUnverified) || r.Status != harnesses.TurnFailed {
		t.Fatalf("%+v %v", r, err)
	}
	if r, err := a.Checkpoint(ctx); !errors.Is(err, ErrUnverified) || r.Guarantee != "" {
		t.Fatalf("%+v %v", r, err)
	}
	if _, err := a.Quiesce(ctx); !errors.Is(err, ErrUnverified) {
		t.Fatal(err)
	}
	c := a.DescribeCapabilities()
	if c.EventStreaming || c.GracefulSuspension || len(c.ToolTransports)+len(c.RecoveryModes)+len(c.CheckpointGuarantees) != 0 || c.Interruption != "none" {
		t.Fatalf("false capability claims: %+v", c)
	}
	if a.Events() != a.Events() {
		t.Fatal("unstable event channel")
	}
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _ = a.Stop(ctx) }()
	}
	wg.Wait()
	if _, ok := <-a.Events(); ok {
		t.Fatal("events must close")
	}
}
