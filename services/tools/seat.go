package tools

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/darcys22/steadmesh/pkg/compile"
	"github.com/darcys22/steadmesh/pkg/runtimeapi"
	"github.com/darcys22/steadmesh/services/connections"
	"github.com/darcys22/steadmesh/services/gateway"
	"github.com/darcys22/steadmesh/services/policy"
	"github.com/darcys22/steadmesh/services/scheduler"
	"github.com/darcys22/steadmesh/services/store"
)

// maxStatusSeats bounds the status tool's answer.
const maxStatusSeats = 20

func (r *Registry) selfTools() []*tool {
	return []*tool{
		{name: "self", description: "Describe this seat: identity, instruction sources, memory stores, recipients and connections.",
			schema: `{"type":"object","properties":{},"additionalProperties":false}`, allowed: always,
			handle: func(_ context.Context, c *Call) (any, error) { return Self(c.Seat, c.Org), nil }},
		{name: "status", description: "Report the technical state (running, stopped, queued work) of this seat and the seats it communicates with. Use it to explain real delays.",
			schema:  `{"type":"object","properties":{"seats":{"type":"array","items":{"type":"string"},"maxItems":20}},"additionalProperties":false}`,
			allowed: always, handle: r.status},
		{name: "handoff.update", description: "Replace this seat's portable handoff: current objective, unresolved questions, and relevant record, message and operation ids. It is restored after restarts.",
			schema:   `{"type":"object","properties":{"objective":{"type":"string"},"unresolved":{"type":"array","items":{"type":"string"}},"record_ids":{"type":"array","items":{"type":"string"}},"pending_message_ids":{"type":"array","items":{"type":"string"}},"operation_ids":{"type":"array","items":{"type":"string"}},"notes":{"type":"string"}},"additionalProperties":false}`,
			mutating: true, allowed: always, handle: r.handoff},
	}
}

func adapterKind(adapter string) string { return compile.DefaultCatalog().Connectors[adapter].Kind }

// Self describes the seat from its committed manifest.
func Self(seat *store.Seat, org *store.Organization) runtimeapi.Self {
	sm := &seat.Manifest
	out := runtimeapi.Self{
		OrganizationID: seat.OrganizationID, OrganizationKey: org.Key, OrganizationName: org.Manifest.Spec.DisplayName,
		SeatID: seat.ID, SeatKey: seat.Key, DisplayName: sm.DisplayName, Teams: sm.Teams, IsRepresentative: sm.IsRepresentative,
		ConfigRevision: seat.ConfigRevision, PolicyRevision: seat.PolicyRevision, ChannelBindings: sm.ChannelBindings,
		Instructions: []runtimeapi.InstructionInfo{}, MemoryStores: []runtimeapi.MemoryStoreInfo{},
		Recipients: []runtimeapi.RecipientInfo{}, Connections: []runtimeapi.ConnectionInfo{},
	}
	for _, i := range sm.Instructions {
		out.Instructions = append(out.Instructions, runtimeapi.InstructionInfo{Ref: i.Ref, Scope: i.Scope, Order: i.Order})
	}
	for _, c := range sm.Capabilities {
		if key, ok := strings.CutPrefix(c.Resource, "memory:"); ok {
			out.MemoryStores = append(out.MemoryStores, runtimeapi.MemoryStoreInfo{Key: key, Personal: key == sm.PersonalMemory, Operations: c.Operations})
		}
	}
	for _, e := range sm.SendTo {
		out.Recipients = append(out.Recipients, runtimeapi.RecipientInfo{Seat: e.Seat, DisplayName: displayName(&org.Manifest, e.Seat), CanReply: e.Reply})
	}
	conns := policy.Connections(sm)
	keys := make([]string, 0, len(conns))
	for k := range conns {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out.Connections = append(out.Connections, runtimeapi.ConnectionInfo{Key: k, Adapter: org.Manifest.Spec.Connections[k].Adapter, Operations: conns[k].Operations})
	}
	return out
}

type seatStatus struct {
	Seat             string     `json:"seat"`
	State            string     `json:"state"`
	Detail           string     `json:"detail,omitempty"`
	Running          bool       `json:"running"`
	Pending          int        `json:"pending_messages"`
	OldestPendingAge float64    `json:"oldest_pending_age_seconds,omitempty"`
	Dead             int        `json:"dead_letters,omitempty"`
	LastActivity     *time.Time `json:"last_activity,omitempty"`
	OnCurrentConfig  bool       `json:"on_current_config"`
}

