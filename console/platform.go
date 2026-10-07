// Package console is the Steadmesh Console: a read-only browser view of an
// organisation's seats, runs, handoffs, outputs and readiness. It combines
// the platform's console API (/console/v1) with the AgentOrganization and
// AgentSeat resources and seat Pods in the cluster. It is optional and off by
// default; Terraform enables it in the platform stage.
package console

import (
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

// Platform is the console API of the platform service.
type Platform interface {
	Organizations(ctx context.Context) ([]runtimeapi.ConsoleOrganization, error)
	Organization(ctx context.Context, orgID string) (*runtimeapi.ConsoleOrganization, error)
	Seats(ctx context.Context, orgID string) ([]runtimeapi.ConsoleSeatStatus, error)
	Seat(ctx context.Context, seatID string) (*runtimeapi.ConsoleSeatDetail, error)
	Execution(ctx context.Context, id string) (*runtimeapi.ConsoleExecutionDetail, error)
	Conversations(ctx context.Context, orgID string) ([]runtimeapi.ConsoleConversation, error)
	Conversation(ctx context.Context, id string) (*runtimeapi.ConsoleConversationDetail, error)
	Operations(ctx context.Context, orgID, seat, status string) ([]runtimeapi.ConsoleOperation, error)
	Artifacts(ctx context.Context, orgID string) ([]runtimeapi.ConsoleArtifact, error)
	// Work lists work items in shared stores with their publication state.
	Work(ctx context.Context, orgID string) ([]runtimeapi.ConsoleWorkItem, error)
	// Executions lists the organisation's runs, newest first, optionally in one state.
	Executions(ctx context.Context, orgID, state string) ([]runtimeapi.ConsoleExecution, error)
	// Activity returns items after the given time, or the latest limit items
	// when after is zero.
	Activity(ctx context.Context, orgID string, after time.Time, limit int) ([]runtimeapi.ConsoleActivityItem, error)
}

// ErrNotFound is a 404 from the platform.
var ErrNotFound = errors.New("not found")

// PlatformClient implements Platform over HTTP. It authenticates with the
// console's ServiceAccount token, re-read on every request because the
// kubelet rotates it.
type PlatformClient struct {
	BaseURL   string
	TokenFile string
	HTTP      *http.Client
}

var _ Platform = (*PlatformClient)(nil)

// NewPlatformClient returns a client for baseURL. An empty tokenFile sends no
// credentials (tests only).
func NewPlatformClient(baseURL, tokenFile string) *PlatformClient {
	return &PlatformClient{BaseURL: strings.TrimRight(baseURL, "/"), TokenFile: tokenFile, HTTP: &http.Client{Timeout: 15 * time.Second}}
}

func (c *PlatformClient) get(ctx context.Context, path string, q url.Values, out any) error {
	u := c.BaseURL + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if c.TokenFile != "" {
		tok, err := os.ReadFile(c.TokenFile)
		if err != nil {
			return fmt.Errorf("read console token: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(tok)))
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	if resp.StatusCode != http.StatusOK {
		var e runtimeapi.Error
		_ = json.Unmarshal(data, &e)
		return fmt.Errorf("platform: %d %s: %s", resp.StatusCode, e.Code, e.Message)
	}
	return json.Unmarshal(data, out)
}

func orgPath(orgID, sub string) string {
	return runtimeapi.PathConsoleOrgs + "/" + url.PathEscape(orgID) + sub
}

func (c *PlatformClient) Organizations(ctx context.Context) ([]runtimeapi.ConsoleOrganization, error) {
	var out []runtimeapi.ConsoleOrganization
	return out, c.get(ctx, runtimeapi.PathConsoleOrgs, nil, &out)
}

func (c *PlatformClient) Organization(ctx context.Context, orgID string) (*runtimeapi.ConsoleOrganization, error) {
	var out runtimeapi.ConsoleOrganization
	return &out, c.get(ctx, orgPath(orgID, ""), nil, &out)
}

func (c *PlatformClient) Seats(ctx context.Context, orgID string) ([]runtimeapi.ConsoleSeatStatus, error) {
	var out runtimeapi.ConsoleSeatList
	return out.Seats, c.get(ctx, orgPath(orgID, "/seats"), nil, &out)
}

func (c *PlatformClient) Seat(ctx context.Context, seatID string) (*runtimeapi.ConsoleSeatDetail, error) {
	var out runtimeapi.ConsoleSeatDetail
	return &out, c.get(ctx, runtimeapi.PathConsoleSeats+url.PathEscape(seatID), nil, &out)
}

func (c *PlatformClient) Execution(ctx context.Context, id string) (*runtimeapi.ConsoleExecutionDetail, error) {
	var out runtimeapi.ConsoleExecutionDetail
	return &out, c.get(ctx, runtimeapi.PathConsoleExecutions+url.PathEscape(id), nil, &out)
}

func (c *PlatformClient) Conversations(ctx context.Context, orgID string) ([]runtimeapi.ConsoleConversation, error) {
	var out runtimeapi.ConsoleConversationList
	return out.Conversations, c.get(ctx, orgPath(orgID, "/conversations"), nil, &out)
}

func (c *PlatformClient) Conversation(ctx context.Context, id string) (*runtimeapi.ConsoleConversationDetail, error) {
	var out runtimeapi.ConsoleConversationDetail
	return &out, c.get(ctx, runtimeapi.PathConsoleConversations+url.PathEscape(id), nil, &out)
}

func (c *PlatformClient) Operations(ctx context.Context, orgID, seat, status string) ([]runtimeapi.ConsoleOperation, error) {
	q := url.Values{}
	if seat != "" {
		q.Set("seat", seat)
	}
	if status != "" {
		q.Set("status", status)
	}
	var out runtimeapi.ConsoleOperationList
	return out.Operations, c.get(ctx, orgPath(orgID, "/operations"), q, &out)
}

func (c *PlatformClient) Artifacts(ctx context.Context, orgID string) ([]runtimeapi.ConsoleArtifact, error) {
	var out runtimeapi.ConsoleArtifactList
	return out.Artifacts, c.get(ctx, orgPath(orgID, "/artifacts"), nil, &out)
}

func (c *PlatformClient) Activity(ctx context.Context, orgID string, after time.Time, limit int) ([]runtimeapi.ConsoleActivityItem, error) {
	q := url.Values{"limit": {fmt.Sprint(limit)}}
	if !after.IsZero() {
		q.Set("after", after.UTC().Format(time.RFC3339Nano))
	}
	var out runtimeapi.ConsoleActivity
	return out.Items, c.get(ctx, orgPath(orgID, "/activity"), q, &out)
}

func (c *PlatformClient) Executions(ctx context.Context, orgID, state string) ([]runtimeapi.ConsoleExecution, error) {
	q := url.Values{}
	if state != "" {
		q.Set("state", state)
	}
	var out runtimeapi.ConsoleExecutionList
	return out.Executions, c.get(ctx, orgPath(orgID, "/executions"), q, &out)
}

func (c *PlatformClient) Work(ctx context.Context, orgID string) ([]runtimeapi.ConsoleWorkItem, error) {
	var out runtimeapi.ConsoleWorkList
	return out.Work, c.get(ctx, orgPath(orgID, "/work"), nil, &out)
}
