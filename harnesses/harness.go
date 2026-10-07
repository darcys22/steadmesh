// Package harnesses defines the harness adapter contract (design §7.1).
//
// A harness controls its own reasoning loop. The seat-runner supplies an
// Environment (identity, revision, generation, persistent locations,
// instructions and the tool command) and drives the adapter through
// Prepare -> StartOrResume -> Deliver* -> Quiesce -> Checkpoint -> Stop.
//
// Technical completion versus business completion: a TurnResult's Status says
// only whether the harness finished processing a delivered message
// (completed, failed, interrupted, timed_out). Whether the agent considers a
// task done is the agent's claim. Adapters may surface it in
// TurnResult.Claim, but the platform never uses it for lifecycle decisions.
package harnesses

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/darcys22/steadmesh/pkg/runtimeapi"
)

// Adapter is the §7.1 harness adapter interface. Implementations must be safe
// for one Deliver at a time; Quiesce, Stop and Events may be called
// concurrently with Deliver.
type Adapter interface {
	// DescribeCapabilities reports tools, streaming, recovery, interruption
	// and checkpoint modes. It must not require Prepare.
	DescribeCapabilities() Capabilities
	// Prepare validates the pinned runtime, generates startup configuration
	// and attaches platform-owned resources.
	Prepare(ctx context.Context, env Environment) error
	// StartOrResume starts the seat from a recovery descriptor. It reports
	// whether the native session was resumed or a portable handoff was used.
	StartOrResume(ctx context.Context, rec RecoveryDescriptor) (ResumeResult, error)
	// Deliver processes one platform message and blocks until the turn is
	// technically complete. Cancelling ctx interrupts the turn. All events for
	// the turn are emitted on Events before Deliver returns.
	Deliver(ctx context.Context, d Delivery) (TurnResult, error)
	// Events returns the adapter's event stream. The same channel is returned
	// on every call; it is closed by Stop. Consumers must drain it.
	Events() <-chan Event
	// Quiesce stops accepting deliveries and waits for the current turn to
	// reach a safe point. When ctx expires first the turn is interrupted.
	Quiesce(ctx context.Context) (QuiesceResult, error)
	// Checkpoint persists the recovery descriptor and reports its guarantee.
	Checkpoint(ctx context.Context) (runtimeapi.Checkpoint, error)
	// Stop terminates execution without deleting durable seat data. An
	// already-expired ctx means kill immediately.
	Stop(ctx context.Context) error
}

// Errors returned by adapters.
var (
	ErrQuiescing  = errors.New("harness: quiescing; not accepting deliveries")
	ErrStopped    = errors.New("harness: stopped")
	ErrNotStarted = errors.New("harness: StartOrResume has not been called")
	ErrBusy       = errors.New("harness: a turn is already in progress")
)

// Recovery modes.
const (
	RecoveryFresh           = "fresh"            // no previous session
	RecoveryNativeResume    = "native_resume"    // harness-native session resumed
	RecoveryPortableHandoff = "portable_handoff" // new session seeded from the portable handoff (§6.4)
)

// Checkpoint guarantees (runtimeapi.Checkpoint.Guarantee).
const (
	GuaranteeApplication = "application_checkpoint"
	GuaranteeProcess     = "process_snapshot"
)

// Capabilities answers describe_capabilities.
type Capabilities struct {
	Adapter string `json:"adapter"`
	// Version is the pinned runtime version, when known before Prepare.
	Version string `json:"version,omitempty"`
	// ToolTransports lists how platform tools reach the harness: mcp, cli.
	ToolTransports []string `json:"tool_transports"`
	EventStreaming bool     `json:"event_streaming"`
	// Interruption is how a running turn can be interrupted: signal, cooperative or none.
	Interruption string `json:"interruption"`
	// RecoveryModes lists supported recovery modes (Recovery* constants).
	RecoveryModes []string `json:"recovery_modes"`
	// CheckpointGuarantees lists the guarantees Checkpoint can report.
	CheckpointGuarantees []string `json:"checkpoint_guarantees"`
	// GracefulSuspension reports whether a live process can be suspended and
	// resumed. False means Quiesce only reaches a turn boundary.
	GracefulSuspension bool `json:"graceful_suspension"`
	// SessionFormat identifies the native session format family.
	SessionFormat string `json:"session_format,omitempty"`
}

