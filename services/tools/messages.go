package tools

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/darcys22/steadmesh/pkg/compile"
	"github.com/darcys22/steadmesh/pkg/runtimeapi"
	"github.com/darcys22/steadmesh/services/policy"
	"github.com/darcys22/steadmesh/services/store"
)

const (
	maxMessageBody = 12 << 10
	historyBody    = 4 << 10
)

func (r *Registry) messageTools() []*tool {
	canSend := func(sm *compile.SeatManifest, _ *compile.Manifest) bool { return len(sm.SendTo) > 0 }
	return []*tool{
		{name: "messages.recipients", description: "List the seats this seat may message, and whether they can reply.",
			schema: `{"type":"object","properties":{},"additionalProperties":false}`, allowed: canSend, handle: r.recipients},
		{name: "messages.send", description: "Send a message to a seat over a declared route. By default it starts a turn for the recipient. With wake false it is queued instead and handed over with the recipient's next turn, for FYIs that need no action now. Continue a conversation with conversation_id, and correlate related work with correlation_id.",
			schema:   `{"type":"object","properties":{"to":{"type":"string","description":"recipient seat key"},"body":{"type":"string","maxLength":12288},"conversation_id":{"type":"string"},"correlation_id":{"type":"string"},"wake":{"type":"boolean","default":true}},"required":["to","body"],"additionalProperties":false}`,
			mutating: true, allowed: canSend, handle: r.send},
		{name: "messages.reply", description: "Reply to a message you received (message_id), or, as a representative, send an update to your bound human (binding). wake false queues a reply to a seat without starting a turn for it.",
			schema:   `{"type":"object","properties":{"message_id":{"type":"string"},"binding":{"type":"string"},"body":{"type":"string","maxLength":12288},"wake":{"type":"boolean","default":true}},"required":["body"],"additionalProperties":false}`,
			mutating: true,
			allowed: func(sm *compile.SeatManifest, _ *compile.Manifest) bool {
				return len(sm.SendTo) > 0 || len(sm.ReceiveFrom) > 0 || len(sm.ChannelBindings) > 0
			},
			handle: r.reply},
		{name: "messages.inbox", description: "Read the messages queued for you without a turn of their own (sent with wake false). They are also handed over automatically with your next turn.",
			schema:   `{"type":"object","properties":{"max_results":{"type":"integer","minimum":1,"maximum":20}},"additionalProperties":false}`,
			mutating: true, allowed: always, handle: r.inbox},
		{name: "messages.history", description: "Read a conversation you take part in (conversation_id), or list your conversations. Human conversations are private to their representative.",
			schema:  `{"type":"object","properties":{"conversation_id":{"type":"string"},"limit":{"type":"integer","minimum":1,"maximum":50},"cursor":{"type":"string"}},"additionalProperties":false}`,
			allowed: always, handle: r.history},
	}
}

func displayName(org *compile.Manifest, key string) string {
	if s, ok := org.Seats[key]; ok {
		return s.DisplayName
	}
	return key
}

func (r *Registry) recipients(_ context.Context, c *Call) (any, error) {
	out := []runtimeapi.RecipientInfo{}
	for _, e := range c.Seat.Manifest.SendTo {
		out = append(out, runtimeapi.RecipientInfo{Seat: e.Seat, DisplayName: displayName(&c.Org.Manifest, e.Seat), CanReply: e.Reply})
	}
	return map[string]any{"recipients": out}, nil
}

func checkBody(body string) error {
	if strings.TrimSpace(body) == "" {
		return toolErr("invalid", "body is required")
	}
	if len(body) > maxMessageBody {
		return toolErr("invalid", "body exceeds %d bytes; put large content in memory and reference it", maxMessageBody)
	}
	return nil
}

