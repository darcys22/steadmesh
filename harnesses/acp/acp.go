// Package acp is a deliberately disabled ACP v1 compatibility spike.
// It is not registered as a selectable harness. See CONTRACT.md for the
// target-specific conformance evidence required before execution is safe.
package acp

import (
	"context"
	"errors"
	"sync"

	"github.com/darcys22/steadmesh/harnesses"
	"github.com/darcys22/steadmesh/pkg/runtimeapi"
)

// ErrUnverified prevents an unverified agent from executing with seat authority.
var ErrUnverified = errors.New("acp: execution disabled; model routing, credential isolation, permissions, tools, skills and recovery require target-specific conformance")

// Adapter implements the lifecycle contract without launching a process,
// retaining an Environment, writing credentials or claiming recovery support.
// There is intentionally no configuration switch that enables execution.
// Construct with New; Stop is safe concurrently and is idempotent.
type Adapter struct {
	events chan harnesses.Event
	stop   sync.Once
}

func New() *Adapter { return &Adapter{events: make(chan harnesses.Event)} }

var _ harnesses.Adapter = (*Adapter)(nil)

func (*Adapter) DescribeCapabilities() harnesses.Capabilities {
	return harnesses.Capabilities{Adapter: "acp-experimental", Interruption: "none"}
}

func (*Adapter) Prepare(context.Context, harnesses.Environment) error { return ErrUnverified }
func (*Adapter) StartOrResume(context.Context, harnesses.RecoveryDescriptor) (harnesses.ResumeResult, error) {
	return harnesses.ResumeResult{}, ErrUnverified
}
func (*Adapter) Deliver(context.Context, harnesses.Delivery) (harnesses.TurnResult, error) {
	return harnesses.TurnResult{Status: harnesses.TurnFailed}, ErrUnverified
}
func (a *Adapter) Events() <-chan harnesses.Event { return a.events }
func (*Adapter) Quiesce(context.Context) (harnesses.QuiesceResult, error) {
	return harnesses.QuiesceResult{}, ErrUnverified
}
func (*Adapter) Checkpoint(context.Context) (runtimeapi.Checkpoint, error) {
	return runtimeapi.Checkpoint{}, ErrUnverified
}
func (a *Adapter) Stop(context.Context) error {
	a.stop.Do(func() { close(a.events) })
	return nil
}
