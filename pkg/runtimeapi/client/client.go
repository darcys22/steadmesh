// Package client is the seat-side Go client for the platform runtime API
// (/v1/*). It is used by seat-runner, steadmesh-tools and harness adapters.
//
// Every request re-reads the projected ServiceAccount token from TokenFile,
// because the kubelet rotates it. Mutating requests carry the current lease
// generation in runtimeapi.HeaderGeneration; the platform fences stale
// generations with 409 code=fenced, surfaced here as ErrFenced.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/darcys22/steadmesh/pkg/runtimeapi"
)

// Sentinel errors matched by APIError.Is, so callers can use errors.Is.
var (
	ErrFenced          = errors.New("fenced: lease generation is no longer current")
	ErrConflict        = errors.New("conflict")
	ErrForbidden       = errors.New("forbidden")
	ErrNotFound        = errors.New("not found")
	ErrUnauthenticated = errors.New("unauthenticated")
	ErrInvalid         = errors.New("invalid request")
	ErrBlocked         = errors.New("blocked")
	ErrUnavailable     = errors.New("unavailable")
)

// APIError is a decoded runtimeapi.Error with its HTTP status.
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("runtime api: %d %s", e.Status, e.Code)
	}
	return fmt.Sprintf("runtime api: %d %s: %s", e.Status, e.Code, e.Message)
}

// Is maps the error code (or, without a code, the status) to a sentinel.
func (e *APIError) Is(target error) bool {
	switch e.Code {
	case "fenced":
		return target == ErrFenced
	case "conflict":
		return target == ErrConflict
	case "forbidden":
		return target == ErrForbidden
	case "not_found":
		return target == ErrNotFound
	case "unauthenticated":
		return target == ErrUnauthenticated
	case "invalid":
		return target == ErrInvalid
	case "blocked":
		return target == ErrBlocked
	case "unavailable":
		return target == ErrUnavailable
	case "":
		switch e.Status {
		case http.StatusConflict:
			return target == ErrConflict
		case http.StatusForbidden:
			return target == ErrForbidden
		case http.StatusNotFound:
			return target == ErrNotFound
		case http.StatusUnauthorized:
			return target == ErrUnauthenticated
		case http.StatusBadRequest:
			return target == ErrInvalid
		case http.StatusServiceUnavailable, http.StatusBadGateway, http.StatusGatewayTimeout:
			return target == ErrUnavailable
		}
	}
	return false
}

// IsFenced reports whether err means the caller's generation is obsolete.
func IsFenced(err error) bool { return errors.Is(err, ErrFenced) }

// Config configures a Client.
type Config struct {
	// BaseURL is the platform URL, e.g. http://steadmesh-platform.steadmesh-system.svc:8080.
	BaseURL string
	// TokenFile holds the projected token. It is read on every request.
	TokenFile string
	// Generation returns the current lease generation; 0 omits the header.
	Generation func() int64
	// Execution returns the current execution id; "" omits the header.
	Execution func() string
	// HTTPClient defaults to a client without an overall timeout (long polls
	// are bounded by context and the wait parameter).
	HTTPClient *http.Client
	// UserAgent is sent on every request.
	UserAgent string
}

// Client calls the seat-facing runtime API.
type Client struct {
	base      *url.URL
	tokenFile string
	gen       func() int64
	exec      func() string
	hc        *http.Client
	ua        string
}

// New validates cfg and returns a Client.
func New(cfg Config) (*Client, error) {
	if cfg.BaseURL == "" {
		return nil, errors.New("client: BaseURL is required")
	}
	u, err := url.Parse(strings.TrimRight(cfg.BaseURL, "/"))
	if err != nil {
		return nil, fmt.Errorf("client: parse BaseURL: %w", err)
	}
	if cfg.TokenFile == "" {
		return nil, errors.New("client: TokenFile is required")
	}
	c := &Client{base: u, tokenFile: cfg.TokenFile, gen: cfg.Generation, exec: cfg.Execution, hc: cfg.HTTPClient, ua: cfg.UserAgent}
	if c.gen == nil {
		c.gen = func() int64 { return 0 }
	}
	if c.exec == nil {
		c.exec = func() string { return "" }
	}
	if c.hc == nil {
		c.hc = &http.Client{Transport: http.DefaultTransport}
	}
	if c.ua == "" {
		c.ua = "steadmesh-seat"
	}
	return c, nil
}

// BaseURL returns the platform base URL.
func (c *Client) BaseURL() string { return c.base.String() }

