package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const (
	OriginExternalAgent      = "external_agent"
	ConvExternal             = "external"
	MaxExternalMessageBody   = 12 << 10
	MaxExternalPageSize      = 20
	maxExternalStatusReplies = 10
	// Allow room for the JSON-RPC response and for escaping a text result.
	maxExternalMessagePageBytes = 120 << 10
)

// ExternalPrincipal is assigned by trusted server authentication, never by
// external tool arguments. Every operation resolves the live representative
// and scopes retained records to its immutable seat identity and this caller.
type ExternalPrincipal struct {
	OrganizationID        string
	RepresentativeSeatKey string
	CallerID              string
}

// ExternalMessage is untrusted input for one representative. An empty
// ConversationID starts a dedicated conversation. IdempotencyKey is required.
type ExternalMessage struct {
	Body           string
	ConversationID string
	IdempotencyKey string
}

// ExternalConversationMessage intentionally contains no internal route,
// execution, human binding, or swarm information.
type ExternalConversationMessage struct {
	MessageID        string    `json:"message_id"`
	Origin           string    `json:"origin"`
	ExternalCallerID string    `json:"external_caller_id,omitempty"`
	ParentID         string    `json:"parent_id,omitempty"`
	Body             string    `json:"body"`
	CreatedAt        time.Time `json:"created_at"`
}

// ExternalRequest identifies the durable inbound message. Status reflects its
// delivery lifecycle, not whether a representative has already sent a reply.
// Replies are bounded; read the conversation to page through longer histories.
type ExternalRequest struct {
	RequestID      string                        `json:"request_id"`
	ConversationID string                        `json:"conversation_id"`
	Status         string                        `json:"status"`
	Duplicate      bool                          `json:"duplicate,omitempty"`
	Replies        []ExternalConversationMessage `json:"replies"`
	HasMoreReplies bool                          `json:"has_more_replies"`
}

type ExternalConversation struct {
	ConversationID string                        `json:"conversation_id"`
	Messages       []ExternalConversationMessage `json:"messages"`
	HasMore        bool                          `json:"has_more"`
	NextOffset     int                           `json:"next_offset,omitempty"`
}

func validExternalLabel(s string) bool {
	return strings.TrimSpace(s) != "" && len(s) <= 256 && utf8.ValidString(s) && !strings.ContainsFunc(s, unicode.IsControl)
}

func validExternalBody(body string) bool {
	return strings.TrimSpace(body) != "" && len(body) <= MaxExternalMessageBody && utf8.ValidString(body) && !strings.ContainsRune(body, 0)
}

// externalRepresentative locks the live manifest and identity for the whole
// operation. Locks follow organization -> seat, as organization sync does.
// Writes exclusively lock the recipient so cap checks and concurrent retries
// serialize with all other external sends to that representative.
func externalRepresentative(ctx context.Context, tx pgx.Tx, p ExternalPrincipal, write bool) (string, error) {
	if _, err := uuid.Parse(p.OrganizationID); err != nil || !validExternalLabel(p.CallerID) || p.RepresentativeSeatKey == "" {
		return "", ErrNotFound
	}
	var org string
	if err := tx.QueryRow(ctx, `SELECT id FROM organizations WHERE id = $1 AND deleted_at IS NULL
        AND manifest->'seats'->$2->>'is_representative' = 'true' FOR SHARE`, p.OrganizationID, p.RepresentativeSeatKey).Scan(&org); err != nil {
		return "", notFound(err)
	}
	lock := " FOR SHARE"
	if write {
		lock = " FOR UPDATE"
	}
	var id string
	err := tx.QueryRow(ctx, `SELECT id FROM seats WHERE organization_id = $1 AND key = $2
        AND retired_at IS NULL AND retiring_at IS NULL AND manifest->>'is_representative' = 'true'`+lock,
		p.OrganizationID, p.RepresentativeSeatKey).Scan(&id)
	return id, notFound(err)
}

