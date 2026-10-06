// Package runtimeapi defines the wire contract of the platform runtime API
// (design §13.2). Three callers use separate authentication paths:
//
//   - Seats (seat-runner, steadmesh-tools, harnesses) call /v1/* with a projected
//     ServiceAccount token whose only audience is steadmesh-gateway. The platform
//     resolves the token to a seat with a TokenReview; clients cannot select a
//     different principal. Mutating calls carry HeaderGeneration and are fenced.
//   - The controller calls /internal/v1/* with its own ServiceAccount token.
//   - Operators use orgctl, which reads Kubernetes status and calls
//     /internal/v1 through the controller's verify flow.
package runtimeapi

import (
	"encoding/json"
	"time"
)

const (
	// HeaderGeneration carries the caller's execution lease generation.
	HeaderGeneration = "X-Steadmesh-Generation"
	// HeaderExecution carries the current execution id, for event correlation.
	HeaderExecution = "X-Steadmesh-Execution"

	// Seat-facing paths.
	PathLeaseAcquire = "/v1/lease/acquire"
	PathLeaseRenew   = "/v1/lease/renew"
	PathLeaseRelease = "/v1/lease/release"
	PathState        = "/v1/state"
	PathSelf         = "/v1/self"
	PathBootstrap    = "/v1/bootstrap"
	PathTools        = "/v1/tools"       // GET: list descriptors
	PathToolCall     = "/v1/tools/"      // POST /v1/tools/{name}
	PathInboxNext    = "/v1/inbox/next"  // GET ?wait=<seconds>
	PathInboxAck     = "/v1/inbox/"      // POST /v1/inbox/{delivery_id}/ack
	PathExecEvents   = "/v1/executions/" // POST /v1/executions/{id}/events
	PathCheckpoint   = "/v1/checkpoint"  // PUT
	PathModelProxy   = "/v1/model/"      // /v1/model/{connection}/... (Anthropic-compatible)

	// Controller-facing paths.
	PathInternalSync  = "/internal/v1/organizations:sync" // POST
	PathInternalOrgs  = "/internal/v1/organizations/"     // {id}/runtime, {id}/verify, {id} DELETE
	PathInternalSeats = "/internal/v1/seats/"             // {id}/fence, {id}/probe, {id}/probe/{probe_id}
	PathHealthz       = "/healthz"
	PathReadyz        = "/readyz"
)

// Error is the JSON body of every non-2xx response.
type Error struct {
	Code    string `json:"code"` // unauthenticated, forbidden, fenced, conflict, not_found, invalid, blocked, unavailable
	Message string `json:"message"`
}

// ---- Lease and state ------------------------------------------------------

type LeaseAcquireRequest struct {
	// Holder is informational; the platform records the Pod UID from the token.
	Holder string `json:"holder,omitempty"`
}

type LeaseResponse struct {
	SeatID     string    `json:"seat_id"`
	Generation int64     `json:"generation"`
	ExpiresAt  time.Time `json:"expires_at"`
	// TTL is how long the lease lasts without renewal.
	TTLSeconds int `json:"ttl_seconds"`
}

type LeaseRenewRequest struct {
	Generation int64 `json:"generation"`
}

type StateRequest struct {
	Generation int64  `json:"generation"`
	State      string `json:"state"` // Warm, Executing, Quiescing, Stopped
	Detail     string `json:"detail,omitempty"`
	// AdoptedRevision is the config revision the runner is executing with.
	AdoptedRevision string `json:"adopted_revision,omitempty"`
}

// ---- Self and bootstrap (§7.2) ---------------------------------------------

type MemoryStoreInfo struct {
	Key        string   `json:"key"`
	Personal   bool     `json:"personal"`
	Operations []string `json:"operations"`
}

type RecipientInfo struct {
	Seat        string `json:"seat"`
	DisplayName string `json:"display_name"`
	CanReply    bool   `json:"can_reply"`
}

type ConnectionInfo struct {
	Key        string   `json:"key"`
	Adapter    string   `json:"adapter"`
	Operations []string `json:"operations"`
}

type InstructionInfo struct {
	Ref   string `json:"ref"`
	Scope string `json:"scope"`
	Order int    `json:"order"`
}

