package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"sync"
	"sync/atomic"
	"unicode/utf8"
)

// Client speaks ACP v1 over an already isolated, owned duplex transport.
// It does not start arbitrary executables or authorize their external effects.
// Calls are serialized; cancellation closes the transport rather than leaving
// an uncertain request running. Close must unblock reads and writes on transport.
type Client struct {
	transport    io.ReadWriteCloser
	records      chan json.RawMessage
	done         chan struct{}
	once         sync.Once
	busy         sync.Mutex
	next         atomic.Uint64
	session      string
	initialized  bool
	beforePrompt func(context.Context) error
}

func NewClient(transport io.ReadWriteCloser) *Client {
	c := &Client{transport: transport, records: make(chan json.RawMessage, 32), done: make(chan struct{})}
	go c.read()
	return c
}

func (c *Client) Close() error {
	var err error
	c.once.Do(func() { close(c.done); err = c.transport.Close() })
	return err
}

func (c *Client) read() {
	defer c.Close()
	sc := bufio.NewScanner(c.transport)
	sc.Buffer(make([]byte, 64<<10), MaxRecordBytes)
	for sc.Scan() {
		line := sc.Bytes()
		if !utf8.Valid(line) || uniqueJSON(line) != nil {
			return
		}
		select {
		case c.records <- append(json.RawMessage(nil), line...):
		case <-c.done:
			return
		default:
			return // bounded queue overflow is a protocol failure, never a drop
		}
	}
}

func (c *Client) write(ctx context.Context, v any) error {
	data, err := json.Marshal(v)
	if err != nil || len(data) >= MaxRecordBytes {
		return errors.New("acp: invalid or oversized outgoing record")
	}
	written := make(chan error, 1)
	go func() {
		n, err := c.transport.Write(append(data, '\n'))
		if err == nil && n != len(data)+1 {
			err = io.ErrShortWrite
		}
		written <- err
	}()
	select {
	case err := <-written:
		return err
	case <-ctx.Done():
		_ = c.Close()
		return ctx.Err()
	case <-c.done:
		return errors.New("acp: connection closed")
	}
}

// request assumes the caller holds busy. Unknown responses, remote errors and
// invalid envelopes close the connection; raw error details are not surfaced.
func (c *Client) request(ctx context.Context, method string, params any, update func(json.RawMessage) error, result any) (err error) {
	defer func() {
		if err != nil {
			_ = c.Close()
		}
	}()
	if err := ctx.Err(); err != nil {
		return err
	}
	id := "steadmesh-" + strconv.FormatUint(c.next.Add(1), 10)
	if err := c.write(ctx, map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		return err
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.done:
			return errors.New("acp: connection closed before response")
		case line := <-c.records:
			var m struct {
				JSONRPC string          `json:"jsonrpc"`
				ID      json.RawMessage `json:"id"`
				Method  string          `json:"method"`
				Params  json.RawMessage `json:"params"`
				Result  json.RawMessage `json:"result"`
				Error   json.RawMessage `json:"error"`
			}
			if json.Unmarshal(line, &m) != nil || m.JSONRPC != "2.0" {
				return errors.New("acp: invalid envelope")
			}
			if m.Method != "" {
				if len(m.Result) != 0 || len(m.Error) != 0 {
					return errors.New("acp: mixed envelope")
				}
				if len(m.ID) != 0 {
					// No callback can override this policy. Requests for file, terminal,
					// elicitation or other side effects receive method-not-found.
					var reqID any
					if json.Unmarshal(m.ID, &reqID) != nil || reqID == nil {
						return errors.New("acp: invalid request ID")
					}
					switch reqID.(type) {
					case string, float64:
					default:
						return errors.New("acp: invalid request ID")
					}
					response := map[string]any{"jsonrpc": "2.0", "id": m.ID}
					if m.Method == "session/request_permission" {
						response["result"] = map[string]any{"outcome": map[string]string{"outcome": "cancelled"}}
					} else {
						response["error"] = map[string]any{"code": -32601, "message": "Unsupported client capability"}
					}
					if err := c.write(ctx, response); err != nil {
						return err
					}
					continue
				}
				switch m.Method {
				case "session/update":
					if update == nil {
						return errors.New("acp: unexpected session update")
					}
					if err := update(m.Params); err != nil {
						return err
					}
				case "_auth/status_update": // advisory only; never starts authentication
				default:
					return errors.New("acp: unsupported notification")
				}
				continue
			}
			var responseID string
			if json.Unmarshal(m.ID, &responseID) != nil || responseID != id || len(m.Params) != 0 || len(m.Error) != 0 || len(m.Result) == 0 {
				return errors.New("acp: invalid, failed or uncorrelated response")
			}
			if result != nil && json.Unmarshal(m.Result, result) != nil {
				return errors.New("acp: invalid response result")
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			return nil
		}
	}
}