func externalConversationAccess(ctx context.Context, tx pgx.Tx, p ExternalPrincipal, seatID, conversationID string) error {
	if _, err := uuid.Parse(conversationID); err != nil {
		return ErrNotFound
	}
	var id string
	err := tx.QueryRow(ctx, `SELECT e.conversation_id FROM external_conversations e JOIN conversations c ON c.id = e.conversation_id
        WHERE e.conversation_id = $1 AND e.organization_id = $2 AND e.representative_seat_id = $3 AND e.caller_id = $4
        AND c.organization_id = e.organization_id AND c.kind = 'external' AND c.participants = ARRAY[$3::uuid]`,
		conversationID, p.OrganizationID, seatID, p.CallerID).Scan(&id)
	return notFound(err)
}

// SendExternalMessage atomically persists untrusted external input and the
// ordinary runtime delivery. Duplicate retries return the original request;
// reusing a key with changed body or conversation fails with ErrConflict.
func (s *Store) SendExternalMessage(ctx context.Context, p ExternalPrincipal, in ExternalMessage) (*ExternalRequest, error) {
	if !validExternalBody(in.Body) || !validExternalLabel(in.IdempotencyKey) {
		return nil, ErrInvalid
	}
	if in.ConversationID != "" {
		parsed, err := uuid.Parse(in.ConversationID)
		if err != nil {
			return nil, ErrNotFound
		}
		in.ConversationID = parsed.String()
	}
	var out *ExternalRequest
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		seatID, err := externalRepresentative(ctx, tx, p, true)
		if err != nil {
			return err
		}
		var id, body, requested string
		err = tx.QueryRow(ctx, `SELECT r.message_id, m.body, COALESCE(r.requested_conversation_id::text, '')
            FROM external_requests r JOIN messages m ON m.id = r.message_id
            WHERE r.organization_id = $1 AND r.representative_seat_id = $2 AND r.caller_id = $3 AND r.idempotency_key = $4`,
			p.OrganizationID, seatID, p.CallerID, in.IdempotencyKey).Scan(&id, &body, &requested)
		if err == nil {
			if body != in.Body || requested != in.ConversationID {
				return ErrConflict
			}
			out, err = externalRequest(ctx, tx, p, seatID, id)
			if err == nil {
				out.Duplicate = true
			}
			return err
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		conversationID := in.ConversationID
		if conversationID == "" {
			conversationID = uuid.NewString()
			if _, err := tx.Exec(ctx, `INSERT INTO conversations (id, organization_id, kind, participants)
                VALUES ($1, $2, 'external', ARRAY[$3::uuid])`, conversationID, p.OrganizationID, seatID); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO external_conversations (conversation_id, organization_id, representative_seat_id, caller_id)
                VALUES ($1, $2, $3, $4)`, conversationID, p.OrganizationID, seatID, p.CallerID); err != nil {
				return err
			}
		} else if err := externalConversationAccess(ctx, tx, p, seatID, conversationID); err != nil {
			return err
		}
		id = uuid.NewString()
		if _, err := tx.Exec(ctx, `INSERT INTO messages (id, organization_id, conversation_id, origin, origin_external_caller,
            recipient_seat_id, correlation_id, body, route, external_sequence, created_at)
            VALUES ($1, $2, $3, 'external_agent', $4, $5, $1, $6, 'external', nextval('external_message_sequence'), clock_timestamp())`,
			id, p.OrganizationID, conversationID, p.CallerID, seatID, in.Body); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO external_requests (message_id, organization_id, representative_seat_id, caller_id,
            idempotency_key, requested_conversation_id) VALUES ($1, $2, $3, $4, $5, NULLIF($6, '')::uuid)`,
			id, p.OrganizationID, seatID, p.CallerID, in.IdempotencyKey, in.ConversationID); err != nil {
			return err
		}
		if _, err := insertDelivery(ctx, tx, id, seatID, true, false); err != nil {
			return err
		}
		out = &ExternalRequest{RequestID: id, ConversationID: conversationID, Status: "queued", Replies: []ExternalConversationMessage{}}
		return nil
	})
	return out, err
}