type Self struct {
	OrganizationID   string            `json:"organization_id"`
	OrganizationKey  string            `json:"organization_key"`
	OrganizationName string            `json:"organization_name"`
	SeatID           string            `json:"seat_id"`
	SeatKey          string            `json:"seat_key"`
	DisplayName      string            `json:"display_name"`
	Teams            []string          `json:"teams,omitempty"`
	IsRepresentative bool              `json:"is_representative"`
	ConfigRevision   string            `json:"config_revision"`
	PolicyRevision   int64             `json:"policy_revision"`
	Instructions     []InstructionInfo `json:"instructions"`
	MemoryStores     []MemoryStoreInfo `json:"memory_stores"`
	Recipients       []RecipientInfo   `json:"recipients"`
	Connections      []ConnectionInfo  `json:"connections"`
	ChannelBindings  []string          `json:"channel_bindings,omitempty"`
}

type Recovery struct {
	// Session is the harness checkpoint, if any.
	Session *Checkpoint `json:"session,omitempty"`
	Handoff *Handoff    `json:"handoff,omitempty"`
	// PendingMessages is the number of undelivered inbox messages.
	PendingMessages int `json:"pending_messages"`
	// UnknownOperations are external operations whose outcome is unknown and
	// must be reconciled by the agent before being reissued (§6.3).
	UnknownOperations []Operation `json:"unknown_operations,omitempty"`
	// Note explains the recovery state, e.g. "native session not convertible;
	// resumed from portable handoff" (§6.4).
	Note string `json:"note,omitempty"`
}

type Bootstrap struct {
	Self Self `json:"self"`
	// Instructions is the rendered, ordered instruction text.
	Instructions string `json:"instructions"`
	// Guidance is platform guidance on memory retrieval and progress keeping.
	Guidance string   `json:"guidance"`
	Recovery Recovery `json:"recovery"`
}

// ---- Tools ------------------------------------------------------------------

