package harnesses

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/darcys22/steadmesh/pkg/runtimeapi"
)

// Interruption causes, available through context.Cause on a turn context.
var (
	ErrInterruptedByQuiesce = errors.New("turn interrupted: quiesce deadline reached")
	ErrInterruptedByStop    = errors.New("turn interrupted: harness stopped")
)

// TurnGuard serialises turns and implements the Quiesce and Stop semantics
// shared by adapters that run one turn at a time.
type TurnGuard struct {
	mu        sync.Mutex
	quiescing bool
	stopped   bool
	active    bool
	cancel    context.CancelCauseFunc
	done      chan struct{}
}

// Begin starts a turn. The returned context is cancelled when ctx is, or
// when Quiesce or Stop interrupts the turn. end must be called exactly once.
func (g *TurnGuard) Begin(ctx context.Context) (context.Context, func(), error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	switch {
	case g.stopped:
		return nil, nil, ErrStopped
	case g.quiescing:
		return nil, nil, ErrQuiescing
	case g.active:
		return nil, nil, ErrBusy
	}
	tctx, cancel := context.WithCancelCause(ctx)
	g.active, g.cancel, g.done = true, cancel, make(chan struct{})
	done := g.done
	var once sync.Once
	end := func() {
		once.Do(func() {
			g.mu.Lock()
			g.active = false
			g.mu.Unlock()
			cancel(nil)
			close(done)
		})
	}
	return tctx, end, nil
}

// Quiesce stops new turns and waits for the active one; at ctx expiry the
// turn is interrupted and Quiesce waits for it to unwind.
func (g *TurnGuard) Quiesce(ctx context.Context) QuiesceResult {
	g.mu.Lock()
	g.quiescing = true
	active, done, cancel := g.active, g.done, g.cancel
	g.mu.Unlock()
	res := QuiesceResult{SafePoint: "turn_boundary"}
	if !active {
		return res
	}
	select {
	case <-done:
		return res
	case <-ctx.Done():
	}
	cancel(ErrInterruptedByQuiesce)
	<-done
	res.Interrupted = true
	return res
}

// Stop marks the guard stopped and interrupts any active turn. It returns a
// channel closed when the active turn (if any) has ended.
func (g *TurnGuard) Stop() <-chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.stopped = true
	if g.active {
		g.cancel(ErrInterruptedByStop)
		return g.done
	}
	c := make(chan struct{})
	close(c)
	return c
}

// Stopped reports whether Stop was called.
func (g *TurnGuard) Stopped() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.stopped
}

// StatusFor maps a turn context's state to a TurnResult status, or "" when
// the context is still live.
func StatusFor(tctx context.Context) (string, string) {
	if tctx.Err() == nil {
		return "", ""
	}
	cause := context.Cause(tctx)
	switch {
	case errors.Is(cause, context.DeadlineExceeded):
		return TurnTimedOut, "turn exceeded its maximum duration"
	default:
		return TurnInterrupted, cause.Error()
	}
}

// EventBus is a buffered event channel that can be closed safely while
// emitters are blocked.
type EventBus struct {
	ch     chan Event
	done   chan struct{}
	wg     sync.WaitGroup
	mu     sync.Mutex
	closed bool
}

// NewEventBus returns a bus with the given buffer size.
func NewEventBus(buffer int) *EventBus {
	return &EventBus{ch: make(chan Event, buffer), done: make(chan struct{})}
}

// C returns the receive side.
func (b *EventBus) C() <-chan Event { return b.ch }

// Emit sends an event, blocking while the buffer is full. It drops the event
// when the bus is closed or ctx is done.
func (b *EventBus) Emit(ctx context.Context, e Event) {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.wg.Add(1)
	b.mu.Unlock()
	defer b.wg.Done()
	if e.Time.IsZero() {
		e.Time = time.Now().UTC()
	}
	select {
	case b.ch <- e:
	case <-b.done:
	case <-ctx.Done():
		// The turn was interrupted; still try a non-blocking send so terminal
		// events are not lost when there is buffer space.
		select {
		case b.ch <- e:
		default:
		}
	}
}

// Close unblocks pending emitters and closes the channel. It is idempotent.
func (b *EventBus) Close() {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.closed = true
	close(b.done)
	b.mu.Unlock()
	b.wg.Wait()
	close(b.ch)
}