// ExternalRequestStatus reveals only a request belonging to this authenticated
// caller and the currently active representative identity.
func (s *Store) ExternalRequestStatus(ctx context.Context, p ExternalPrincipal, requestID string) (*ExternalRequest, error) {
	parsed, err := uuid.Parse(requestID)
	if err != nil {
		return nil, ErrNotFound
	}
	requestID = parsed.String()
	var out *ExternalRequest
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		seatID, err := externalRepresentative(ctx, tx, p, false)
		if err != nil {
			return err
		}
		out, err = externalRequest(ctx, tx, p, seatID, requestID)
		return err
	})
	return out, err
}

func externalRequest(ctx context.Context, tx pgx.Tx, p ExternalPrincipal, seatID, requestID string) (*ExternalRequest, error) {
	out := &ExternalRequest{Replies: []ExternalConversationMessage{}}
	var delivery, execution string
	err := tx.QueryRow(ctx, `SELECT r.message_id, m.conversation_id, d.state, COALESCE(x.state, '')
        FROM external_requests r JOIN messages m ON m.id = r.message_id
        JOIN external_conversations ec ON ec.conversation_id = m.conversation_id
        JOIN deliveries d ON d.message_id = m.id AND d.seat_id = r.representative_seat_id
        LEFT JOIN executions x ON x.id = d.execution_id
        WHERE r.message_id = $1 AND r.organization_id = $2 AND r.representative_seat_id = $3 AND r.caller_id = $4
        AND ec.organization_id = r.organization_id AND ec.representative_seat_id = r.representative_seat_id AND ec.caller_id = r.caller_id`,
		requestID, p.OrganizationID, seatID, p.CallerID).Scan(&out.RequestID, &out.ConversationID, &delivery, &execution)
	if err != nil {
		return nil, notFound(err)
	}
	switch delivery {
	case "pending", "passive":
		out.Status = "queued"
	case "leased":
		out.Status = "running"
	case "done":
		if execution == "running" {
			out.Status = "running"
		} else if execution == "completed" {
			out.Status = "completed"
		} else {
			out.Status = "failed"
		}
	default:
		out.Status = "failed"
	}
	out.Replies, err = externalMessages(ctx, tx, p, seatID, out.ConversationID, requestID, 0, maxExternalStatusReplies+1)
	n := externalPageCount(out.Replies, maxExternalStatusReplies)
	out.HasMoreReplies = len(out.Replies) > n
	out.Replies = out.Replies[:n]
	return out, err
}

// ReadExternalConversation returns only the caller's submitted messages and
// replies deliberately sent by its representative to this conversation.
// External writes are serialized per representative and ordered by a database
// sequence allocated under that lock, so later commits cannot backfill an
// earlier page even when transactions start out of order or the clock changes.
func (s *Store) ReadExternalConversation(ctx context.Context, p ExternalPrincipal, conversationID string, offset, limit int) (*ExternalConversation, error) {
	if offset < 0 || limit < 1 || limit > MaxExternalPageSize {
		return nil, ErrInvalid
	}
	parsed, err := uuid.Parse(conversationID)
	if err != nil {
		return nil, ErrNotFound
	}
	conversationID = parsed.String()
	var out *ExternalConversation
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		seatID, err := externalRepresentative(ctx, tx, p, false)
		if err != nil {
			return err
		}
		if err := externalConversationAccess(ctx, tx, p, seatID, conversationID); err != nil {
			return err
		}
		msgs, err := externalMessages(ctx, tx, p, seatID, conversationID, "", offset, limit+1)
		if err != nil {
			return err
		}
		n := externalPageCount(msgs, limit)
		out = &ExternalConversation{ConversationID: conversationID, HasMore: len(msgs) > n, Messages: msgs[:n]}
		if out.HasMore {
			out.NextOffset = offset + n
		}
		return nil
	})
	return out, err
}

// externalPageCount counts complete messages fitting the encoded response
// budget. Bodies are never truncated; the caller can fetch the next page.
func externalPageCount(messages []ExternalConversationMessage, limit int) int {
	bytes := 0
	for i, m := range messages[:min(len(messages), limit)] {
		raw, _ := json.Marshal(m)
		if bytes+len(raw)+1 > maxExternalMessagePageBytes {
			return i
		}
		bytes += len(raw) + 1
	}
	return min(len(messages), limit)
}

