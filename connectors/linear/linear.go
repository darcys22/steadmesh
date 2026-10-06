// Package linear is the Linear work-tracker adapter (ADR-0003). Linear has no
// idempotency keys, so created records carry an "steadmesh-op:<operation id>"
// marker that FindByOperation searches for after an ambiguous outcome (§10.3).
package linear

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/darcys22/steadmesh/connectors"
	"github.com/darcys22/steadmesh/connectors/internal/httpx"
)

// DefaultEndpoint is Linear's GraphQL API.
const DefaultEndpoint = "https://api.linear.app/graphql"

// Operations supported by the adapter (pkg/compile/catalog.go).
const (
	OpProjectRead   = "project.read"
	OpProjectCreate = "project.create"
	OpTaskRead      = "task.read"
	OpTaskWrite     = "task.write"
	OpCommentWrite  = "comment.write"
)

const maxResponseBytes = 4 << 20

// Adapter implements connectors.Tracker for Linear.
type Adapter struct {
	cfg      connectors.Config
	endpoint string
	apiKey   string
	teamID   string
	http     *http.Client
}

var _ connectors.Tracker = (*Adapter)(nil)

// New builds a Linear adapter. cfg.Secret["api_key"] is required;
// cfg.Extra["team_id"] is the default team for created records.
func New(cfg connectors.Config) (connectors.Tracker, error) {
	return newAdapter(cfg)
}

func newAdapter(cfg connectors.Config) (*Adapter, error) {
	key := cfg.Secret["api_key"]
	if key == "" {
		return nil, fmt.Errorf("%w: linear secret must contain api_key", connectors.ErrUnauthorized)
	}
	ep, err := endpointURL(cfg.Endpoint)
	if err != nil {
		return nil, err
	}
	return &Adapter{cfg: cfg, endpoint: ep, apiKey: key, teamID: cfg.Extra["team_id"], http: httpx.Client(cfg.HTTP)}, nil
}

// endpointURL resolves an override: a bare base URL gets /graphql appended.
func endpointURL(override string) (string, error) {
	if override == "" {
		return DefaultEndpoint, nil
	}
	u, err := url.Parse(override)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("%w: invalid linear endpoint %q", connectors.ErrPermanent, override)
	}
	if u.Path == "" || u.Path == "/" {
		u.Path = "/graphql"
	}
	return u.String(), nil
}

// ReadOnly reports whether an operation has no side effects.
func (a *Adapter) ReadOnly(operation string) bool {
	return operation == OpProjectRead || operation == OpTaskRead
}

// Verify runs the viewer query and, when cfg.AccountID is set, checks that the
// key belongs to that organisation (id or url key).
func (a *Adapter) Verify(ctx context.Context) error {
	var out struct {
		Viewer struct {
			ID           string `json:"id"`
			Organization struct {
				ID     string `json:"id"`
				URLKey string `json:"urlKey"`
			} `json:"organization"`
		} `json:"viewer"`
	}
	if err := a.do(ctx, true, qViewer, nil, &out); err != nil {
		return err
	}
	if out.Viewer.ID == "" {
		return fmt.Errorf("%w: viewer query returned no user", connectors.ErrUnauthorized)
	}
	org := out.Viewer.Organization
	if a.cfg.AccountID != "" && a.cfg.AccountID != org.ID && a.cfg.AccountID != org.URLKey {
		return fmt.Errorf("%w: api key belongs to organisation %s (%s), expected %s",
			connectors.ErrUnauthorized, org.ID, org.URLKey, a.cfg.AccountID)
	}
	return nil
}

type gqlError struct {
	Message    string         `json:"message"`
	Extensions map[string]any `json:"extensions"`
}

func (e gqlError) code() string {
	c, _ := e.Extensions["code"].(string)
	return c
}

// do sends one GraphQL request and decodes data into out. read marks a
// request without side effects: an unknown outcome is then retryable.
func (a *Adapter) do(ctx context.Context, read bool, query string, vars map[string]any, out any) error {
	err := a.doRaw(ctx, query, vars, out)
	if read && errors.Is(err, connectors.ErrAmbiguous) {
		return fmt.Errorf("%w: %v", connectors.ErrRetryable, err)
	}
	return err
}