// Environment is what the platform supplies to a harness (§7, §7.2).
type Environment struct {
	OrganizationID string
	SeatID         string
	SeatKey        string
	DisplayName    string
	ConfigRevision string
	// Generation is the execution lease generation (fencing token).
	Generation int64

	// PlatformURL is the platform base URL; TokenFile is the projected token.
	PlatformURL string
	TokenFile   string

	// WorkspaceDir is the harness working directory (/seat/workspace),
	// HomeDir is HOME (/seat/home), RunnerDir holds runner and adapter state
	// (/seat/runner), TmpDir is scratch space (/tmp).
	WorkspaceDir string
	HomeDir      string
	RunnerDir    string
	TmpDir       string

	// Instructions is the rendered instruction text from
	// /etc/steadmesh/manifest/instructions.md (Bootstrap.Instructions only
	// lists the ordered refs).
	Instructions string
	// Bootstrap is the platform bootstrap (identity, guidance, recovery).
	Bootstrap runtimeapi.Bootstrap

	// ToolCommand is the steadmesh-tools binary.
	ToolCommand string
	// ModelProxyURL is the Anthropic-compatible base URL
	// (<platform>/v1/model/<connection>); empty without a model connection.
	ModelProxyURL   string
	ModelConnection string
	Model           string

	// HarnessConfig is the harness profile's free-form config.
	HarnessConfig map[string]string
	// ExtraEnv is passed to harness subprocesses (e.g. STEADMESH_* for tools).
	ExtraEnv []string
}

// RecoveryDescriptor is what StartOrResume restores from.
type RecoveryDescriptor struct {
	// Session is the last harness checkpoint recorded by the platform.
	Session           *runtimeapi.Checkpoint
	Handoff           *runtimeapi.Handoff
	PendingMessages   int
	UnknownOperations []runtimeapi.Operation
	// Note is the platform's recovery note, if any.
	Note string
}

// RecoveryFromBootstrap builds a descriptor from the platform bootstrap.
func RecoveryFromBootstrap(b runtimeapi.Bootstrap) RecoveryDescriptor {
	return RecoveryDescriptor{
		Session:           b.Recovery.Session,
		Handoff:           b.Recovery.Handoff,
		PendingMessages:   b.Recovery.PendingMessages,
		UnknownOperations: b.Recovery.UnknownOperations,
		Note:              b.Recovery.Note,
	}
}

// ResumeResult reports how StartOrResume restored the seat.
type ResumeResult struct {
	Mode      string `json:"mode"`
	SessionID string `json:"session_id,omitempty"`
	// Note is a visible recovery note, e.g. why native resume was not used.
	// It never claims byte-for-byte continuation of a lost session.
	Note string `json:"note,omitempty"`
}

// Delivery is one platform message with its execution context.
type Delivery struct {
	DeliveryID  int64
	ExecutionID string
	Attempt     int
	Message     runtimeapi.Envelope
	// Passive are queued messages (sent with wake=false) handed over with
	// this turn. They are context, not separate requests to answer.
	Passive []runtimeapi.Envelope
}

// Turn statuses: technical completion only.
const (
	TurnCompleted   = "completed"
	TurnFailed      = "failed"
	TurnInterrupted = "interrupted"
	TurnTimedOut    = "timed_out"
)

// TurnResult is the technical outcome of one delivered message.
type TurnResult struct {
	MessageID string `json:"message_id"`
	Status    string `json:"status"`
	Error     string `json:"error,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	// Output is the harness's final text for the turn. It is not delivered to
	// anyone; agents reply through the messages tools.
	Output   string        `json:"output,omitempty"`
	Duration time.Duration `json:"duration"`
	// Claim is the agent's own statement about business completion, if the
	// adapter can identify one. It is informational and agent-owned.
	Claim *Claim `json:"claim,omitempty"`
	// Recovery is set when this turn started a new session in place of a
	// native resume.
	Recovery *ResumeResult `json:"recovery,omitempty"`
}

// Claim is an agent-authored completion claim.
type Claim struct {
	Kind string `json:"kind"` // e.g. task_done
	Text string `json:"text"`
}

// QuiesceResult reports how Quiesce reached its safe point.
type QuiesceResult struct {
	// Interrupted is true when the in-flight turn had to be interrupted.
	Interrupted bool `json:"interrupted"`
	// SafePoint describes the boundary reached, e.g. turn_boundary.
	SafePoint string `json:"safe_point"`
	// SuspensionSupported is false for adapters that cannot suspend a live process.
	SuspensionSupported bool `json:"suspension_supported"`
}

// Event kinds; they match runtimeapi.ExecutionEvent.Kind.
const (
	EventProgress    = "progress"
	EventToolRequest = "tool_request"
	EventToolResult  = "tool_result"
	EventOutput      = "output"
	EventCompletion  = "completion"
	EventError       = "error"
)

// Event is one structured harness event (§7.1 events).
type Event struct {
	Kind string `json:"kind"`
	// ExecutionID and MessageID correlate the event to its delivery.
	ExecutionID string `json:"execution_id"`
	MessageID   string `json:"message_id"`
	// CorrelationID links related events, e.g. a tool_use id.
	CorrelationID string          `json:"correlation_id,omitempty"`
	Data          json.RawMessage `json:"data,omitempty"`
	Time          time.Time       `json:"time"`
}

// ToExecutionEvent converts to the wire form.
func (e Event) ToExecutionEvent() runtimeapi.ExecutionEvent {
	return runtimeapi.ExecutionEvent{Kind: e.Kind, CorrelationID: e.CorrelationID, Data: e.Data, Time: e.Time}
}

// MustJSON marshals v, returning a JSON string on error so events are always valid JSON.
func MustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		b, _ = json.Marshal(map[string]string{"marshal_error": err.Error()})
	}
	return b
}