// Token reads the current token from the token file.
func (c *Client) Token() (string, error) {
	b, err := os.ReadFile(c.tokenFile)
	if err != nil {
		return "", fmt.Errorf("client: read token: %w", err)
	}
	t := strings.TrimSpace(string(b))
	if t == "" {
		return "", errors.New("client: token file is empty")
	}
	return t, nil
}

// Do sends a request with authentication and fencing headers. A 2xx response
// body is decoded into out when out is non-nil. A 204 leaves out untouched and
// returns (false, nil); otherwise ok is true.
func (c *Client) Do(ctx context.Context, method, path string, query url.Values, in, out any) (bool, error) {
	resp, err := c.Raw(ctx, method, path, query, in)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		_, _ = io.Copy(io.Discard, resp.Body)
		return false, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return false, decodeError(resp)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return true, nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return false, fmt.Errorf("client: read %s %s: %w", method, path, err)
	}
	if len(bytes.TrimSpace(body)) == 0 || string(bytes.TrimSpace(body)) == "null" {
		return false, nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return false, fmt.Errorf("client: decode %s %s: %w", method, path, err)
	}
	return true, nil
}

// Raw sends an authenticated request and returns the undecoded response. The
// caller must close the body. Non-2xx statuses are not converted to errors.
func (c *Client) Raw(ctx context.Context, method, path string, query url.Values, in any) (*http.Response, error) {
	u := *c.base
	u.Path = strings.TrimRight(u.Path, "/") + path
	if query != nil {
		u.RawQuery = query.Encode()
	}
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return nil, fmt.Errorf("client: encode request: %w", err)
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, err
	}
	tok, err := c.Token()
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("User-Agent", c.ua)
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if g := c.gen(); g > 0 {
		req.Header.Set(runtimeapi.HeaderGeneration, strconv.FormatInt(g, 10))
	}
	if e := c.exec(); e != "" {
		req.Header.Set(runtimeapi.HeaderExecution, e)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("client: %s %s: %w", method, path, err)
	}
	return resp, nil
}

func decodeError(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	e := &APIError{Status: resp.StatusCode}
	var wire runtimeapi.Error
	if json.Unmarshal(b, &wire) == nil && (wire.Code != "" || wire.Message != "") {
		e.Code, e.Message = wire.Code, wire.Message
	} else {
		e.Message = strings.TrimSpace(string(b))
		if len(e.Message) > 512 {
			e.Message = e.Message[:512]
		}
	}
	return e
}

// ---- Lease and state -------------------------------------------------------

// AcquireLease acquires (or re-acquires for the same Pod) the seat lease.
func (c *Client) AcquireLease(ctx context.Context, holder string) (runtimeapi.LeaseResponse, error) {
	var out runtimeapi.LeaseResponse
	_, err := c.Do(ctx, http.MethodPost, runtimeapi.PathLeaseAcquire, nil, runtimeapi.LeaseAcquireRequest{Holder: holder}, &out)
	return out, err
}

// RenewLease renews the lease for generation.
func (c *Client) RenewLease(ctx context.Context, generation int64) (runtimeapi.LeaseResponse, error) {
	var out runtimeapi.LeaseResponse
	_, err := c.Do(ctx, http.MethodPost, runtimeapi.PathLeaseRenew, nil, runtimeapi.LeaseRenewRequest{Generation: generation}, &out)
	return out, err
}

// ReleaseLease releases the lease for generation.
func (c *Client) ReleaseLease(ctx context.Context, generation int64) error {
	_, err := c.Do(ctx, http.MethodPost, runtimeapi.PathLeaseRelease, nil, runtimeapi.LeaseRenewRequest{Generation: generation}, nil)
	return err
}

// ReportState reports the seat's technical execution state.
func (c *Client) ReportState(ctx context.Context, req runtimeapi.StateRequest) error {
	_, err := c.Do(ctx, http.MethodPost, runtimeapi.PathState, nil, req, nil)
	return err
}

// ---- Self and bootstrap ----------------------------------------------------

func (c *Client) Self(ctx context.Context) (runtimeapi.Self, error) {
	var out runtimeapi.Self
	_, err := c.Do(ctx, http.MethodGet, runtimeapi.PathSelf, nil, nil, &out)
	return out, err
}

func (c *Client) Bootstrap(ctx context.Context) (runtimeapi.Bootstrap, error) {
	var out runtimeapi.Bootstrap
	_, err := c.Do(ctx, http.MethodGet, runtimeapi.PathBootstrap, nil, nil, &out)
	return out, err
}