func (a *Adapter) doRaw(ctx context.Context, query string, vars map[string]any, out any) error {
	body, err := json.Marshal(map[string]any{"query": query, "variables": vars})
	if err != nil {
		return fmt.Errorf("%w: encoding request: %v", connectors.ErrPermanent, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("%w: %v", connectors.ErrPermanent, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", a.apiKey)
	resp, err := a.http.Do(req)
	if err != nil {
		return httpx.ClassifyTransport(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		// The status line arrived, so the server processed the request.
		return fmt.Errorf("%w: reading response: %v", connectors.ErrAmbiguous, err)
	}
	var env struct {
		Data   json.RawMessage `json:"data"`
		Errors []gqlError      `json:"errors"`
	}
	jsonErr := json.Unmarshal(raw, &env)
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusUnauthorized ||
		resp.StatusCode == http.StatusForbidden || resp.StatusCode >= 500 {
		return httpx.ClassifyStatus(resp.StatusCode, firstMessage(env.Errors))
	}
	if len(env.Errors) > 0 {
		return classifyGraphQL(env.Errors)
	}
	if resp.StatusCode/100 != 2 {
		return httpx.ClassifyStatus(resp.StatusCode, "")
	}
	if jsonErr != nil {
		return fmt.Errorf("%w: decoding response: %v", connectors.ErrAmbiguous, jsonErr)
	}
	if out != nil {
		if err := json.Unmarshal(env.Data, out); err != nil {
			return fmt.Errorf("%w: decoding data: %v", connectors.ErrAmbiguous, err)
		}
	}
	return nil
}

func firstMessage(errs []gqlError) string {
	if len(errs) == 0 {
		return ""
	}
	return errs[0].Message
}

func classifyGraphQL(errs []gqlError) error {
	e := errs[0]
	msg := e.Message
	if c := e.code(); c != "" {
		msg = c + ": " + msg
	}
	switch e.code() {
	case "RATELIMITED":
		return fmt.Errorf("%w: %s", connectors.ErrRetryable, msg)
	case "AUTHENTICATION_ERROR", "FORBIDDEN":
		return fmt.Errorf("%w: %s", connectors.ErrUnauthorized, msg)
	case "INTERNAL_SERVER_ERROR":
		return fmt.Errorf("%w: %s", connectors.ErrAmbiguous, msg)
	}
	return fmt.Errorf("%w: %s", connectors.ErrPermanent, msg)
}

// Invoke performs an operation.
func (a *Adapter) Invoke(ctx context.Context, operation string, params json.RawMessage, operationID string) (connectors.Result, error) {
	switch operation {
	case OpProjectRead:
		return a.projectRead(ctx, params)
	case OpProjectCreate:
		return a.projectCreate(ctx, params, operationID)
	case OpTaskRead:
		return a.taskRead(ctx, params)
	case OpTaskWrite:
		return a.taskWrite(ctx, params, operationID)
	case OpCommentWrite:
		return a.commentWrite(ctx, params, operationID)
	}
	return connectors.Result{}, fmt.Errorf("%w: unsupported operation %q", connectors.ErrPermanent, operation)
}

// FindByOperation looks for the effect of an earlier attempt of operationID.
// It returns (nil, nil) when no effect is found.
func (a *Adapter) FindByOperation(ctx context.Context, operation string, params json.RawMessage, operationID string) (*connectors.Result, error) {
	if a.ReadOnly(operation) {
		return nil, nil
	}
	if operationID == "" {
		return nil, fmt.Errorf("%w: operation id is required for read-back", connectors.ErrPermanent)
	}
	switch operation {
	case OpProjectCreate:
		return a.findProject(ctx, params, operationID)
	case OpTaskWrite:
		return a.findTask(ctx, params, operationID)
	case OpCommentWrite:
		return a.findComment(ctx, operationID)
	}
	return nil, fmt.Errorf("%w: unsupported operation %q", connectors.ErrPermanent, operation)
}
