package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// rpcConn is a JSON-RPC connection to `codex app-server` over stdio:
// newline-delimited messages without a "jsonrpc" field (CONTRACT.md §1).
type rpcConn struct {
	w   io.Writer
	wmu sync.Mutex

	mu      sync.Mutex
	nextID  int64
	pending map[int64]chan rpcMessage
	sink    chan rpcMessage // notifications for the active turn, if any
	closed  chan struct{}
	err     error

	// onRequest answers server-to-client requests.
	onRequest func(m rpcMessage) (any, *rpcError)
	// tap, when set, sees every raw line read.
	tap func([]byte)
}

type rpcMessage struct {
	ID     *json.RawMessage `json:"id,omitempty"`
	Method string           `json:"method,omitempty"`
	Params json.RawMessage  `json:"params,omitempty"`
	Result json.RawMessage  `json:"result,omitempty"`
	Error  *rpcError        `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("codex rpc error %d: %s", e.Code, e.Message) }

// errClosed means the app-server exited.
var errClosed = errors.New("codex app-server connection closed")

func newRPC(w io.Writer, r io.Reader, onRequest func(rpcMessage) (any, *rpcError), tap func([]byte)) *rpcConn {
	c := &rpcConn{w: w, pending: map[int64]chan rpcMessage{}, closed: make(chan struct{}), onRequest: onRequest, tap: tap}
	go c.read(r)
	return c
}

func (c *rpcConn) read(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 64<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if c.tap != nil {
			c.tap(append([]byte(nil), line...))
		}
		var m rpcMessage
		if json.Unmarshal(line, &m) != nil {
			continue
		}
		switch {
		case m.Method != "" && m.ID != nil:
			go c.answer(m)
		case m.Method != "":
			c.mu.Lock()
			sink := c.sink
			c.mu.Unlock()
			if sink != nil {
				select {
				case sink <- m:
				case <-c.closed:
				}
			}
		case m.ID != nil:
			var id int64
			if json.Unmarshal(*m.ID, &id) == nil {
				c.mu.Lock()
				ch := c.pending[id]
				delete(c.pending, id)
				c.mu.Unlock()
				if ch != nil {
					ch <- m
				}
			}
		}
	}
	c.mu.Lock()
	c.err = sc.Err()
	c.mu.Unlock()
	close(c.closed)
}

func (c *rpcConn) answer(m rpcMessage) {
	res, rerr := c.onRequest(m)
	out := map[string]any{"id": m.ID}
	if rerr != nil {
		out["error"] = rerr
	} else {
		out["result"] = res
	}
	_ = c.write(out)
}

func (c *rpcConn) write(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, err = c.w.Write(append(b, '\n'))
	return err
}

// call sends a request and waits for its response.
func (c *rpcConn) call(ctx context.Context, method string, params, result any) error {
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	ch := make(chan rpcMessage, 1)
	c.pending[id] = ch
	c.mu.Unlock()
	if err := c.write(map[string]any{"id": id, "method": method, "params": params}); err != nil {
		return err
	}
	select {
	case m := <-ch:
		if m.Error != nil {
			return m.Error
		}
		if result != nil && len(m.Result) > 0 {
			return json.Unmarshal(m.Result, result)
		}
		return nil
	case <-c.closed:
		return errClosed
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return ctx.Err()
	}
}

// notify sends a notification.
func (c *rpcConn) notify(method string, params any) error {
	m := map[string]any{"method": method}
	if params != nil {
		m["params"] = params
	}
	return c.write(m)
}

// subscribe routes notifications to a new channel until the returned func is called.
func (c *rpcConn) subscribe() (<-chan rpcMessage, func()) {
	ch := make(chan rpcMessage, 1024)
	c.mu.Lock()
	c.sink = ch
	c.mu.Unlock()
	return ch, func() {
		c.mu.Lock()
		if c.sink == ch {
			c.sink = nil
		}
		c.mu.Unlock()
	}
}