func (r *Registry) send(ctx context.Context, c *Call) (any, error) {
	var a struct {
		To             string `json:"to"`
		Body           string `json:"body"`
		ConversationID string `json:"conversation_id"`
		CorrelationID  string `json:"correlation_id"`
		Wake           *bool  `json:"wake"`
	}
	if err := decode(c, &a); err != nil {
		return nil, err
	}
	if err := checkBody(a.Body); err != nil {
		return nil, err
	}
	edge, ok := policy.SendEdge(&c.Seat.Manifest, a.To)
	if !ok {
		r.d.Metrics.MessagesRejected.WithLabelValues("no_route").Inc()
		return nil, toolErr("forbidden", "no declared route to seat %q", a.To)
	}
	to, err := r.d.Store.SeatByKey(ctx, c.Seat.OrganizationID, a.To)
	if err != nil {
		return nil, toolErr("unavailable", "seat %q is not active", a.To)
	}
	return r.deliver(ctx, c, to, store.SeatMessage{ConversationID: a.ConversationID, CorrelationID: a.CorrelationID,
		Route: strings.Join(edge.Routes, ","), Body: a.Body, Passive: a.Wake != nil && !*a.Wake})
}

// deliver persists a seat-to-seat message and reports the recipient's
// state so a sender can explain delays (§9.3).
func (r *Registry) deliver(ctx context.Context, c *Call, to *store.Seat, msg store.SeatMessage) (any, error) {
	msg.OrganizationID, msg.SenderID, msg.RecipientID = c.Seat.OrganizationID, c.Seat.ID, to.ID
	sent, err := r.d.Store.SendSeatMessage(ctx, c.fence(), msg)
	if err != nil {
		if errors.Is(err, store.ErrInboxFull) {
			r.d.Metrics.MessagesRejected.WithLabelValues("inbox_full").Inc()
		}
		return nil, err
	}
	r.d.Metrics.MessagesAccepted.WithLabelValues(store.OriginSeat).Inc()
	r.d.Log.Info("message sent", "organization_id", c.Seat.OrganizationID, "seat_id", c.Seat.ID, "execution_id", c.ExecutionID,
		"message_id", sent.MessageID, "recipient_seat_id", to.ID, "correlation_id", sent.CorrelationID)
	out := map[string]any{"message_id": sent.MessageID, "conversation_id": sent.ConversationID, "correlation_id": sent.CorrelationID}
	if rt, err := r.d.Store.SeatRuntimes(ctx, c.Seat.OrganizationID, []string{to.Key}); err == nil {
		if s, ok := rt[to.Key]; ok {
			out["recipient_state"], out["recipient_pending"] = s.State, s.PendingDeliveries
		}
	}
	return out, nil
}

func (r *Registry) reply(ctx context.Context, c *Call) (any, error) {
	var a struct {
		MessageID string `json:"message_id"`
		Binding   string `json:"binding"`
		Body      string `json:"body"`
		Wake      *bool  `json:"wake"`
	}
	if err := decode(c, &a); err != nil {
		return nil, err
	}
	if err := checkBody(a.Body); err != nil {
		return nil, err
	}
	switch {
	case (a.MessageID == "") == (a.Binding == ""):
		return nil, toolErr("invalid", "give exactly one of message_id or binding")
	case a.Binding != "":
		return r.replyHuman(ctx, c, a.Binding, "", "", a.Body)
	}
	m, err := r.d.Store.Message(ctx, c.Seat.OrganizationID, a.MessageID)
	if err != nil || m.RecipientSeatID != c.Seat.ID {
		return nil, toolErr("not_found", "no message %s was delivered to this seat", a.MessageID)
	}
	switch m.Origin {
	case store.OriginHuman:
		if !slices.Contains(m.Participants, c.Seat.ID) {
			return nil, toolErr("forbidden", "not a participant of this conversation")
		}
		return r.replyHuman(ctx, c, m.Binding, m.ConversationID, m.ID, a.Body)
	case store.OriginSeat:
		if !policy.CanReplyToSeat(&c.Seat.Manifest, m.SenderSeat) {
			r.d.Metrics.MessagesRejected.WithLabelValues("no_reply_route").Inc()
			return nil, toolErr("forbidden", "no route permits a reply to seat %q", m.SenderSeat)
		}
		to, err := r.d.Store.SeatByKey(ctx, c.Seat.OrganizationID, m.SenderSeat)
		if err != nil || to.ID != m.SenderSeatID {
			return nil, toolErr("unavailable", "the sending seat is no longer active")
		}
		return r.deliver(ctx, c, to, store.SeatMessage{ConversationID: m.ConversationID, ParentID: m.ID,
			CorrelationID: m.CorrelationID, Route: "reply", Body: a.Body, Passive: a.Wake != nil && !*a.Wake})
	default:
		return nil, toolErr("invalid", "%s messages cannot be replied to", m.Origin)
	}
}