// ---- Tools -----------------------------------------------------------------

// ListTools returns the tool descriptors permitted for the seat. It accepts
// both the runtimeapi.ToolList object and a bare array.
func (c *Client) ListTools(ctx context.Context) ([]runtimeapi.ToolDescriptor, error) {
	var raw json.RawMessage
	if _, err := c.Do(ctx, http.MethodGet, runtimeapi.PathTools, nil, nil, &raw); err != nil {
		return nil, err
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil, nil
	}
	if raw[0] == '[' {
		var arr []runtimeapi.ToolDescriptor
		if err := json.Unmarshal(raw, &arr); err != nil {
			return nil, fmt.Errorf("client: decode tools: %w", err)
		}
		return arr, nil
	}
	var list runtimeapi.ToolList
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("client: decode tools: %w", err)
	}
	return list.Tools, nil
}

// CallTool invokes a tool by its canonical dotted name. A tool-level failure
// is reported in the result's IsError, not as an error.
func (c *Client) CallTool(ctx context.Context, name string, args json.RawMessage) (runtimeapi.ToolCallResult, error) {
	if len(bytes.TrimSpace(args)) == 0 {
		args = json.RawMessage(`{}`)
	}
	var out runtimeapi.ToolCallResult
	_, err := c.Do(ctx, http.MethodPost, runtimeapi.PathToolCall+url.PathEscape(name), nil, runtimeapi.ToolCallRequest{Arguments: args}, &out)
	return out, err
}

// ---- Inbox and executions --------------------------------------------------

// InboxNext long-polls for the next delivery for up to wait (rounded to whole
// seconds, at most 60). It returns (nil, nil) when no work arrived.
func (c *Client) InboxNext(ctx context.Context, wait time.Duration) (*runtimeapi.InboxDelivery, error) {
	secs := int(wait / time.Second)
	if secs < 0 {
		secs = 0
	}
	if secs > 60 {
		secs = 60
	}
	q := url.Values{"wait": {strconv.Itoa(secs)}}
	var out runtimeapi.InboxDelivery
	ok, err := c.Do(ctx, http.MethodGet, runtimeapi.PathInboxNext, q, nil, &out)
	if err != nil || !ok || out.DeliveryID == 0 {
		return nil, err
	}
	return &out, nil
}

// Ack completes a delivery and its execution.
func (c *Client) Ack(ctx context.Context, deliveryID int64, req runtimeapi.InboxAckRequest) error {
	path := runtimeapi.PathInboxAck + strconv.FormatInt(deliveryID, 10) + "/ack"
	_, err := c.Do(ctx, http.MethodPost, path, nil, req, nil)
	return err
}

// PostEvents appends events to an execution (sent as a JSON array).
func (c *Client) PostEvents(ctx context.Context, executionID string, events []runtimeapi.ExecutionEvent) error {
	if len(events) == 0 {
		return nil
	}
	path := runtimeapi.PathExecEvents + url.PathEscape(executionID) + "/events"
	_, err := c.Do(ctx, http.MethodPost, path, nil, events, nil)
	return err
}

// PutCheckpoint records the harness recovery descriptor.
func (c *Client) PutCheckpoint(ctx context.Context, cp runtimeapi.Checkpoint) error {
	_, err := c.Do(ctx, http.MethodPut, runtimeapi.PathCheckpoint, nil, cp, nil)
	return err
}

// ModelProxyURL returns the Anthropic-compatible base URL for a model
// connection, e.g. <platform>/v1/model/<connection>.
func (c *Client) ModelProxyURL(connection string) string {
	return c.BaseURL() + runtimeapi.PathModelProxy + url.PathEscape(connection)
}

// Access returns the seat's current sandbox access.
func (c *Client) Access(ctx context.Context) (runtimeapi.AccessResponse, error) {
	var out runtimeapi.AccessResponse
	_, err := c.Do(ctx, http.MethodGet, runtimeapi.PathAccess, nil, nil, &out)
	return out, err
}

// Credential fetches a sandbox-delivered credential of a connection.
func (c *Client) Credential(ctx context.Context, connection string) (runtimeapi.CredentialResponse, error) {
	var out runtimeapi.CredentialResponse
	_, err := c.Do(ctx, http.MethodPost, runtimeapi.PathCredentials+url.PathEscape(connection), nil, struct{}{}, &out)
	return out, err
}