func (r *Registry) status(ctx context.Context, c *Call) (any, error) {
	var a struct {
		Seats []string `json:"seats"`
	}
	if err := decode(c, &a); err != nil {
		return nil, err
	}
	reachable := policy.Reachable(&c.Seat.Manifest)
	keys := []string{c.Seat.Key}
	var unknown []string
	if len(a.Seats) == 0 {
		a.Seats = reachable
	}
	for _, k := range a.Seats {
		switch {
		case k == c.Seat.Key:
		case slices.Contains(reachable, k) && len(keys) < maxStatusSeats:
			keys = append(keys, k)
		default:
			unknown = append(unknown, k)
		}
	}
	rt, err := r.d.Store.SeatRuntimes(ctx, c.Seat.OrganizationID, keys)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	out := map[string]any{}
	var others []seatStatus
	for _, k := range keys {
		s, ok := rt[k]
		if !ok {
			unknown = append(unknown, k)
			continue
		}
		st := seatStatus{Seat: k, State: s.State, Detail: s.StateDetail, Running: s.LeaseExpiresAt.After(now),
			Pending: s.PendingDeliveries, OldestPendingAge: s.OldestPendingAge, Dead: s.DeadDeliveries, LastActivity: s.LastActivity,
			OnCurrentConfig: s.AdoptedRevision != "" && s.AdoptedRevision == c.Org.Manifest.Seats[k].ConfigRevision}
		if k == c.Seat.Key {
			out["self"] = st
		} else {
			others = append(others, st)
		}
	}
	out["seats"] = nonNilSlice(others)
	if len(unknown) > 0 {
		out["not_reachable"] = unknown
	}
	return out, nil
}

func (r *Registry) handoff(ctx context.Context, c *Call) (any, error) {
	var h runtimeapi.Handoff
	if err := decode(c, &h); err != nil {
		return nil, err
	}
	if err := r.d.Store.SaveHandoff(ctx, c.fence(), h); err != nil {
		return nil, err
	}
	return map[string]any{"saved": true}, nil
}

func (r *Registry) wakeTools() []*tool {
	return []*tool{
		{name: "wake.schedule", description: "Schedule a durable future wake of this seat: once (at, RFC 3339) or recurring (every, e.g. \"24h\", at least 1m). The note is delivered with the wake.",
			schema:   `{"type":"object","properties":{"at":{"type":"string","format":"date-time"},"every":{"type":"string"},"note":{"type":"string","maxLength":2048}},"additionalProperties":false}`,
			mutating: true, allowed: always, handle: r.wakeSchedule},
		{name: "wake.list", description: "List this seat's active wake schedules.",
			schema: `{"type":"object","properties":{},"additionalProperties":false}`, allowed: always,
			handle: func(ctx context.Context, c *Call) (any, error) {
				s, err := r.d.Store.Schedules(ctx, c.Seat.ID)
				return map[string]any{"schedules": nonNilSlice(s)}, err
			}},
		{name: "wake.cancel", description: "Cancel one of this seat's wake schedules.",
			schema:   `{"type":"object","properties":{"id":{"type":"string"}},"required":["id"],"additionalProperties":false}`,
			mutating: true, allowed: always, handle: r.wakeCancel},
	}
}

func (r *Registry) wakeSchedule(ctx context.Context, c *Call) (any, error) {
	var a struct {
		At    string `json:"at"`
		Every string `json:"every"`
		Note  string `json:"note"`
	}
	if err := decode(c, &a); err != nil {
		return nil, err
	}
	if len(a.Note) > 2048 {
		return nil, toolErr("invalid", "note exceeds 2048 bytes")
	}
	sched, next, err := scheduler.Parse(a.At, a.Every, time.Now())
	if err != nil {
		return nil, toolErr("invalid", "%s", err.Error())
	}
	return r.d.Store.CreateSchedule(ctx, c.fence(), c.Seat.OrganizationID, sched, next, a.Note)
}

