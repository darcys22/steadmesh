package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
)

// meter observes a proxied model response without delaying streaming. Bounds
// apply to both JSON bodies and SSE lines. Lost/malformed usage never refunds a
// reservation; the complete response still passes through unchanged.
type meter struct {
	http.ResponseWriter
	status        int
	sse           bool
	buf           bytes.Buffer
	usage         map[string]int64
	complete      bool
	terminalAPI   string
	finalUsageAPI string
	invalid       bool
}

const meterJSONLimit = 1 << 20

func (m *meter) WriteHeader(code int) {
	if m.status == 0 {
		m.status = code
		m.sse = strings.HasPrefix(m.Header().Get("Content-Type"), "text/event-stream")
	}
	m.ResponseWriter.WriteHeader(code)
}

func (m *meter) Write(b []byte) (int, error) {
	if m.status == 0 {
		m.WriteHeader(http.StatusOK)
	}
	n, err := m.ResponseWriter.Write(b)
	m.observe(b[:n])
	if err != nil || n != len(b) {
		m.invalid = true
	}
	return n, err
}
func (m *meter) Flush() {
	if m.status == 0 {
		m.WriteHeader(http.StatusOK)
	}
	if f, ok := m.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
func (m *meter) Unwrap() http.ResponseWriter { return m.ResponseWriter }

func (m *meter) observe(b []byte) {
	if m.invalid {
		return
	}
	if !m.sse {
		if m.buf.Len()+len(b) > meterJSONLimit {
			m.invalid = true
			m.buf.Reset()
			return
		}
		m.buf.Write(b)
		return
	}
	for len(b) > 0 {
		line, rest, found := bytes.Cut(b, []byte{'\n'})
		if m.buf.Len()+len(line) > meterJSONLimit {
			m.invalid = true
			m.buf.Reset()
			return
		}
		m.buf.Write(line)
		if !found {
			return
		}
		m.scanLine(m.buf.Bytes())
		m.buf.Reset()
		b = rest
	}
}
func (m *meter) scanLine(line []byte) {
	if data, ok := bytes.CutPrefix(bytes.TrimSpace(line), []byte("data:")); ok {
		data = bytes.TrimSpace(data)
		if bytes.Equal(data, []byte("[DONE]")) {
			m.complete = true
			m.terminalAPI = "openai_chat"
			return
		}
		if !m.scan(data) {
			m.invalid = true
		}
	}
}
func (m *meter) done() {
	if m.invalid {
		return
	}
	if !m.sse {
		m.complete = m.scan(m.buf.Bytes())
		return
	}
	if m.buf.Len() > 0 {
		m.scanLine(m.buf.Bytes())
		m.buf.Reset()
	}
}

// Usage values are cumulative, including Anthropic output usage across start
// and delta. Never let a later smaller/negative/fractional value reduce usage.
func (m *meter) scan(doc []byte) bool {
	var v struct {
		Type    string                     `json:"type"`
		Usage   map[string]json.RawMessage `json:"usage"`
		Message struct {
			Usage map[string]json.RawMessage `json:"usage"`
		} `json:"message"`
		Response struct {
			Usage map[string]json.RawMessage `json:"usage"`
		} `json:"response"`
	}
	if len(doc) == 0 || doc[0] != '{' || json.Unmarshal(doc, &v) != nil {
		return false
	}
	if v.Type == "message_stop" {
		m.complete = true
		m.terminalAPI = "anthropic_messages"
	}
	if v.Type == "response.completed" || v.Type == "response.incomplete" {
		m.complete = true
		m.terminalAPI = "openai_responses"
	}
	if m.sse {
		// Anthropic's message_start output count is preliminary. Only the
		// final message_delta supplies refundable usage. A terminal marker
		// alone must never turn preliminary usage into a refund.
		switch v.Type {
		case "message_delta":
			m.finalUsageAPI = ""
			if validOutputUsage(v.Usage, "output_tokens") {
				m.finalUsageAPI = "anthropic_messages"
			}
		case "response.completed", "response.incomplete":
			m.finalUsageAPI = ""
			if validOutputUsage(v.Response.Usage, "output_tokens") {
				m.finalUsageAPI = "openai_responses"
			}
		default:
			if validOutputUsage(v.Usage, "completion_tokens") {
				m.finalUsageAPI = "openai_chat"
			}
		}
	}
	for _, u := range []map[string]json.RawMessage{v.Message.Usage, v.Response.Usage, v.Usage} {
		for k, raw := range u {
			var n int64
			if json.Unmarshal(raw, &n) != nil || string(raw) == "null" || n < 0 {
				if k == "output_tokens" || k == "completion_tokens" {
					m.invalid = true
				}
				continue
			}
			if m.usage == nil {
				m.usage = map[string]int64{}
			}
			if old, ok := m.usage[k]; !ok || n > old {
				m.usage[k] = n
			}
		}
	}
	return true
}

func (m *meter) outputTokens(api string) (int64, bool) {
	if m.invalid || !m.complete || (m.sse && (m.terminalAPI != api || m.finalUsageAPI != api)) || m.status < 200 || m.status >= 300 {
		return 0, false
	}
	key := "output_tokens"
	if api == "openai_chat" {
		key = "completion_tokens"
	}
	n, ok := m.usage[key]
	return n, ok
}

func validOutputUsage(usage map[string]json.RawMessage, key string) bool {
	raw, ok := usage[key]
	var n int64
	return ok && string(raw) != "null" && json.Unmarshal(raw, &n) == nil && n >= 0
}
