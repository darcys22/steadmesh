package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
)

// meter observes a proxied model response: its status and the token usage
// the endpoint reported, from a JSON body or from server-sent events.
type meter struct {
	http.ResponseWriter
	status int
	sse    bool
	buf    bytes.Buffer // JSON body (bounded) or the partial SSE line
	usage  map[string]int64
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
	return n, err
}

func (m *meter) Flush() {
	if f, ok := m.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (m *meter) Unwrap() http.ResponseWriter { return m.ResponseWriter }

func (m *meter) observe(b []byte) {
	if !m.sse {
		if m.buf.Len()+len(b) <= meterJSONLimit {
			m.buf.Write(b)
		}
		return
	}
	m.buf.Write(b)
	for {
		line, err := m.buf.ReadBytes('\n')
		if err != nil {
			// Keep the incomplete line for the next write.
			rest := append([]byte(nil), line...)
			m.buf.Reset()
			m.buf.Write(rest)
			return
		}
		if data, ok := bytes.CutPrefix(bytes.TrimSpace(line), []byte("data:")); ok {
			m.scan(bytes.TrimSpace(data))
		}
	}
}

// done finishes observation of a non-streamed body.
func (m *meter) done() {
	if !m.sse {
		m.scan(m.buf.Bytes())
	}
}

// scan merges usage from one JSON document: top-level usage (Chat
// Completions, Anthropic message_delta), message.usage (Anthropic
// message_start) or response.usage (Responses).
func (m *meter) scan(doc []byte) {
	if len(doc) == 0 || doc[0] != '{' {
		return
	}
	var v struct {
		Usage    map[string]any                 `json:"usage"`
		Message  struct{ Usage map[string]any } `json:"message"`
		Response struct{ Usage map[string]any } `json:"response"`
	}
	if json.Unmarshal(doc, &v) != nil {
		return
	}
	for _, u := range []map[string]any{v.Message.Usage, v.Response.Usage, v.Usage} {
		for k, x := range u {
			if f, ok := x.(float64); ok {
				if m.usage == nil {
					m.usage = map[string]int64{}
				}
				m.usage[k] = int64(f)
			}
		}
	}
}
