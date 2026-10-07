// Package tools implements the seat tool surface (contracts.md "Tools").
// Every call is authorised server-side against the seat's committed policy
// revision, which is re-read for each call, so a sync that revokes a grant
// takes effect on the next call (A13). Mutating tools are fenced on the
// caller's lease generation inside the write transaction.
package tools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"

	"github.com/darcys22/steadmesh/pkg/compile"
	"github.com/darcys22/steadmesh/pkg/runtimeapi"
	"github.com/darcys22/steadmesh/services/gateway"
	"github.com/darcys22/steadmesh/services/metrics"
	"github.com/darcys22/steadmesh/services/store"
)

// MaxResultBytes bounds a tool result (ADR-0006).
const MaxResultBytes = 16 << 10

var (
	// ErrUnknownTool means no tool has the requested name.
	ErrUnknownTool = errors.New("unknown tool")
	// ErrFenced means a mutating call lacked the current lease generation.
	ErrFenced = store.ErrFenced
)

// Deps are the services tools use.
type Deps struct {
	Store   *store.Store
	Gateway *gateway.Gateway
	Metrics *metrics.Metrics
	Log     *slog.Logger
}

// Call is one authenticated tool invocation.
type Call struct {
	Seat *store.Seat
	Org  *store.Organization
	// Generation is the caller's lease generation; HasGeneration is false
	// when the header was absent.
	Generation    int64
	HasGeneration bool
	ExecutionID   string
	Args          json.RawMessage
}

func (c *Call) fence() store.Fence { return store.Fence{SeatID: c.Seat.ID, Generation: c.Generation} }

type tool struct {
	name        string
	description string
	schema      string
	mutating    bool
	allowed     func(sm *compile.SeatManifest, org *compile.Manifest) bool
	handle      func(ctx context.Context, c *Call) (any, error)
}

// Registry holds the tools.
type Registry struct {
	d     Deps
	tools []*tool
}

// New returns the registry of all platform tools.
func New(d Deps) *Registry {
	r := &Registry{d: d}
	r.tools = append(r.tools, r.selfTools()...)
	r.tools = append(r.tools, r.memoryTools()...)
	r.tools = append(r.tools, r.workTools()...)
	r.tools = append(r.tools, r.messageTools()...)
	r.tools = append(r.tools, r.wakeTools()...)
	r.tools = append(r.tools, r.connectionTools()...)
	return r
}

func always(*compile.SeatManifest, *compile.Manifest) bool { return true }

// List returns the descriptors of the tools the seat may use.
func (r *Registry) List(seat *store.Seat, org *store.Organization) []runtimeapi.ToolDescriptor {
	var out []runtimeapi.ToolDescriptor
	for _, t := range r.tools {
		if t.allowed(&seat.Manifest, &org.Manifest) {
			out = append(out, runtimeapi.ToolDescriptor{Name: t.name, Description: t.description, InputSchema: json.RawMessage(t.schema)})
		}
	}
	return out
}