func externalMessages(ctx context.Context, tx pgx.Tx, p ExternalPrincipal, seatID, conversationID, requestID string, offset, limit int) ([]ExternalConversationMessage, error) {
	rows, err := tx.Query(ctx, `SELECT m.id, m.origin, m.origin_external_caller, COALESCE(m.parent_id::text, ''), m.body, m.created_at
        FROM messages m WHERE m.organization_id = $1 AND m.conversation_id = $2
        AND ((m.origin = 'external_agent' AND m.origin_external_caller = $3 AND m.recipient_seat_id = $4)
          OR (m.origin = 'seat' AND m.sender_seat_id = $4 AND m.recipient_seat_id IS NULL AND m.recipient_binding = '' AND m.route = 'external'))
        AND ($5 = '' OR (m.origin = 'seat' AND m.parent_id = NULLIF($5, '')::uuid))
        ORDER BY m.external_sequence, m.id OFFSET $6 LIMIT $7`, p.OrganizationID, conversationID, p.CallerID, seatID, requestID, offset, limit)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (ExternalConversationMessage, error) {
		var m ExternalConversationMessage
		err := r.Scan(&m.MessageID, &m.Origin, &m.ExternalCallerID, &m.ParentID, &m.Body, &m.CreatedAt)
		return m, err
	})
	if out == nil {
		out = []ExternalConversationMessage{}
	}
	return out, err
}

// ExternalReply is an explicit representative reply to one external inbound
// message. There is no connector outbox or human binding involved.
type ExternalReply struct {
	OrganizationID string
	SeatID         string
	ParentID       string
	Body           string
}

func (s *Store) ReplyToExternal(ctx context.Context, f Fence, in ExternalReply) (*Sent, error) {
	if !validExternalBody(in.Body) {
		return nil, ErrInvalid
	}
	if f.SeatID != in.SeatID {
		return nil, ErrFenced
	}
	if _, err := uuid.Parse(in.ParentID); err != nil {
		return nil, ErrNotFound
	}
	out := &Sent{MessageID: uuid.NewString()}
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		// The parent supplies the caller scope; never trust a caller ID from
		// the runtime's tool arguments to choose the destination.
		var p ExternalPrincipal
		p.OrganizationID = in.OrganizationID
		err := tx.QueryRow(ctx, `SELECT s.key, ec.caller_id FROM messages m
            JOIN external_requests er ON er.message_id = m.id
            JOIN external_conversations ec ON ec.conversation_id = m.conversation_id
            JOIN seats s ON s.id = ec.representative_seat_id
            WHERE m.id = $1 AND m.organization_id = $2 AND m.origin = 'external_agent' AND m.recipient_seat_id = $3
            AND ec.organization_id = $2 AND ec.representative_seat_id = $3
            AND er.organization_id = $2 AND er.representative_seat_id = $3 AND er.caller_id = ec.caller_id`,
			in.ParentID, in.OrganizationID, in.SeatID).Scan(&p.RepresentativeSeatKey, &p.CallerID)
		if err != nil {
			return notFound(err)
		}
		seatID, err := externalRepresentative(ctx, tx, p, true)
		if err != nil {
			return err
		}
		if seatID != in.SeatID {
			return ErrNotFound
		}
		if err := checkFence(ctx, tx, f); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT conversation_id, COALESCE(correlation_id, id) FROM messages WHERE id = $1`, in.ParentID).
			Scan(&out.ConversationID, &out.CorrelationID); err != nil {
			return notFound(err)
		}
		if err := externalConversationAccess(ctx, tx, p, seatID, out.ConversationID); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO messages (id, organization_id, conversation_id, origin, sender_seat_id,
            parent_id, correlation_id, body, route, sender_generation, external_sequence, created_at)
            VALUES ($1, $2, $3, 'seat', $4, $5, $6, $7, 'external', $8, nextval('external_message_sequence'), clock_timestamp())`, out.MessageID, in.OrganizationID,
			out.ConversationID, in.SeatID, in.ParentID, out.CorrelationID, in.Body, f.Generation)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
