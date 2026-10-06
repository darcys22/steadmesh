package claudecode

import (
	"bufio"
	"encoding/json"
	"io"
	"strings"

	"github.com/darcys22/steadmesh/harnesses"
)

// Bounds applied while parsing harness output.
const (
	maxLineBytes = 16 << 20 // larger stream-json lines are dropped
	maxTextBytes = 16 << 10 // text and tool payloads are truncated in events
)

// ParsedEvent is a harness event derived from one stream-json line.
type ParsedEvent struct {
	Kind          string
	CorrelationID string
	Data          any
}

// Result is the terminal "result" line of a claude -p run.
type Result struct {
	Subtype      string          `json:"subtype"`
	IsError      bool            `json:"is_error"`
	Result       string          `json:"result"`
	SessionID    string          `json:"session_id"`
	NumTurns     int             `json:"num_turns"`
	DurationMS   int64           `json:"duration_ms"`
	TotalCostUSD float64         `json:"total_cost_usd"`
	StopReason   *string         `json:"stop_reason"`
	Errors       []string        `json:"errors"`
	Usage        json.RawMessage `json:"usage"`
}

// Init is the "system/init" line.
type Init struct {
	SessionID      string   `json:"session_id"`
	Model          string   `json:"model"`
	Tools          []string `json:"tools"`
	PermissionMode string   `json:"permissionMode"`
	APIKeySource   string   `json:"apiKeySource"`
	Version        string   `json:"claude_code_version"`
	MCPServers     []struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	} `json:"mcp_servers"`
}

// StreamParser converts claude --output-format stream-json lines into events.
type StreamParser struct {
	SessionID string
	Init      *Init
	Result    *Result
	// AssistantMessages counts assistant lines, i.e. model output was produced.
	AssistantMessages int
	// LastText is the most recent assistant text block.
	LastText string
}

type line struct {
	Type      string          `json:"type"`
	Subtype   string          `json:"subtype"`
	SessionID string          `json:"session_id"`
	Message   json.RawMessage `json:"message"`
}

type message struct {
	Content json.RawMessage `json:"content"`
}

type block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

// Parse consumes one line and returns the events it produces. Unknown or
// malformed lines produce no events (malformed ones a progress note).
func (p *StreamParser) Parse(raw []byte) []ParsedEvent {
	raw = []byte(strings.TrimSpace(string(raw)))
	if len(raw) == 0 {
		return nil
	}
	var l line
	if err := json.Unmarshal(raw, &l); err != nil {
		return []ParsedEvent{{Kind: harnesses.EventProgress, Data: map[string]any{"phase": "unparsed_output", "text": harnesses.TruncateUTF8(string(raw), 1024)}}}
	}
	if l.SessionID != "" {
		p.SessionID = l.SessionID
	}
	switch l.Type {
	case "system":
		if l.Subtype != "init" {
			return nil
		}
		var in Init
		_ = json.Unmarshal(raw, &in)
		p.Init = &in
		servers := map[string]string{}
		for _, s := range in.MCPServers {
			servers[s.Name] = s.Status
		}
		return []ParsedEvent{{Kind: harnesses.EventProgress, Data: map[string]any{
			"phase": "init", "session_id": in.SessionID, "model": in.Model, "claude_code_version": in.Version,
			"mcp_servers": servers, "tool_count": len(in.Tools), "permission_mode": in.PermissionMode, "api_key_source": in.APIKeySource,
		}}}
	case "assistant":
		p.AssistantMessages++
		var out []ParsedEvent
		for _, b := range blocks(l.Message) {
			switch b.Type {
			case "text":
				if strings.TrimSpace(b.Text) == "" {
					continue
				}
				p.LastText = b.Text
				out = append(out, ParsedEvent{Kind: harnesses.EventOutput, Data: map[string]any{"text": harnesses.TruncateUTF8(b.Text, maxTextBytes)}})
			case "tool_use":
				out = append(out, ParsedEvent{Kind: harnesses.EventToolRequest, CorrelationID: b.ID, Data: map[string]any{"name": b.Name, "input": boundedJSON(b.Input)}})
			}
		}
		return out
	case "user":
		var out []ParsedEvent
		for _, b := range blocks(l.Message) {
			if b.Type == "tool_result" {
				out = append(out, ParsedEvent{Kind: harnesses.EventToolResult, CorrelationID: b.ToolUseID, Data: map[string]any{"is_error": b.IsError, "content": boundedJSON(b.Content)}})
			}
		}
		return out
	case "result":
		var r Result
		_ = json.Unmarshal(raw, &r)
		p.Result = &r
		if r.SessionID != "" {
			p.SessionID = r.SessionID
		}
		if r.IsError {
			return []ParsedEvent{{Kind: harnesses.EventError, Data: map[string]any{"subtype": r.Subtype, "errors": r.Errors, "result": harnesses.TruncateUTF8(r.Result, maxTextBytes)}}}
		}
		return nil
	}
	return nil
}

func blocks(msg json.RawMessage) []block {
	var m message
	if json.Unmarshal(msg, &m) != nil || len(m.Content) == 0 {
		return nil
	}
	var bs []block
	if json.Unmarshal(m.Content, &bs) == nil {
		return bs
	}
	var s string
	if json.Unmarshal(m.Content, &s) == nil && s != "" {
		return []block{{Type: "text", Text: s}}
	}
	return nil
}

// boundedJSON returns v as JSON when small enough, otherwise a truncated string.
func boundedJSON(v json.RawMessage) any {
	if len(v) == 0 {
		return nil
	}
	if len(v) <= maxTextBytes && json.Valid(v) {
		return v
	}
	return map[string]any{"truncated": true, "bytes": len(v), "preview": harnesses.TruncateUTF8(string(v), maxTextBytes)}
}

// ResumeFailed reports whether a result shows that --resume could not find
// or load the session.
func (p *StreamParser) ResumeFailed() bool {
	if p.Result == nil || !p.Result.IsError || p.AssistantMessages > 0 {
		return false
	}
	for _, e := range append(p.Result.Errors, p.Result.Result) {
		le := strings.ToLower(e)
		if strings.Contains(le, "no conversation found") || strings.Contains(le, "session") && (strings.Contains(le, "not found") || strings.Contains(le, "invalid") || strings.Contains(le, "incompatible")) {
			return true
		}
	}
	return false
}

// readLines calls fn for each newline-terminated line of r, dropping lines
// longer than maxLineBytes so a runaway harness cannot exhaust memory.
func readLines(r io.Reader, fn func([]byte)) error {
	br := bufio.NewReaderSize(r, 64<<10)
	var buf []byte
	dropping := false
	for {
		chunk, err := br.ReadSlice('\n')
		if len(chunk) > 0 && !dropping {
			if len(buf)+len(chunk) > maxLineBytes {
				dropping = true
				buf = buf[:0]
			} else {
				buf = append(buf, chunk...)
			}
		}
		switch {
		case err == bufio.ErrBufferFull:
			continue
		case err != nil:
			if len(buf) > 0 && !dropping {
				fn(buf)
			}
			if err == io.EOF {
				return nil
			}
			return err
		}
		if !dropping {
			fn(buf)
		}
		buf, dropping = buf[:0], false
	}
}
