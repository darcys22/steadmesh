package acp

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"unicode/utf8"
)

// MaxRecordBytes bounds a single JSONL record. A future transport must enforce
// this while reading, before allocating a complete record.
const MaxRecordBytes = 1 << 20

// Observation is a normalized, offline wire observation, not a platform event.
// Tool observations do not execute tools or grant permission. Usage is omitted:
// ACP context-window usage is not necessarily per-turn billable token usage.
type Observation struct {
	Kind       string
	Text       string
	ToolCallID string
	StopReason string
	// Reply is populated only with a cancelled permission outcome.
	Reply json.RawMessage
}

// TurnDecoder checks one session's serialized ACP v1 prompt exchange. It is
// not a transport, an initializer, an acpx CLI parser, or a concurrency boundary.
// A decoder is single-use; any invalid/unsupported record poisons it.
type TurnDecoder struct {
	session  string
	promptID string
	done     bool
	failed   bool
}

// NewTurnDecoder requires a string JSON-RPC prompt ID. Numeric IDs are valid
// ACP, but intentionally outside this narrow spike's supported wire subset.
func NewTurnDecoder(session, promptID string) (*TurnDecoder, error) {
	if session == "" || promptID == "" {
		return nil, errors.New("acp: session and prompt ID required")
	}
	return &TurnDecoder{session: session, promptID: promptID}, nil
}

func (d *TurnDecoder) Decode(line []byte) (o Observation, err error) {
	defer func() {
		if err != nil {
			d.failed = true
		}
	}()
	if d.failed || d.done || d.session == "" || d.promptID == "" {
		return o, errors.New("acp: turn decoder is closed")
	}
	if len(line) > MaxRecordBytes {
		return o, errors.New("acp: oversized record")
	}
	if !utf8.Valid(line) || uniqueJSON(line) != nil {
		return o, errors.New("acp: invalid or ambiguous JSON")
	}
	var m struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params"`
		Result  json.RawMessage `json:"result"`
		Error   json.RawMessage `json:"error"`
	}
	if json.Unmarshal(line, &m) != nil || m.JSONRPC != "2.0" {
		return o, errors.New("acp: invalid JSON-RPC record")
	}
	if len(m.Error) != 0 {
		return o, errors.New("acp: remote error (payload withheld)")
	}
	if m.Method == "" {
		var id string
		if json.Unmarshal(m.ID, &id) != nil || id != d.promptID || len(m.Params) != 0 {
			return o, errors.New("acp: uncorrelated prompt response")
		}
		var result struct {
			StopReason string `json:"stopReason"`
		}
		if json.Unmarshal(m.Result, &result) != nil {
			return o, errors.New("acp: invalid prompt result")
		}
		switch result.StopReason {
		case "end_turn", "max_tokens", "max_turn_requests", "refusal", "cancelled":
		default:
			return o, errors.New("acp: unsupported stop reason")
		}
		d.done = true
		return Observation{Kind: "stop", StopReason: result.StopReason}, nil
	}
	if len(m.Result) != 0 {
		return o, errors.New("acp: mixed request and response")
	}
	var p struct {
		SessionID string `json:"sessionId"`
		Update    struct {
			SessionUpdate string `json:"sessionUpdate"`
			ToolCallID    string `json:"toolCallId"`
			Content       struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"update"`
	}
	// Permission/tool payloads have different content shapes. Only decode the
	// session first, then inspect update content for the supported variants.
	var session struct {
		SessionID string `json:"sessionId"`
	}
	if json.Unmarshal(m.Params, &session) != nil || session.SessionID != d.session {
		return o, errors.New("acp: wrong or missing session")
	}
	if m.Method == "session/request_permission" {
		var id any
		dec := json.NewDecoder(bytes.NewReader(m.ID))
		dec.UseNumber()
		if dec.Decode(&id) != nil {
			return o, errors.New("acp: invalid permission request ID")
		}
		switch id.(type) {
		case string, json.Number:
		default:
			return o, errors.New("acp: invalid permission request ID")
		}
		reply, marshalErr := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"outcome": map[string]string{"outcome": "cancelled"}}})
		if marshalErr != nil {
			return o, errors.New("acp: permission reply encoding failed")
		}
		return Observation{Kind: "permission_cancelled", Reply: reply}, nil
	}
	if m.Method != "session/update" || len(m.ID) != 0 {
		return o, errors.New("acp: unsupported agent request")
	}
	// Decode content separately because tool_call content is an array, whereas
	// message chunks carry a single content block.
	var update struct {
		Update struct {
			SessionUpdate string `json:"sessionUpdate"`
			ToolCallID    string `json:"toolCallId"`
		} `json:"update"`
	}
	if json.Unmarshal(m.Params, &update) != nil {
		return o, errors.New("acp: invalid update")
	}
	switch update.Update.SessionUpdate {
	case "agent_message_chunk":
		if json.Unmarshal(m.Params, &p) != nil || p.Update.Content.Type != "text" {
			return o, errors.New("acp: unsupported message content")
		}
		return Observation{Kind: "output", Text: p.Update.Content.Text}, nil
	case "tool_call", "tool_call_update":
		if update.Update.ToolCallID == "" {
			return o, errors.New("acp: missing tool call ID")
		}
		return Observation{Kind: update.Update.SessionUpdate, ToolCallID: update.Update.ToolCallID}, nil
	default:
		return o, errors.New("acp: unsupported session update")
	}
}

// Finish must be called at EOF. A partial transcript is never completion.
func (d *TurnDecoder) Finish() error {
	if d.failed || !d.done {
		d.failed = true
		return errors.New("acp: incomplete or invalid turn")
	}
	return nil
}

// uniqueJSON rejects duplicate object keys at every nesting level. Accepting
// last-key-wins could correlate a record differently than another participant.
func uniqueJSON(raw []byte) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var value func() error
	value = func() error {
		token, err := d.Token()
		if err != nil {
			return err
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			keys := make(map[string]bool)
			for d.More() {
				key, err := d.Token()
				if err != nil {
					return err
				}
				name, ok := key.(string)
				if !ok || keys[name] {
					return errors.New("duplicate or invalid key")
				}
				keys[name] = true
				if err := value(); err != nil {
					return err
				}
			}
		case '[':
			for d.More() {
				if err := value(); err != nil {
					return err
				}
			}
		default:
			return errors.New("unexpected delimiter")
		}
		_, err = d.Token()
		return err
	}
	// json.Valid also bounds nesting depth before recursive token inspection.
	if !json.Valid(raw) {
		return errors.New("invalid JSON")
	}
	if err := value(); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return errors.New("trailing JSON")
	}
	return nil
}
