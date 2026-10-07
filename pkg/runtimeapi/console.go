package runtimeapi

import (
	"encoding/json"
	"time"
)

// ---- Console (read-only operator) API ----------------------------------------
//
// The console API is served only when the platform is started with a console
// identity (--console-username). Every route is GET and accepts exactly that
// identity. Secret values never appear in any response: connections report the
// kind and path of their secret reference and the last verification result.

const (
	PathConsoleOrgs          = "/console/v1/organizations"  // GET list; /{id}/{seats,conversations,operations,artifacts,activity}
	PathConsoleSeats         = "/console/v1/seats/"         // {id}, {id}/executions
	PathConsoleExecutions    = "/console/v1/executions/"    // {id}
	PathConsoleConversations = "/console/v1/conversations/" // {id}
)

// ConsoleOrganization is an organisation's committed configuration as the
// platform holds it.
type ConsoleOrganization struct {
	ID          string                  `json:"id"`
	Key         string                  `json:"key"`
	Namespace   string                  `json:"namespace"`
	DisplayName string                  `json:"display_name"`
	Revision    string                  `json:"revision"`
	Teams       []string                `json:"teams"`
	Seats       []ConsoleSeatConfig     `json:"seats"`
	Connections []ConsoleConnection     `json:"connections"`
	Bindings    []ConsoleChannelBinding `json:"channel_bindings,omitempty"`
}

// ConsoleSeatConfig is a seat's committed configuration.
type ConsoleSeatConfig struct {
	SeatID              string              `json:"seat_id"`
	OrganizationID      string              `json:"organization_id"`
	Key                 string              `json:"key"`
	DisplayName         string              `json:"display_name"`
	RoleRef             string              `json:"role_ref"`
	Teams               []string            `json:"teams,omitempty"`
	IsRepresentative    bool                `json:"is_representative"`
	HarnessAdapter      string              `json:"harness_adapter"`
	Model               string              `json:"model,omitempty"`
	ConfigRevision      string              `json:"config_revision"`
	PolicyRevision      int64               `json:"policy_revision"`
	PersistentWorkspace bool                `json:"persistent_workspace"`
	SharedWorkspaces    []string            `json:"shared_workspaces,omitempty"`
	PersonalMemory      string              `json:"personal_memory,omitempty"`
	SendTo              []string            `json:"send_to,omitempty"`
	ChannelBindings     []string            `json:"channel_bindings,omitempty"`
	Capabilities        []ConsoleCapability `json:"capabilities,omitempty"`
	CreatedAt           time.Time           `json:"created_at"`
	AdoptedFrom         string              `json:"adopted_from,omitempty"`
}

// ConsoleCapability is one resolved grant of a seat.
type ConsoleCapability struct {
	Resource   string   `json:"resource"`
	Operations []string `json:"operations"`
	Sources    []string `json:"sources,omitempty"`
}

// ConsoleConnection describes a connection and its last verification. The
// secret reference is reported as kind (vault, k8s) and path only.
type ConsoleConnection struct {
	Key        string        `json:"key"`
	Adapter    string        `json:"adapter"`
	Required   bool          `json:"required"`
	SecretKind string        `json:"secret_kind,omitempty"`
	SecretPath string        `json:"secret_path,omitempty"`
	Check      *ConsoleCheck `json:"check,omitempty"`
}

type ConsoleCheck struct {
	OK        bool      `json:"ok"`
	Detail    string    `json:"detail,omitempty"`
	CheckedAt time.Time `json:"checked_at"`
	// Credential is the credential refresh state at the last check.
	Credential CredentialStatus `json:"credential"`
}

type ConsoleChannelBinding struct {
	Key        string `json:"key"`
	Connection string `json:"connection"`
	Seat       string `json:"seat"`
}

// ConsoleSeatStatus is the live state of a seat from the platform's records.
type ConsoleSeatStatus struct {
	SeatRuntime
	// Current is the running execution, if any.
	Current *ConsoleExecution `json:"current,omitempty"`
	// LastProgressAt is the time of the seat's latest tool result or output,
	// as distinct from lease liveness.
	LastProgressAt *time.Time `json:"last_progress_at,omitempty"`
	// Unknown counts the seat's connector operations with unknown outcome.
	UnknownOperations int `json:"unknown_operations"`
}