type ToolDescriptor struct {
	Name        string          `json:"name"` // dotted, e.g. memory.search
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// ToolList is the body of GET /v1/tools.
type ToolList struct {
	Tools []ToolDescriptor `json:"tools"`
}

type ToolCallRequest struct {
	Arguments json.RawMessage `json:"arguments"`
}

type ToolCallResult struct {
	// Content is the bounded JSON result.
	Content json.RawMessage `json:"content"`
	// IsError marks a tool-level failure the agent should see (denied,
	// conflict, invalid). Transport errors use HTTP status codes instead.
	IsError bool `json:"is_error,omitempty"`
}

// ---- Messages and inbox (§9.2) ---------------------------------------------

// Envelope is the durable message envelope. Origin fields are assigned by
// trusted ingress; agent-authored text cannot overwrite them.
type Envelope struct {
	MessageID      string `json:"message_id"`
	OrganizationID string `json:"organization_id"`
	ConversationID string `json:"conversation_id"`
	Origin         string `json:"origin"` // human, seat, system, schedule, probe
	// For human origin: the binding key and verified external user.
	Binding        string    `json:"binding,omitempty"`
	ExternalUserID string    `json:"external_user_id,omitempty"`
	SenderSeat     string    `json:"sender_seat,omitempty"`
	SenderSeatID   string    `json:"sender_seat_id,omitempty"`
	RecipientSeat  string    `json:"recipient_seat"`
	ParentID       string    `json:"parent_id,omitempty"`
	CorrelationID  string    `json:"correlation_id,omitempty"`
	Body           string    `json:"body"`
	CreatedAt      time.Time `json:"created_at"`
	// ReplyRoute describes how to reply: "binding:<key>" or "seat:<key>".
	ReplyRoute string `json:"reply_route,omitempty"`
}

type InboxDelivery struct {
	DeliveryID  int64    `json:"delivery_id"`
	ExecutionID string   `json:"execution_id"`
	Attempt     int      `json:"attempt"`
	Message     Envelope `json:"message"`
}

type InboxAckRequest struct {
	ExecutionID string `json:"execution_id"`
	Outcome     string `json:"outcome"` // completed, failed
	Error       string `json:"error,omitempty"`
}

type ExecutionEvent struct {
	Kind          string          `json:"kind"` // progress, tool_request, tool_result, output, completion, error
	CorrelationID string          `json:"correlation_id,omitempty"`
	Data          json.RawMessage `json:"data,omitempty"`
	Time          time.Time       `json:"time"`
}

// MaxEventDataBytes is the largest ExecutionEvent.Data the platform stores.
// POST /v1/executions/{id}/events accepts one ExecutionEvent or a JSON array
// of them. The runner replaces larger payloads with
// {"truncated":true,"bytes":N,"preview":"..."}.
const MaxEventDataBytes = 8 * 1024

// EventProbeResult is the ExecutionEvent.Kind the runner posts for an
// origin=probe delivery before acking it (outcome=completed). Data is a
// ProbeResult; the platform reads data.checks from any event kind.
const EventProbeResult = "probe_result"

// ProbeResult is the Data of an EventProbeResult event. Checks maps a check
// name (tool, workspace, model) to "ok" or "fail: <reason>"; a value
// starting with "fail" or equal to "false" fails the probe.
type ProbeResult struct {
	OK     bool              `json:"ok"`
	Checks map[string]string `json:"checks"`
}

// ---- Checkpoints and handoff (§6.3, §8.4) ----------------------------------

type Checkpoint struct {
	HarnessAdapter string `json:"harness_adapter"`
	FormatVersion  string `json:"format_version"`
	CheckpointRef  string `json:"checkpoint_ref"`
	// Guarantee is application_checkpoint or process_snapshot (§6.4).
	Guarantee string `json:"guarantee"`
}

type Handoff struct {
	Objective         string   `json:"objective"`
	Unresolved        []string `json:"unresolved,omitempty"`
	RecordIDs         []string `json:"record_ids,omitempty"`
	PendingMessageIDs []string `json:"pending_message_ids,omitempty"`
	OperationIDs      []string `json:"operation_ids,omitempty"`
	Notes             string   `json:"notes,omitempty"`
}

// ---- Connector operations (§10.3) ------------------------------------------

type Operation struct {
	ID              string          `json:"id"`
	Connection      string          `json:"connection"`
	Operation       string          `json:"operation"`
	Target          string          `json:"target,omitempty"`
	IdempotencyKey  string          `json:"idempotency_key"`
	Status          string          `json:"status"` // pending, succeeded, failed, unknown
	ExternalReceipt string          `json:"external_receipt,omitempty"`
	Result          json.RawMessage `json:"result,omitempty"`
	Error           string          `json:"error,omitempty"`
	CreatedAt       time.Time       `json:"created_at"`
}

// ---- Internal (controller) API --------------------------------------------

// SyncRequest establishes durable identities for an organisation's resolved
// manifest. It is idempotent.
type SyncRequest struct {
	Namespace string `json:"namespace"`
	Key       string `json:"key"`
	SourceUID string `json:"source_uid"`
	// Manifest is a compile.Manifest serialised as JSON.
	Manifest json.RawMessage `json:"manifest"`
}

type SeatIdentity struct {
	SeatID         string `json:"seat_id"`
	ServiceAccount string `json:"service_account"`
	PolicyRevision int64  `json:"policy_revision"`
}

type SyncResponse struct {
	OrganizationID string                  `json:"organization_id"`
	Seats          map[string]SeatIdentity `json:"seats"`
	// Retired lists seat keys retired by this sync.
	Retired []string `json:"retired,omitempty"`
}

// SeatRuntime is the platform's view of one seat, used for wake and idle decisions.
type SeatRuntime struct {
	SeatID            string     `json:"seat_id"`
	SeatKey           string     `json:"seat_key"`
	PendingDeliveries int        `json:"pending_deliveries"`
	OldestPendingAge  float64    `json:"oldest_pending_age_seconds"`
	LeaseGeneration   int64      `json:"lease_generation"`
	LeaseHolder       string     `json:"lease_holder,omitempty"`
	LeaseExpiresAt    time.Time  `json:"lease_expires_at"`
	State             string     `json:"state"`
	StateDetail       string     `json:"state_detail,omitempty"`
	AdoptedRevision   string     `json:"adopted_revision,omitempty"`
	LastActivity      *time.Time `json:"last_activity,omitempty"`
	LatestCheckpoint  string     `json:"latest_checkpoint,omitempty"`
	DeadDeliveries    int        `json:"dead_deliveries"`
}

type RuntimeResponse struct {
	Seats map[string]SeatRuntime `json:"seats"`
}

type CheckResult struct {
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

type VerifyResponse struct {
	Connections map[string]CheckResult `json:"connections"`
	Ingress     map[string]CheckResult `json:"ingress"`
	Bindings    map[string]CheckResult `json:"bindings"`
}

type FenceRequest struct {
	// ExpectedGeneration must match the current generation; the platform then
	// increments it so the previous holder's calls are rejected.
	ExpectedGeneration int64 `json:"expected_generation"`
}

type ProbeResponse struct {
	ProbeID string `json:"probe_id"`
	// Status is pending, passed or failed.
	Status string            `json:"status"`
	Checks map[string]string `json:"checks,omitempty"`
	Error  string            `json:"error,omitempty"`
}