// replyHuman sends to the human of binding. Only the representative bound to
// the binding may do so.
func (r *Registry) replyHuman(ctx context.Context, c *Call, binding, convID, parentID, body string) (any, error) {
	b, ok := c.Org.Manifest.Spec.ChannelBindings[binding]
	if !ok || !policy.HasBinding(&c.Seat.Manifest, binding) {
		r.d.Metrics.MessagesRejected.WithLabelValues("not_bound").Inc()
		return nil, toolErr("forbidden", "this seat is not the representative for binding %q", binding)
	}
	sent, err := r.d.Store.ReplyToHuman(ctx, c.fence(), store.HumanReply{OrganizationID: c.Seat.OrganizationID, SeatID: c.Seat.ID,
		Binding: binding, Connection: b.Connection, ConversationID: convID, ParentID: parentID, Body: body})
	if err != nil {
		return nil, err
	}
	r.d.Log.Info("reply to human queued", "organization_id", c.Seat.OrganizationID, "seat_id", c.Seat.ID,
		"execution_id", c.ExecutionID, "message_id", sent.MessageID, "binding", binding)
	return map[string]any{"message_id": sent.MessageID, "conversation_id": sent.ConversationID,
		"correlation_id": sent.CorrelationID, "delivery": "queued"}, nil
}

type historyMessage struct {
	store.Message
	Truncated bool `json:"truncated,omitempty"`
}

func (r *Registry) history(ctx context.Context, c *Call) (any, error) {
	var a struct {
		ConversationID string `json:"conversation_id"`
		Limit          int    `json:"limit"`
		Cursor         string `json:"cursor"`
	}
	if err := decode(c, &a); err != nil {
		return nil, err
	}
	offset, limit, err := page(a.Cursor, a.Limit, 20, 50)
	if err != nil {
		return nil, err
	}
	org := c.Seat.OrganizationID
	if a.ConversationID == "" {
		convs, err := r.d.Store.Conversations(ctx, org, c.Seat.ID, offset, limit+1)
		if err != nil {
			return nil, err
		}
		more := len(convs) > limit
		convs = convs[:min(len(convs), limit)]
		return fit(convs, offset, more, func(v []store.ConversationSummary, next string) any {
			return map[string]any{"conversations": nonNilSlice(v), "next_cursor": next}
		}), nil
	}
	kind, binding, participants, err := r.d.Store.Conversation(ctx, org, a.ConversationID)
	// A human conversation is private history of the representative bound to
	// it (A05); everything else is visible to participants only.
	if err != nil || !slices.Contains(participants, c.Seat.ID) ||
		(kind == store.ConvHuman && !policy.HasBinding(&c.Seat.Manifest, binding)) {
		return nil, toolErr("not_found", "conversation not found or not accessible")
	}
	msgs, err := r.d.Store.ConversationMessages(ctx, org, a.ConversationID, offset, limit+1)
	if err != nil {
		return nil, err
	}
	more := len(msgs) > limit
	out := make([]historyMessage, 0, min(len(msgs), limit))
	for _, m := range msgs[:min(len(msgs), limit)] {
		hm := historyMessage{Message: m}
		hm.Body, hm.Truncated = truncate(m.Body, historyBody)
		out = append(out, hm)
	}
	return fit(out, offset, more, func(v []historyMessage, next string) any {
		return map[string]any{"conversation_id": a.ConversationID, "kind": kind, "messages": v, "next_cursor": next}
	}), nil
}

func (r *Registry) inbox(ctx context.Context, c *Call) (any, error) {
	var a struct {
		MaxResults int `json:"max_results"`
	}
	if err := decode(c, &a); err != nil {
		return nil, err
	}
	limit := a.MaxResults
	if limit <= 0 || limit > store.MaxPassivePerTurn {
		limit = store.MaxPassivePerTurn
	}
	msgs, err := r.d.Store.TakePassive(ctx, c.fence(), c.ExecutionID, limit)
	if err != nil {
		return nil, err
	}
	for i := range msgs {
		if len(msgs[i].Body) > historyBody {
			msgs[i].Body, _ = truncate(msgs[i].Body, historyBody)
		}
	}
	return map[string]any{"messages": nonNilSlice(msgs)}, nil
}