// RenderBootstrap renders the bootstrap context a harness receives as its
// system context (§7.2): identity, instructions, guidance, tools, routes and
// recovery state. It is deliberately small: memory is retrieved via tools.
func RenderBootstrap(env Environment, recoveryNote string) string {
	b := env.Bootstrap
	s := b.Self
	var sb strings.Builder
	fmt.Fprintf(&sb, "# Seat identity\n\n")
	name := s.DisplayName
	if name == "" {
		name = env.DisplayName
	}
	key := s.SeatKey
	if key == "" {
		key = env.SeatKey
	}
	fmt.Fprintf(&sb, "You are %q (seat key %q, seat id %s) in the organisation %q.\n", name, key, firstNonEmpty(s.SeatID, env.SeatID), firstNonEmpty(s.OrganizationName, s.OrganizationKey))
	if len(s.Teams) > 0 {
		fmt.Fprintf(&sb, "Teams: %s.\n", strings.Join(s.Teams, ", "))
	}
	if s.IsRepresentative {
		sb.WriteString("You are the personal representative of the human bound to you. Their messages reach you through your channel binding.\n")
	}
	fmt.Fprintf(&sb, "Configuration revision: %s. Policy revision: %d. Execution generation: %d.\n\n", firstNonEmpty(s.ConfigRevision, env.ConfigRevision), s.PolicyRevision, env.Generation)

	// Instruction text comes from the manifest (instructions.md);
	// Bootstrap.Instructions only lists refs.
	instr := env.Instructions
	if strings.TrimSpace(instr) != "" {
		sb.WriteString("# Instructions\n\n")
		if len(s.Instructions) > 0 {
			sb.WriteString("Sources, in order: ")
			for i, in := range s.Instructions {
				if i > 0 {
					sb.WriteString("; ")
				}
				fmt.Fprintf(&sb, "%s (%s)", in.Ref, in.Scope)
			}
			sb.WriteString(".\n\n")
		}
		sb.WriteString(strings.TrimSpace(instr))
		sb.WriteString("\n\n")
	}
	if strings.TrimSpace(b.Guidance) != "" {
		sb.WriteString("# Platform guidance\n\n")
		sb.WriteString(strings.TrimSpace(b.Guidance))
		sb.WriteString("\n\n")
	}

	sb.WriteString("# Capabilities\n\n")
	if len(s.MemoryStores) > 0 {
		sb.WriteString("Memory stores (use the memory tools; nothing is preloaded):\n")
		for _, m := range s.MemoryStores {
			p := ""
			if m.Personal {
				p = " (personal)"
			}
			fmt.Fprintf(&sb, "- %s%s: %s\n", m.Key, p, strings.Join(m.Operations, ", "))
		}
	}
	if len(s.Recipients) > 0 {
		sb.WriteString("Seats you may message:\n")
		for _, r := range s.Recipients {
			fmt.Fprintf(&sb, "- %s (%s)\n", r.Seat, r.DisplayName)
		}
	}
	if len(s.Connections) > 0 {
		sb.WriteString("Connections (use connections.invoke):\n")
		for _, c := range s.Connections {
			fmt.Fprintf(&sb, "- %s [%s]: %s\n", c.Key, c.Adapter, strings.Join(c.Operations, ", "))
		}
	}
	if len(s.ChannelBindings) > 0 {
		fmt.Fprintf(&sb, "Channel bindings (for proactive updates via messages.reply with binding): %s\n", strings.Join(s.ChannelBindings, ", "))
	}
	sb.WriteString("\n# How to communicate\n\n")
	sb.WriteString("Each turn delivers one platform message with an envelope. Your final text output is NOT delivered to anyone. " +
		"To answer, call the messages.reply tool with the envelope's message_id. To contact another seat, use messages.send. " +
		"To update your bound human proactively, use messages.reply with binding. " +
		"Record durable progress with handoff.update and memory tools so work can continue after a restart.\n")

	rec := b.Recovery
	notes := []string{}
	if recoveryNote != "" {
		notes = append(notes, recoveryNote)
	}
	if rec.Note != "" && rec.Note != recoveryNote {
		notes = append(notes, rec.Note)
	}
	if len(notes) > 0 || rec.PendingMessages > 0 || len(rec.UnknownOperations) > 0 || rec.Handoff != nil || len(rec.Work) > 0 {
		sb.WriteString("\n# Recovery status\n\n")
		for _, n := range notes {
			fmt.Fprintf(&sb, "Note: %s\n", n)
		}
		if rec.PendingMessages > 0 {
			fmt.Fprintf(&sb, "Pending inbox messages: %d.\n", rec.PendingMessages)
		}
		if len(rec.UnknownOperations) > 0 {
			sb.WriteString("External operations with UNKNOWN outcome. Check them with operations.get or a read in the destination system before reissuing:\n")
			for _, op := range rec.UnknownOperations {
				fmt.Fprintf(&sb, "- %s: %s %s target=%s\n", op.ID, op.Connection, op.Operation, op.Target)
			}
		}
		if len(rec.Work) > 0 {
			sb.WriteString("Work items you own (read each with work.get before continuing):\n")
			for _, w := range rec.Work {
				fmt.Fprintf(&sb, "- %s [%s, revision %d]: %s", w.WorkID, w.Status, w.Revision, w.Objective)
				if w.CurrentStep != "" {
					fmt.Fprintf(&sb, "; current step: %s", w.CurrentStep)
				}
				if w.LastNote != "" {
					fmt.Fprintf(&sb, "; last note: %s", w.LastNote)
				}
				sb.WriteString("\n")
			}
		}
		if rec.Handoff != nil {
			sb.WriteString(RenderHandoff(*rec.Handoff))
		}
	}
	return sb.String()
}