func (r *Registry) wakeCancel(ctx context.Context, c *Call) (any, error) {
	var a struct {
		ID string `json:"id"`
	}
	if err := decode(c, &a); err != nil {
		return nil, err
	}
	if err := r.d.Store.CancelSchedule(ctx, c.fence(), a.ID); err != nil {
		return nil, err
	}
	return map[string]any{"cancelled": a.ID}, nil
}

func hasTracker(sm *compile.SeatManifest, org *compile.Manifest) bool {
	for k := range policy.Connections(sm) {
		if adapterKind(org.Spec.Connections[k].Adapter) == "tracker" {
			return true
		}
	}
	return false
}

func (r *Registry) connectionTools() []*tool {
	return []*tool{
		{name: "connections.list", description: "List the external connections this seat may use and the granted operations and targets.",
			schema:  `{"type":"object","properties":{},"additionalProperties":false}`,
			allowed: func(sm *compile.SeatManifest, _ *compile.Manifest) bool { return len(policy.Connections(sm)) > 0 },
			handle:  r.connectionsList},
		{name: "connections.invoke", description: "Invoke a granted operation on a work-tracker connection. Each call is recorded in the operation ledger; reusing an idempotency_key returns the recorded operation instead of repeating it. An unknown status means the outcome could not be confirmed: check the tracker before retrying.",
			schema:   `{"type":"object","properties":{"connection":{"type":"string"},"operation":{"type":"string"},"params":{"type":"object"},"idempotency_key":{"type":"string","maxLength":200}},"required":["connection","operation"],"additionalProperties":false}`,
			mutating: true, allowed: hasTracker, handle: r.invoke},
		{name: "operations.get", description: "Get a recorded connector operation of this seat by id.",
			schema:  `{"type":"object","properties":{"id":{"type":"string"}},"required":["id"],"additionalProperties":false}`,
			allowed: hasTracker, handle: r.operation},
	}
}

func (r *Registry) connectionsList(_ context.Context, c *Call) (any, error) {
	type info struct {
		Key        string   `json:"key"`
		Adapter    string   `json:"adapter"`
		Kind       string   `json:"kind"`
		Operations []string `json:"operations"`
		Targets    []string `json:"targets,omitempty"`
		Invocable  bool     `json:"invocable"`
	}
	conns := policy.Connections(&c.Seat.Manifest)
	out := []info{}
	for k, cp := range conns {
		adapter := c.Org.Manifest.Spec.Connections[k].Adapter
		kind := adapterKind(adapter)
		out = append(out, info{Key: k, Adapter: adapter, Kind: kind, Operations: cp.Operations, Targets: cp.Targets, Invocable: kind == "tracker"})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return map[string]any{"connections": out}, nil
}

func (r *Registry) invoke(ctx context.Context, c *Call) (any, error) {
	var a struct {
		Connection     string          `json:"connection"`
		Operation      string          `json:"operation"`
		Params         json.RawMessage `json:"params"`
		IdempotencyKey string          `json:"idempotency_key"`
	}
	if err := decode(c, &a); err != nil {
		return nil, err
	}
	if len(a.IdempotencyKey) > 200 {
		return nil, toolErr("invalid", "idempotency_key exceeds 200 bytes")
	}
	op, err := r.d.Gateway.Invoke(ctx, gateway.Request{Seat: c.Seat, Generation: c.Generation, ExecutionID: c.ExecutionID,
		Connection: a.Connection, Operation: a.Operation, Params: a.Params, IdempotencyKey: a.IdempotencyKey})
	if errors.Is(err, connections.ErrNotConfigured) {
		return nil, toolErr("unavailable", "%s", err.Error())
	}
	if err != nil {
		return nil, err
	}
	return bounded(op), nil
}

// bounded omits an oversized connector result; the receipt identifies the
// external record.
func bounded(op *runtimeapi.Operation) *runtimeapi.Operation {
	if len(op.Result) > MaxResultBytes/2 {
		op.Result = json.RawMessage(`{"omitted":"result exceeds the tool result bound; use the receipt"}`)
	}
	return op
}

func (r *Registry) operation(ctx context.Context, c *Call) (any, error) {
	var a struct {
		ID string `json:"id"`
	}
	if err := decode(c, &a); err != nil {
		return nil, err
	}
	op, err := r.d.Store.Operation(ctx, c.Seat.OrganizationID, c.Seat.ID, a.ID)
	if err != nil {
		return nil, err
	}
	return bounded(op), nil
}
