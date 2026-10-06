// Package platform is the controller's client for the platform internal API
// (/internal/v1/*). It authenticates with the controller's own ServiceAccount
// token, re-read from the in-cluster token file on every request because the
// kubelet rotates it.
package platform

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
	"strings"
	"time"

	"github.com/darcys22/steadmesh/pkg/runtimeapi"
)

// DefaultTokenFile is the in-cluster ServiceAccount token.
const DefaultTokenFile = "/var/run/secrets/kubernetes.io/serviceaccount/token"

// API is the subset of the internal API the controller uses.
type API interface {
	Sync(ctx context.Context, req runtimeapi.SyncRequest) (*runtimeapi.SyncResponse, error)
	Runtime(ctx context.Context, orgID string) (*runtimeapi.RuntimeResponse, error)
	Verify(ctx context.Context, orgID string) (*runtimeapi.VerifyResponse, error)
	Fence(ctx context.Context, seatID string, expectedGeneration int64) error
	StartProbe(ctx context.Context, seatID string) (*runtimeapi.ProbeResponse, error)
	GetProbe(ctx context.Context, seatID, probeID string) (*runtimeapi.ProbeResponse, error)
	// DeleteOrganization revokes the organisation's capabilities and retires
	// its seats. With retention "retain" durable rows are kept (A19).
	DeleteOrganization(ctx context.Context, orgID, retention string) error
}

// Error is a non-2xx response.
type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string {
	return fmt.Sprintf("platform: %d %s: %s", e.Status, e.Code, e.Message)
}

// IsNotFound reports a 404 from the platform.
func IsNotFound(err error) bool {
	var pe *Error
	return errors.As(err, &pe) && pe.Status == http.StatusNotFound
}

// Client implements API over HTTP.
type Client struct {
	BaseURL   string
	TokenFile string
	HTTP      *http.Client
}

var _ API = (*Client)(nil)

// New returns a client for baseURL. An empty tokenFile sends no credentials
// (tests only).
func New(baseURL, tokenFile string) *Client {
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), TokenFile: tokenFile, HTTP: &http.Client{Timeout: 30 * time.Second}}
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	if c.TokenFile != "" {
		tok, err := os.ReadFile(c.TokenFile)
		if err != nil {
			return fmt.Errorf("read controller token: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(tok)))
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var e runtimeapi.Error
		_ = json.Unmarshal(data, &e)
		if e.Message == "" {
			e.Message = strings.TrimSpace(string(data))
			if len(e.Message) > 256 {
				e.Message = e.Message[:256]
			}
		}
		return &Error{Status: resp.StatusCode, Code: e.Code, Message: e.Message}
	}
	if out == nil || len(data) == 0 {
		return nil
	}
	return json.Unmarshal(data, out)
}

func (c *Client) Sync(ctx context.Context, req runtimeapi.SyncRequest) (*runtimeapi.SyncResponse, error) {
	var out runtimeapi.SyncResponse
	return &out, c.do(ctx, http.MethodPost, runtimeapi.PathInternalSync, req, &out)
}

func (c *Client) Runtime(ctx context.Context, orgID string) (*runtimeapi.RuntimeResponse, error) {
	var out runtimeapi.RuntimeResponse
	return &out, c.do(ctx, http.MethodGet, runtimeapi.PathInternalOrgs+url.PathEscape(orgID)+"/runtime", nil, &out)
}

func (c *Client) Verify(ctx context.Context, orgID string) (*runtimeapi.VerifyResponse, error) {
	var out runtimeapi.VerifyResponse
	return &out, c.do(ctx, http.MethodPost, runtimeapi.PathInternalOrgs+url.PathEscape(orgID)+"/verify", struct{}{}, &out)
}

func (c *Client) Fence(ctx context.Context, seatID string, gen int64) error {
	return c.do(ctx, http.MethodPost, runtimeapi.PathInternalSeats+url.PathEscape(seatID)+"/fence", runtimeapi.FenceRequest{ExpectedGeneration: gen}, nil)
}

func (c *Client) StartProbe(ctx context.Context, seatID string) (*runtimeapi.ProbeResponse, error) {
	var out runtimeapi.ProbeResponse
	return &out, c.do(ctx, http.MethodPost, runtimeapi.PathInternalSeats+url.PathEscape(seatID)+"/probe", struct{}{}, &out)
}

func (c *Client) GetProbe(ctx context.Context, seatID, probeID string) (*runtimeapi.ProbeResponse, error) {
	var out runtimeapi.ProbeResponse
	return &out, c.do(ctx, http.MethodGet, runtimeapi.PathInternalSeats+url.PathEscape(seatID)+"/probe/"+url.PathEscape(probeID), nil, &out)
}

func (c *Client) DeleteOrganization(ctx context.Context, orgID, retention string) error {
	err := c.do(ctx, http.MethodDelete, runtimeapi.PathInternalOrgs+url.PathEscape(orgID)+"?retention="+url.QueryEscape(retention), nil, nil)
	if IsNotFound(err) {
		return nil
	}
	return err
}