// RenderHandoff renders a portable handoff (§8.4).
func RenderHandoff(h runtimeapi.Handoff) string {
	var sb strings.Builder
	sb.WriteString("Portable handoff from your previous execution:\n")
	if h.Objective != "" {
		fmt.Fprintf(&sb, "- Objective: %s\n", h.Objective)
	}
	for _, u := range h.Unresolved {
		fmt.Fprintf(&sb, "- Unresolved: %s\n", u)
	}
	if len(h.RecordIDs) > 0 {
		fmt.Fprintf(&sb, "- Memory records: %s\n", strings.Join(h.RecordIDs, ", "))
	}
	if len(h.PendingMessageIDs) > 0 {
		fmt.Fprintf(&sb, "- Pending messages: %s\n", strings.Join(h.PendingMessageIDs, ", "))
	}
	if len(h.OperationIDs) > 0 {
		fmt.Fprintf(&sb, "- Operations: %s\n", strings.Join(h.OperationIDs, ", "))
	}
	if h.Notes != "" {
		fmt.Fprintf(&sb, "- Notes: %s\n", h.Notes)
	}
	return sb.String()
}

// RenderEnvelope renders a delivered message as the harness prompt: the
// trusted envelope fields first, then the body.
func RenderEnvelope(d Delivery) string {
	m := d.Message
	var sb strings.Builder
	sb.WriteString("<platform_message>\n")
	fields := map[string]string{
		"message_id":       m.MessageID,
		"conversation_id":  m.ConversationID,
		"origin":           m.Origin,
		"binding":          m.Binding,
		"external_user_id": m.ExternalUserID,
		"sender_seat":      m.SenderSeat,
		"parent_id":        m.ParentID,
		"correlation_id":   m.CorrelationID,
		"reply_route":      m.ReplyRoute,
		"execution_id":     d.ExecutionID,
	}
	if !m.CreatedAt.IsZero() {
		fields["created_at"] = m.CreatedAt.UTC().Format(time.RFC3339)
	}
	if d.Attempt > 1 {
		fields["attempt"] = fmt.Sprint(d.Attempt)
	}
	keys := make([]string, 0, len(fields))
	for k, v := range fields {
		if v != "" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&sb, "%s: %s\n", k, fields[k])
	}
	sb.WriteString("</platform_message>\n")
	fmt.Fprintf(&sb, "To reply, call messages.reply with message_id %q.\n\n", m.MessageID)
	sb.WriteString(m.Body)
	if len(d.Passive) > 0 {
		sb.WriteString("\n\n<queued_messages>\nThese were sent to you without asking for a turn of their own. Treat them as context; reply only if one needs it.\n")
		for _, p := range d.Passive {
			from := firstNonEmpty(p.SenderSeat, p.Binding, p.Origin)
			fmt.Fprintf(&sb, "\n- message_id %s from %s at %s:\n%s\n", p.MessageID, from, p.CreatedAt.UTC().Format(time.RFC3339), p.Body)
		}
		sb.WriteString("</queued_messages>\n")
	}
	return sb.String()
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

// TruncateUTF8 cuts s to at most n bytes without splitting a rune.
func TruncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && n < len(s) && (s[n]&0xC0) == 0x80 {
		n--
	}
	return s[:n]
}
