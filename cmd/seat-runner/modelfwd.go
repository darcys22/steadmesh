package main

import (
	"encoding/json"

	"github.com/darcys22/steadmesh/harnesses"
)

// probeTrace is what a readiness probe turn did, observed by the runner.
type probeTrace struct {
	modelStatuses []int
	toolCalls     map[string]string // correlation id -> tool name
	toolErrors    map[string]bool   // correlation id -> result was an error
}

func (r *Runner) currentExecution() string {
	id, _ := r.execution.Load().(string)
	return id
}

// trace starts recording a probe execution; the returned func stops it.
func (r *Runner) trace(executionID string) (*probeTrace, func()) {
	t := &probeTrace{toolCalls: map[string]string{}, toolErrors: map[string]bool{}}
	r.traceMu.Lock()
	if r.traces == nil {
		r.traces = map[string]*probeTrace{}
	}
	r.traces[executionID] = t
	r.traceMu.Unlock()
	return t, func() {
		r.traceMu.Lock()
		delete(r.traces, executionID)
		r.traceMu.Unlock()
	}
}

func (r *Runner) observeModel(executionID string, status int) {
	r.traceMu.Lock()
	defer r.traceMu.Unlock()
	if t := r.traces[executionID]; t != nil {
		t.modelStatuses = append(t.modelStatuses, status)
	}
}

func (r *Runner) observeEvent(e harnesses.Event) {
	if e.Kind != harnesses.EventToolRequest && e.Kind != harnesses.EventToolResult {
		return
	}
	r.traceMu.Lock()
	defer r.traceMu.Unlock()
	t := r.traces[e.ExecutionID]
	if t == nil {
		return
	}
	var d struct {
		Name    string `json:"name"`
		IsError bool   `json:"is_error"`
	}
	_ = json.Unmarshal(e.Data, &d)
	if e.Kind == harnesses.EventToolRequest {
		t.toolCalls[e.CorrelationID] = d.Name
	} else if d.IsError {
		t.toolErrors[e.CorrelationID] = true
	}
}

// snapshot copies the trace under the lock.
func (r *Runner) snapshot(t *probeTrace) probeTrace {
	r.traceMu.Lock()
	defer r.traceMu.Unlock()
	c := probeTrace{modelStatuses: append([]int(nil), t.modelStatuses...), toolCalls: map[string]string{}, toolErrors: map[string]bool{}}
	for k, v := range t.toolCalls {
		c.toolCalls[k] = v
	}
	for k, v := range t.toolErrors {
		c.toolErrors[k] = v
	}
	return c
}