type ConsoleSeatList struct {
	Seats []ConsoleSeatStatus `json:"seats"`
}

// ConsoleSeatDetail is everything the platform records about one seat.
type ConsoleSeatDetail struct {
	Config     ConsoleSeatConfig    `json:"config"`
	Status     ConsoleSeatStatus    `json:"status"`
	Handoff    *Handoff             `json:"handoff,omitempty"`
	Checkpoint *Checkpoint          `json:"checkpoint,omitempty"`
	Memory     []ConsoleMemoryStore `json:"memory,omitempty"`
	Executions []ConsoleExecution   `json:"executions"`
}

// ConsoleMemoryStore is memory metadata only; record bodies are not exposed.
type ConsoleMemoryStore struct {
	Key           string     `json:"key"`
	Personal      bool       `json:"personal"`
	Records       int        `json:"records"`
	LastUpdatedAt *time.Time `json:"last_updated_at,omitempty"`
}

// ConsoleExecution is one run of a seat. A seat keeps its id across Pod
// replacements; each run has its own id and the lease generation it ran under.
type ConsoleExecution struct {
	ID               string     `json:"id"`
	SeatID           string     `json:"seat_id"`
	SeatKey          string     `json:"seat_key"`
	LeaseGeneration  int64      `json:"lease_generation"`
	ConfigRevision   string     `json:"config_revision"`
	HarnessAdapter   string     `json:"harness_adapter"`
	TriggerMessageID string     `json:"trigger_message_id,omitempty"`
	TriggerSummary   string     `json:"trigger_summary,omitempty"`
	TriggerOrigin    string     `json:"trigger_origin,omitempty"`
	State            string     `json:"state"`
	Error            string     `json:"error,omitempty"`
	StartedAt        time.Time  `json:"started_at"`
	FinishedAt       *time.Time `json:"finished_at,omitempty"`
	Events           int        `json:"events"`
}

type ConsoleExecutionList struct {
	Executions []ConsoleExecution `json:"executions"`
}

// ConsoleExecutionDetail is a run with its trigger, events, connector
// operations and the messages its seat sent while it ran.
type ConsoleExecutionDetail struct {
	Execution  ConsoleExecution   `json:"execution"`
	Trigger    *ConsoleMessage    `json:"trigger,omitempty"`
	Events     []ConsoleEvent     `json:"events"`
	Operations []ConsoleOperation `json:"operations"`
	Sent       []ConsoleMessage   `json:"sent"`
}

type ConsoleEvent struct {
	ID            int64           `json:"id"`
	Kind          string          `json:"kind"`
	CorrelationID string          `json:"correlation_id,omitempty"`
	Data          json.RawMessage `json:"data,omitempty"`
	CreatedAt     time.Time       `json:"created_at"`
}

type ConsoleOperation struct {
	Operation
	SeatKey     string    `json:"seat_key,omitempty"`
	ExecutionID string    `json:"execution_id,omitempty"`
	Attempts    int       `json:"attempts"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type ConsoleOperationList struct {
	Operations []ConsoleOperation `json:"operations"`
}

// ConsoleMessage is a message with its delivery to each recipient seat.
type ConsoleMessage struct {
	ID               string            `json:"id"`
	ConversationID   string            `json:"conversation_id"`
	ConversationKind string            `json:"conversation_kind"`
	Origin           string            `json:"origin"`
	SenderSeat       string            `json:"sender_seat,omitempty"`
	RecipientSeat    string            `json:"recipient_seat,omitempty"`
	RecipientBinding string            `json:"recipient_binding,omitempty"`
	Route            string            `json:"route,omitempty"`
	ParentID         string            `json:"parent_id,omitempty"`
	CorrelationID    string            `json:"correlation_id,omitempty"`
	Body             string            `json:"body"`
	CreatedAt        time.Time         `json:"created_at"`
	Deliveries       []ConsoleDelivery `json:"deliveries,omitempty"`
	// Outbox is the external delivery state of a reply to a human.
	Outbox string `json:"outbox,omitempty"`
}

type ConsoleDelivery struct {
	SeatKey     string `json:"seat_key"`
	State       string `json:"state"` // pending, leased, done, dead
	Attempts    int    `json:"attempts"`
	ExecutionID string `json:"execution_id,omitempty"`
	LastError   string `json:"last_error,omitempty"`
}

type ConsoleConversation struct {
	ID           string    `json:"id"`
	Kind         string    `json:"kind"`
	Binding      string    `json:"binding,omitempty"`
	Participants []string  `json:"participants"`
	Messages     int       `json:"messages"`
	CreatedAt    time.Time `json:"created_at"`
	LastAt       time.Time `json:"last_message_at"`
}

type ConsoleConversationList struct {
	Conversations []ConsoleConversation `json:"conversations"`
}

type ConsoleConversationDetail struct {
	Conversation ConsoleConversation `json:"conversation"`
	Messages     []ConsoleMessage    `json:"messages"`
}

type ConsoleArtifact struct {
	ID            string    `json:"id"`
	OwnerSeat     string    `json:"owner_seat,omitempty"`
	Store         string    `json:"store,omitempty"`
	Location      string    `json:"location"`
	ContentDigest string    `json:"content_digest"`
	SizeBytes     int64     `json:"size_bytes"`
	CreatedAt     time.Time `json:"created_at"`
}

type ConsoleArtifactList struct {
	Artifacts []ConsoleArtifact `json:"artifacts"`
}

// Activity item kinds.
const (
	ActivityMessage     = "message"
	ActivityRunStarted  = "run_started"
	ActivityRunFinished = "run_finished"
	ActivityToolRequest = "tool_request"
	ActivityToolResult  = "tool_result"
	ActivityError       = "error"
	ActivityOperation   = "operation"
)

// ConsoleActivityItem is one entry of the organisation's activity feed.
type ConsoleActivityItem struct {
	// Key identifies the item for de-duplication across overlapping polls.
	Key            string    `json:"key"`
	Kind           string    `json:"kind"`
	At             time.Time `json:"at"`
	SeatKey        string    `json:"seat_key,omitempty"`
	PeerSeat       string    `json:"peer_seat,omitempty"`
	Peer           string    `json:"peer,omitempty"` // binding or connection for non-seat peers
	ExecutionID    string    `json:"execution_id,omitempty"`
	MessageID      string    `json:"message_id,omitempty"`
	ConversationID string    `json:"conversation_id,omitempty"`
	Status         string    `json:"status,omitempty"`
	Summary        string    `json:"summary"`
}

// ConsoleActivity is a page of activity after a cursor time, oldest first.
// Callers poll with after set slightly before the newest At they have seen
// and de-duplicate by Key, so late-committing rows are not missed.
type ConsoleActivity struct {
	Items []ConsoleActivityItem `json:"items"`
}

// ConsoleWorkItem is a work item from a shared store: its schema fields and
// publication state, never free-form note text.
type ConsoleWorkItem struct {
	RecordID     string                   `json:"record_id"`
	Store        string                   `json:"store"`
	Revision     int                      `json:"revision"`
	ID           string                   `json:"id"`
	Objective    string                   `json:"objective"`
	Creator      string                   `json:"creator"`
	Owner        string                   `json:"owner,omitempty"`
	Status       string                   `json:"status"`
	Blocker      string                   `json:"blocker,omitempty"`
	Plan         []ConsoleWorkStep        `json:"plan,omitempty"`
	DependsOn    []string                 `json:"depends_on,omitempty"`
	Acceptance   []string                 `json:"acceptance,omitempty"`
	Evidence     []string                 `json:"evidence,omitempty"`
	Links        []string                 `json:"links,omitempty"`
	UpdatedAt    time.Time                `json:"updated_at"`
	Publications []ConsoleWorkPublication `json:"publications,omitempty"`
}

type ConsoleWorkStep struct {
	Step   string `json:"step"`
	Status string `json:"status"`
}

// ConsoleWorkPublication is a work item's state in an optional tracker.
type ConsoleWorkPublication struct {
	Connection        string `json:"connection"`
	State             string `json:"state"`
	ExternalID        string `json:"external_id,omitempty"`
	ExternalURL       string `json:"external_url,omitempty"`
	PublishedRevision int    `json:"published_revision"`
	LastError         string `json:"last_error,omitempty"`
}

type ConsoleWorkList struct {
	Work []ConsoleWorkItem `json:"work"`
}