// Initialize requires an exact target identity/version supplied by trusted
// integration code, not an arbitrary server's capability advertisement.
func (c *Client) Initialize(ctx context.Context, name, version string) error {
	if !c.busy.TryLock() {
		return errors.New("acp: concurrent call")
	}
	defer c.busy.Unlock()
	if c.initialized || name == "" || version == "" {
		return errors.New("acp: invalid initialization")
	}
	var res struct {
		ProtocolVersion int `json:"protocolVersion"`
		AgentInfo       struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"agentInfo"`
	}
	err := c.request(ctx, "initialize", map[string]any{"protocolVersion": 1, "clientInfo": map[string]string{"name": "steadmesh", "version": "experimental"}, "clientCapabilities": map[string]any{"fs": map[string]bool{"readTextFile": false, "writeTextFile": false}, "terminal": false}}, nil, &res)
	if err != nil {
		return err
	}
	if res.ProtocolVersion != 1 || res.AgentInfo.Name != name || res.AgentInfo.Version != version {
		_ = c.Close()
		return errors.New("acp: unsupported protocol or target version")
	}
	c.initialized = true
	return nil
}

// NewSession creates a fresh session. Native recovery is intentionally not
// exposed until durable checkpoint conformance has been verified for a target.
func (c *Client) NewSession(ctx context.Context, params SessionParams) (string, error) {
	if !c.busy.TryLock() {
		return "", errors.New("acp: concurrent call")
	}
	defer c.busy.Unlock()
	if !c.initialized || c.session != "" {
		return "", errors.New("acp: invalid session state")
	}
	if err := params.validate(); err != nil {
		return "", err
	}
	var res struct {
		SessionID string `json:"sessionId"`
	}
	if err := c.request(ctx, "session/new", params, metadataUpdate, &res); err != nil {
		return "", err
	}
	if res.SessionID == "" {
		_ = c.Close()
		return "", errors.New("acp: missing session ID")
	}
	c.session = res.SessionID
	return res.SessionID, nil
}

// Prompt returns a protocol stop reason, not a business-completion claim.
// Text is bounded across all chunks. emit must return promptly or respect ctx.
func (c *Client) Prompt(ctx context.Context, text string, emit func(Observation)) (Observation, error) {
	if !c.busy.TryLock() {
		return Observation{}, errors.New("acp: concurrent call")
	}
	defer c.busy.Unlock()
	if !c.initialized || c.session == "" {
		return Observation{}, errors.New("acp: no session")
	}
	if c.beforePrompt != nil {
		if err := c.beforePrompt(ctx); err != nil {
			_ = c.Close()
			return Observation{}, err
		}
	}
	var res struct {
		StopReason string `json:"stopReason"`
	}
	total := 0
	update := func(raw json.RawMessage) error {
		var p struct {
			SessionID string `json:"sessionId"`
			Update    struct {
				SessionUpdate string `json:"sessionUpdate"`
			} `json:"update"`
		}
		if json.Unmarshal(raw, &p) != nil || p.SessionID != c.session {
			return errors.New("acp: wrong session update")
		}
		switch p.Update.SessionUpdate {
		case "agent_message_chunk", "tool_call", "tool_call_update":
			d, _ := NewTurnDecoder(c.session, "observation")
			line, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": raw})
			o, err := d.Decode(line)
			if err != nil {
				return err
			}
			total += len(o.Text)
			if total > MaxRecordBytes {
				return errors.New("acp: output limit exceeded")
			}
			if emit != nil {
				emit(o)
			}
			return nil
		default:
			return metadataUpdate(raw)
		}
	}
	err := c.request(ctx, "session/prompt", map[string]any{"sessionId": c.session, "prompt": []any{map[string]string{"type": "text", "text": text}}}, update, &res)
	if err != nil {
		return Observation{}, err
	}
	switch res.StopReason {
	case "end_turn", "max_tokens", "max_turn_requests", "refusal", "cancelled":
	default:
		_ = c.Close()
		return Observation{}, errors.New("acp: unsupported stop reason")
	}
	return Observation{Kind: "stop", StopReason: res.StopReason}, nil
}

func metadataUpdate(raw json.RawMessage) error {
	var p struct {
		SessionID string `json:"sessionId"`
		Update    struct {
			SessionUpdate string `json:"sessionUpdate"`
		} `json:"update"`
	}
	if json.Unmarshal(raw, &p) != nil || p.SessionID == "" {
		return errors.New("acp: invalid metadata update")
	}
	switch p.Update.SessionUpdate {
	case "available_commands_update", "current_mode_update", "config_option_update", "session_info_update", "usage_update", "plan", "agent_thought_chunk":
		return nil // no execution, accounting or permission changes from metadata
	default:
		return errors.New("acp: unsupported update")
	}
}
