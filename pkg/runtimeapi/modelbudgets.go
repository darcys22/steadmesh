package runtimeapi

import (
	"github.com/darcys22/steadmesh/pkg/spec"
	"time"
)

const PathModelBudgets = "/v1/model-budgets"

// ModelBudgetStatus reports charged usage, including pending and uncertain
// reservations. Execution usage is included only for the caller's execution.
type ModelBudgetStatus struct {
	Limits                   *spec.ModelBudgets `json:"limits,omitempty"`
	Day                      string             `json:"day"`
	ResetsAt                 time.Time          `json:"resets_at"`
	OrganizationOutputTokens int64              `json:"organization_output_tokens"`
	SeatOutputTokens         int64              `json:"seat_output_tokens"`
	ExecutionOutputTokens    int64              `json:"execution_output_tokens"`
}
