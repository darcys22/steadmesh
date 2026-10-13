package platform

import (
	"context"
	"github.com/darcys22/steadmesh/pkg/runtimeapi"
	"net/http"
	"net/url"
)

// TaskRunsAPI is optional for older controller API implementations.
type TaskRunsAPI interface {
	TaskRuns(context.Context, string) ([]runtimeapi.TaskRun, error)
	TaskRunAction(context.Context, string, string, string) error
}

func (c *Client) TaskRuns(ctx context.Context, seatID string) ([]runtimeapi.TaskRun, error) {
	var out []runtimeapi.TaskRun
	err := c.do(ctx, http.MethodGet, runtimeapi.PathInternalSeats+url.PathEscape(seatID)+"/task-runs", nil, &out)
	return out, err
}
func (c *Client) TaskRunAction(ctx context.Context, seatID, id, action string) error {
	return c.do(ctx, http.MethodPost, runtimeapi.PathInternalSeats+url.PathEscape(seatID)+"/task-runs/"+url.PathEscape(id)+"/"+url.PathEscape(action), struct{}{}, nil)
}
