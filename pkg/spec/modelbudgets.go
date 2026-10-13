package spec

// ModelBudgets limits generated output tokens, not input tokens or money.
// Zero scope limits are unlimited. Daily windows use UTC, independent of the
// organisation's display timezone. Removing this block disables enforcement.
type ModelBudgets struct {
	// +optional
	// +kubebuilder:validation:Minimum=0
	OrganizationDailyOutputTokens int64 `json:"organization_daily_output_tokens,omitempty"`
	// SeatDailyOutputTokens applies separately to each durable seat identity.
	// +optional
	// +kubebuilder:validation:Minimum=0
	SeatDailyOutputTokens int64 `json:"seat_daily_output_tokens,omitempty"`
	// ExecutionOutputTokens applies across all model calls in one execution.
	// +optional
	// +kubebuilder:validation:Minimum=0
	ExecutionOutputTokens int64 `json:"execution_output_tokens,omitempty"`
	// MaxRequestOutputTokens is the maximum reservation per model request.
	// Defaults to 4096; the proxy lowers it to available scope headroom.
	// +optional
	// +kubebuilder:validation:Minimum=0
	MaxRequestOutputTokens int64 `json:"max_request_output_tokens,omitempty"`
}

// DefaultMaxRequestOutputTokens is used when the request ceiling is omitted.
const DefaultMaxRequestOutputTokens int64 = 4096

// Valid rejects negative, empty or overflowing policy values.
func (b ModelBudgets) Valid() bool {
	const max = int64(1_000_000_000_000)
	for _, n := range []int64{b.OrganizationDailyOutputTokens, b.SeatDailyOutputTokens, b.ExecutionOutputTokens, b.MaxRequestOutputTokens} {
		if n < 0 || n > max {
			return false
		}
	}
	return b.OrganizationDailyOutputTokens > 0 || b.SeatDailyOutputTokens > 0 || b.ExecutionOutputTokens > 0
}
