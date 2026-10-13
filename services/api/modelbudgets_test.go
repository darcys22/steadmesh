package api

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBudgetRequestOutputCeilings(t *testing.T) {
	for _, tc := range []struct {
		path, body, key string
		want            int64
	}{
		{"/v1/messages", `{"model":"m","max_tokens":99}`, "max_tokens", 99},
		{"/v1/messages", `{"model":"m"}`, "max_tokens", 0},
		{"/v1/responses", `{"model":"m","max_output_tokens":1000}`, "max_output_tokens", 1000},
		{"/v1/chat/completions", `{"model":"m","n":1,"max_tokens":88}`, "max_tokens", 88},
		{"/v1/chat/completions", `{"model":"m","stream":true,"stream_options":{"include_usage":false}}`, "max_completion_tokens", 0},
	} {
		t.Run(tc.path+tc.body, func(t *testing.T) {
			p := prepareBudgetRequest(tc.path, []byte(tc.body))
			if p.invalid != "" || p.requested != tc.want {
				t.Fatalf("prepare: %+v", p)
			}
			var doc map[string]any
			if err := json.Unmarshal(p.withLimit(7), &doc); err != nil {
				t.Fatal(err)
			}
			if doc[tc.key] != float64(7) {
				t.Fatalf("ceiling missing: %v", doc)
			}
			if p.chat && doc["stream"] == true && doc["stream_options"].(map[string]any)["include_usage"] != true {
				t.Fatal("chat usage not enabled")
			}
			if !p.chat && doc["stream_options"] != nil {
				t.Fatal("chat option leaked to other API")
			}
		})
	}
}

func TestBudgetRequestRejectsUnboundedForms(t *testing.T) {
	for _, tc := range []struct{ path, body string }{
		{"/v1/responses/id", `{}`}, {"/v1/files", `{}`}, {"/v1/responses", `{"background":true}`},
		{"/v1/messages", `null`}, {"/v1/messages", `[]`}, {"/v1/messages", `{"max_tokens":null}`},
		{"/v1/messages", `{"max_tokens":0}`}, {"/v1/messages", `{"max_tokens":-1}`}, {"/v1/messages", `{"max_tokens":1.5}`},
		{"/v1/messages", `{"max_tokens":9223372036854775808}`}, {"/v1/messages", `{"stream":"true"}`},
		{"/v1/chat/completions", `{"n":2}`}, {"/v1/chat/completions", `{"max_tokens":5,"max_completion_tokens":99}`},
	} {
		if p := prepareBudgetRequest(tc.path, []byte(tc.body)); p.invalid == "" {
			t.Errorf("accepted %s %s", tc.path, tc.body)
		}
	}
}

func TestBudgetMeterRefundsOnlyCompleteUsage(t *testing.T) {
	for _, tc := range []struct {
		name, api, contentType, body string
		known                        bool
		want                         int64
	}{
		{"json", "anthropic_messages", "application/json", `{"usage":{"output_tokens":7}}`, true, 7},
		{"json missing", "anthropic_messages", "application/json", `{"id":"m"}`, false, 0},
		{"json malformed", "anthropic_messages", "application/json", `{"usage":{"output_tokens":7}}x`, false, 0},
		{"null", "anthropic_messages", "application/json", `{"usage":{"output_tokens":null}}`, false, 0},
		{"negative", "anthropic_messages", "application/json", `{"usage":{"output_tokens":-1}}`, false, 0},
		{"fractional", "anthropic_messages", "application/json", `{"usage":{"output_tokens":1.5}}`, false, 0},
		{"interrupted", "anthropic_messages", "text/event-stream", "data: {\"usage\":{\"output_tokens\":2}}\n\n", false, 0},
		{"complete", "anthropic_messages", "text/event-stream", "data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":7}}\n\ndata: {\"type\":\"message_stop\"}", true, 7},
		{"decreasing", "anthropic_messages", "text/event-stream", "data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":7}}\n\ndata: {\"usage\":{\"output_tokens\":1}}\n\ndata: {\"type\":\"message_stop\"}\n", true, 7},
		{"chat complete", "openai_chat", "text/event-stream", "data: {\"usage\":{\"completion_tokens\":3}}\n\ndata: [DONE]\n", true, 3},
		{"chat missing usage", "openai_chat", "text/event-stream", "data: [DONE]\n", false, 0},
		{"responses complete", "openai_responses", "text/event-stream", "data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"output_tokens\":4}}}\n", true, 4},

		{"initial only", "anthropic_messages", "text/event-stream", "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"output_tokens\":1}}}\n\ndata: {\"type\":\"message_stop\"}\n", false, 0},
		{"malformed delta", "anthropic_messages", "text/event-stream", "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"output_tokens\":1}}}\n\ndata: {broken}\n\ndata: {\"type\":\"message_stop\"}\n", false, 0},
		{"missing delta usage", "anthropic_messages", "text/event-stream", "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"output_tokens\":1}}}\n\ndata: {\"type\":\"message_delta\"}\n\ndata: {\"type\":\"message_stop\"}\n", false, 0},
		{"wrong terminal", "anthropic_messages", "text/event-stream", "data: {\"usage\":{\"output_tokens\":1}}\n\ndata: [DONE]\n", false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &meter{ResponseWriter: httptest.NewRecorder()}
			m.Header().Set("Content-Type", tc.contentType)
			for _, b := range []byte(tc.body) {
				_, _ = m.Write([]byte{b})
			}
			m.done()
			n, known := m.outputTokens(tc.api)
			if known != tc.known || (known && n != tc.want) {
				t.Fatalf("got %d,%v want %d,%v", n, known, tc.want, tc.known)
			}
		})
	}
}

func TestBudgetMeterBoundsBothFormats(t *testing.T) {
	for _, kind := range []string{"application/json", "text/event-stream"} {
		m := &meter{ResponseWriter: httptest.NewRecorder()}
		m.Header().Set("Content-Type", kind)
		_, _ = m.Write([]byte(strings.Repeat("x", meterJSONLimit+1)))
		m.done()
		if !m.invalid || m.buf.Len() != 0 {
			t.Fatalf("unbounded meter %s: %d", kind, m.buf.Len())
		}
		if _, ok := m.outputTokens("openai_chat"); ok {
			t.Fatal("refunded oversized observation")
		}
	}
}
