package api

import (
	"encoding/json"
	"net/http"
	"strconv"
)

// A budgeted request must use a synchronous, single-completion generation
// endpoint with a provider-enforced output ceiling. Preserve unknown fields
// byte-for-byte rather than round-tripping large numbers through float64.
type budgetRequest struct {
	doc       map[string]json.RawMessage
	key       string
	chat      bool
	requested int64
	invalid   string
}

func prepareBudgetRequest(path string, body []byte) budgetRequest {
	p := budgetRequest{}
	switch path {
	case "/v1/messages":
		p.key = "max_tokens"
	case "/v1/responses":
		p.key = "max_output_tokens"
	case "/v1/chat/completions":
		p.key = "max_completion_tokens"
		p.chat = true
	default:
		p.invalid = "this endpoint is not supported with model budgets"
		return p
	}
	if json.Unmarshal(body, &p.doc) != nil || p.doc == nil {
		p.invalid = "model budgets require a JSON object"
		return p
	}
	for _, field := range []string{"background", "stream"} {
		if raw, ok := p.doc[field]; ok {
			var value bool
			if string(raw) == "null" || json.Unmarshal(raw, &value) != nil || (field == "background" && value) {
				p.invalid = "model budgets require a synchronous request and boolean stream/background fields"
				return p
			}
		}
	}
	if raw, ok := p.doc["n"]; ok && string(raw) != "1" {
		p.invalid = "model budgets require n=1"
		return p
	}
	// Reject ambiguous aliases rather than allowing one to override the cap.
	keys := []string{p.key}
	if path == "/v1/chat/completions" {
		if _, old := p.doc["max_tokens"]; old {
			if _, modern := p.doc[p.key]; modern {
				p.invalid = "set only one output-token limit"
				return p
			}
			p.key = "max_tokens"
			keys = []string{p.key}
		}
	}
	for _, key := range keys {
		if raw, ok := p.doc[key]; ok {
			if json.Unmarshal(raw, &p.requested) != nil || p.requested <= 0 {
				p.invalid = "output-token limit must be a positive integer"
				return p
			}
		}
	}
	return p
}

func (p budgetRequest) withLimit(n int64) []byte {
	p.doc[p.key] = json.RawMessage(strconv.FormatInt(n, 10))
	// Chat streams only include usage if explicitly requested.
	var stream bool
	_ = json.Unmarshal(p.doc["stream"], &stream)
	if p.chat && stream {
		var options map[string]json.RawMessage
		_ = json.Unmarshal(p.doc["stream_options"], &options)
		if options == nil {
			options = map[string]json.RawMessage{}
		}
		options["include_usage"] = json.RawMessage("true")
		p.doc["stream_options"], _ = json.Marshal(options)
	}
	b, _ := json.Marshal(p.doc)
	return b
}

func (s *server) modelBudgets(w http.ResponseWriter, r *http.Request, q *seatReq) {
	execution := q.budgetExecution
	if execution == "" {
		execution = q.execution
	}
	status, err := s.Store.ModelBudgetStatus(r.Context(), q.seat.OrganizationID, q.seat.ID, execution)
	if err != nil {
		s.storeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}
