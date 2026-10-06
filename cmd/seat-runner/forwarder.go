package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/darcys22/steadmesh/harnesses"
	"github.com/darcys22/steadmesh/pkg/runtimeapi"
	"github.com/darcys22/steadmesh/pkg/runtimeapi/client"
)

const (
	batchSize     = 50
	flushInterval = 500 * time.Millisecond
	previewBytes  = 4096
	// maxPendingPerExecution bounds memory if the platform is unreachable.
	maxPendingPerExecution = 2000
)

// forwarder streams harness events to /v1/executions/{id}/events in batches,
// capping payload sizes (runtimeapi.MaxEventDataBytes).
type forwarder struct {
	c       *client.Client
	log     *slog.Logger
	in      <-chan harnesses.Event
	onFence func(error)
	flushCh chan chan struct{}
	done    chan struct{}

	pending map[string][]runtimeapi.ExecutionEvent
	order   []string
	dropped map[string]int
}

func newForwarder(c *client.Client, log *slog.Logger, in <-chan harnesses.Event, onFence func(error)) *forwarder {
	return &forwarder{c: c, log: log, in: in, onFence: onFence, flushCh: make(chan chan struct{}), done: make(chan struct{}),
		pending: map[string][]runtimeapi.ExecutionEvent{}, dropped: map[string]int{}}
}

// capData bounds an event payload.
func capData(d json.RawMessage) json.RawMessage {
	if len(d) <= runtimeapi.MaxEventDataBytes {
		return d
	}
	// JSON escaping can grow the preview, so shrink until the wrapper fits.
	for n := previewBytes; ; n /= 2 {
		b, _ := json.Marshal(map[string]any{"truncated": true, "bytes": len(d), "preview": harnesses.TruncateUTF8(string(d), n)})
		if len(b) <= runtimeapi.MaxEventDataBytes || n == 0 {
			return b
		}
	}
}

func (f *forwarder) add(e harnesses.Event) {
	if e.ExecutionID == "" {
		return
	}
	if _, ok := f.pending[e.ExecutionID]; !ok {
		f.order = append(f.order, e.ExecutionID)
	}
	if len(f.pending[e.ExecutionID]) >= maxPendingPerExecution {
		f.dropped[e.ExecutionID]++
		return
	}
	we := e.ToExecutionEvent()
	we.Data = capData(we.Data)
	f.pending[e.ExecutionID] = append(f.pending[e.ExecutionID], we)
}

func (f *forwarder) total() int {
	n := 0
	for _, v := range f.pending {
		n += len(v)
	}
	return n
}

func (f *forwarder) post() {
	for _, id := range f.order {
		evs := f.pending[id]
		if n := f.dropped[id]; n > 0 {
			evs = append(evs, runtimeapi.ExecutionEvent{Kind: harnesses.EventProgress, Time: time.Now().UTC(), Data: harnesses.MustJSON(map[string]any{"phase": "events_dropped", "count": n})})
		}
		for len(evs) > 0 {
			n := min(len(evs), batchSize)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			err := f.c.PostEvents(ctx, id, evs[:n])
			cancel()
			if err != nil {
				if client.IsFenced(err) {
					f.onFence(err)
				}
				f.log.Warn("posting execution events failed; dropping batch", "execution_id", id, "events", n, "err", err)
			}
			evs = evs[n:]
		}
	}
	f.pending = map[string][]runtimeapi.ExecutionEvent{}
	f.order = nil
	f.dropped = map[string]int{}
}

func (f *forwarder) run() {
	defer close(f.done)
	t := time.NewTicker(flushInterval)
	defer t.Stop()
	in := f.in
	for {
		select {
		case e, ok := <-in:
			if !ok {
				f.post()
				// Keep answering flushes until nobody waits.
				for {
					select {
					case ack := <-f.flushCh:
						close(ack)
					case <-time.After(100 * time.Millisecond):
						return
					}
				}
			}
			f.add(e)
			if f.total() >= batchSize {
				f.post()
			}
		case <-t.C:
			if f.total() > 0 {
				f.post()
			}
		case ack := <-f.flushCh:
			// Events sent before the caller's Deliver returned are already in the
			// channel buffer; drain them without blocking.
		drain:
			for {
				select {
				case e, ok := <-in:
					if !ok {
						in = nil
						break drain
					}
					f.add(e)
				default:
					break drain
				}
			}
			f.post()
			close(ack)
			if in == nil {
				return
			}
		}
	}
}

// Flush posts all events received so far.
func (f *forwarder) Flush(ctx context.Context) {
	ack := make(chan struct{})
	select {
	case f.flushCh <- ack:
	case <-f.done:
		return
	case <-ctx.Done():
		return
	}
	select {
	case <-ack:
	case <-ctx.Done():
	}
}

// Wait blocks until the forwarder has drained a closed event channel.
func (f *forwarder) Wait(ctx context.Context) {
	select {
	case <-f.done:
	case <-ctx.Done():
	}
}