// Error is a tool-level failure returned to the agent as an error result.
type Error struct {
	Code    string         `json:"error"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

func toolErr(code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// Call runs a tool. Tool-level failures (denied, conflict, invalid) are
// returned as error results; fencing and internal failures as errors.
func (r *Registry) Call(ctx context.Context, name string, c *Call) (runtimeapi.ToolCallResult, error) {
	i := slices.IndexFunc(r.tools, func(t *tool) bool { return t.name == name })
	if i < 0 {
		return runtimeapi.ToolCallResult{}, ErrUnknownTool
	}
	t := r.tools[i]
	if t.mutating && !c.HasGeneration {
		return runtimeapi.ToolCallResult{}, ErrFenced
	}
	if len(c.Args) == 0 || string(c.Args) == "null" {
		c.Args = json.RawMessage(`{}`)
	}
	var out any
	var err error
	if !t.allowed(&c.Seat.Manifest, &c.Org.Manifest) {
		err = toolErr("forbidden", "tool %s is not permitted for this seat", name)
	} else {
		out, err = t.handle(ctx, c)
	}
	res, err := r.result(out, err)
	result := "ok"
	if err != nil {
		result = "error"
	} else if res.IsError {
		result = "denied"
	}
	r.d.Metrics.ToolCalls.WithLabelValues(name, result).Inc()
	return res, err
}

func (r *Registry) result(out any, err error) (runtimeapi.ToolCallResult, error) {
	if err != nil {
		var te *Error
		switch {
		case errors.As(err, &te):
		case errors.Is(err, store.ErrFenced):
			return runtimeapi.ToolCallResult{}, err
		default:
			if te = mapError(err); te == nil {
				return runtimeapi.ToolCallResult{}, err
			}
		}
		b, _ := json.Marshal(te)
		return runtimeapi.ToolCallResult{Content: b, IsError: true}, nil
	}
	b, err := json.Marshal(out)
	if err != nil {
		return runtimeapi.ToolCallResult{}, err
	}
	if len(b) > MaxResultBytes {
		b, _ = json.Marshal(toolErr("result_too_large", "result exceeds %d bytes; request a smaller page", MaxResultBytes))
		return runtimeapi.ToolCallResult{Content: b, IsError: true}, nil
	}
	return runtimeapi.ToolCallResult{Content: b}, nil
}

func mapError(err error) *Error {
	if c, ok := store.IsConflict(err); ok {
		return &Error{Code: "conflict", Message: "the record changed since the expected revision; read it and retry",
			Details: map[string]any{"current_revision": c.Current, "current_author": c.CurrentAuthor}}
	}
	switch {
	case errors.Is(err, store.ErrNotFound):
		return toolErr("not_found", "not found or not accessible")
	case errors.Is(err, store.ErrStructured):
		return toolErr("invalid", "that record is a work item; change it with the work tools")
	case errors.Is(err, store.ErrInboxFull):
		return toolErr("inbox_full", "recipient inbox full; retry later")
	case errors.Is(err, store.ErrInvalid), errors.Is(err, gateway.ErrInvalid):
		return toolErr("invalid", "%s", err.Error())
	case errors.Is(err, gateway.ErrForbidden):
		return toolErr("forbidden", "%s", err.Error())
	}
	return nil
}

func decode(c *Call, v any) error {
	if err := json.Unmarshal(c.Args, v); err != nil {
		return toolErr("invalid", "arguments: %v", err)
	}
	return nil
}

// page decodes a cursor and clamps a limit.
func page(cursor string, limit, def, maxLimit int) (offset, n int, err error) {
	if cursor != "" {
		b, derr := base64.RawURLEncoding.DecodeString(cursor)
		if derr == nil {
			offset, derr = strconv.Atoi(string(b))
		}
		if derr != nil || offset < 0 {
			return 0, 0, toolErr("invalid", "invalid cursor")
		}
	}
	switch {
	case limit <= 0:
		limit = def
	case limit > maxLimit:
		limit = maxLimit
	}
	return offset, limit, nil
}

func cursorAt(offset int) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.Itoa(offset)))
}

// fit returns the largest prefix of items whose wrapped result fits the
// result bound, with a cursor continuing after it. more reports whether
// items beyond those fetched exist.
func fit[T any](items []T, offset int, more bool, wrap func(items []T, next string) any) any {
	n := len(items)
	for {
		next := ""
		if n < len(items) || more {
			next = cursorAt(offset + n)
		}
		out := wrap(items[:n], next)
		b, _ := json.Marshal(out)
		if len(b) <= MaxResultBytes-256 || n <= 1 {
			return out
		}
		n = max(1, n*(MaxResultBytes-256)/len(b))
	}
}

// truncate shortens s to at most n bytes on a rune boundary.
func truncate(s string, n int) (string, bool) {
	if len(s) <= n {
		return s, false
	}
	for n > 0 && !utf8RuneStart(s[n]) {
		n--
	}
	return s[:n], true
}

func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }
