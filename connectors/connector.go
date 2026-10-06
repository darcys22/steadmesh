// Package connectors defines the adapter contracts used by the trusted
// platform service (§10). External credentials stay inside the platform
// process; seats only reach connectors through the capability gateway.
package connectors

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
)

// Outcome errors. Adapters wrap one of these so the gateway can decide whether
// a retry is safe (§10.3).
var (
	// ErrAmbiguous means the request may have been accepted but the response
	// was lost. The gateway must not blindly retry.
	ErrAmbiguous = errors.New("outcome unknown")
	// ErrRetryable means the request was definitely not applied (e.g. rate
	// limited or rejected before processing) and can be retried.
	ErrRetryable = errors.New("retryable")
	// ErrPermanent means the request was rejected and will not succeed on retry.
	ErrPermanent = errors.New("permanent failure")
	// ErrUnauthorized means credentials are missing, expired or insufficient.
	ErrUnauthorized = errors.New("unauthorized")
)

// Secrets resolves a secret reference (vault:<path> or k8s:<name>) into its
// key/value pairs. Values never leave the platform process.
type Secrets interface {
	Resolve(ctx context.Context, ref string) (map[string]string, error)
}

// Config is what an adapter factory receives for one declared connection.
type Config struct {
	OrganizationID string
	Key            string // connection key
	Adapter        string
	AccountID      string
	// Endpoint overrides the service base URL (used for fakes and self-hosted).
	Endpoint string
	Secret   map[string]string
	Extra    map[string]string
	HTTP     *http.Client
}

// InboundEvent is a verified event from a communication service.
type InboundEvent struct {
	// EventID is the provider's stable event identity for deduplication.
	EventID string
	// AccountID is the verified installation or workspace identity.
	AccountID string
	UserID    string
	ChannelID string
	// ThreadRef identifies the external conversation (e.g. channel or thread ts).
	ThreadRef string
	Text      string
	MessageTS string
}

// IngressSink persists an inbound event. It must return nil only after the
// event is durably accepted (or recognised as a duplicate); the adapter
// acknowledges the external event afterwards (§9.2).
type IngressSink interface {
	Accept(ctx context.Context, connection string, ev InboundEvent) error
}

// OutboundMessage is a reply to a human through a channel binding.
type OutboundMessage struct {
	ExternalUserID string
	ChannelID      string
	ThreadRef      string
	Text           string
	// IdempotencyKey is the platform operation id.
	IdempotencyKey string
}

// Communication is a human communication adapter (e.g. Slack).
type Communication interface {
	// Verify checks authentication (e.g. auth.test) without sending messages.
	Verify(ctx context.Context) error
	// VerifyUser checks that an external user exists in the authorised account.
	VerifyUser(ctx context.Context, userID string) error
	// Run receives events until ctx is cancelled. It reports connection health
	// through Healthy.
	Run(ctx context.Context, sink IngressSink) error
	// Healthy reports whether the ingress connection is currently established.
	Healthy() (bool, string)
	// Send delivers a message and returns the external receipt (e.g. message ts).
	Send(ctx context.Context, msg OutboundMessage) (string, error)
}

// Result is the outcome of a tracker operation.
type Result struct {
	Receipt string          `json:"receipt"`
	Data    json.RawMessage `json:"data"`
}

// Tracker is a work-tracker adapter (e.g. Linear).
type Tracker interface {
	Verify(ctx context.Context) error
	// Invoke performs an operation. operationID is embedded in created records
	// where the API has no idempotency key, so FindByOperation can read back.
	Invoke(ctx context.Context, operation string, params json.RawMessage, operationID string) (Result, error)
	// FindByOperation looks for the effect of a previous attempt of operationID.
	FindByOperation(ctx context.Context, operation string, params json.RawMessage, operationID string) (*Result, error)
	// ReadOnly reports whether an operation has no side effects.
	ReadOnly(operation string) bool
}

// Model is a model-serving connection. The platform proxies inference so the
// credential never enters the sandbox (§5.2).
type Model interface {
	Verify(ctx context.Context) error
	// Proxy returns a handler serving the provider API with the credential
	// injected. The incoming request path has the platform prefix removed.
	Proxy() http.Handler
}
