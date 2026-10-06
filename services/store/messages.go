package store

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Message origins. They are assigned by trusted code only (§9.2).
const (
	OriginHuman    = "human"
	OriginSeat     = "seat"
	OriginSystem   = "system"
	OriginSchedule = "schedule"
	OriginProbe    = "probe"
)

// Conversation kinds.
const (
	ConvHuman    = "human"
	ConvInternal = "internal"
	ConvSystem   = "system"
)

// Message is a stored message with its conversation context.
type Message struct {
	ID               string    `json:"message_id"`
	ConversationID   string    `json:"conversation_id"`
	ConversationKind string    `json:"-"`
	Binding          string    `json:"binding,omitempty"`
	Participants     []string  `json:"-"`
	Origin           string    `json:"origin"`
	SenderSeatID     string    `json:"-"`
	SenderSeat       string    `json:"sender_seat,omitempty"`
	RecipientSeatID  string    `json:"-"`
	RecipientSeat    string    `json:"recipient_seat,omitempty"`
	RecipientBinding string    `json:"recipient_binding,omitempty"`
	ParentID         string    `json:"parent_id,omitempty"`
	CorrelationID    string    `json:"correlation_id,omitempty"`
	Body             string    `json:"body"`
	CreatedAt        time.Time `json:"created_at"`
}

const messageColumns = `m.id, m.conversation_id, c.kind, c.binding, c.participants::text[], m.origin,
	COALESCE(m.sender_seat_id::text, ''), COALESCE(ss.key, ''), COALESCE(m.recipient_seat_id::text, ''), COALESCE(rs.key, ''),
	m.recipient_binding, COALESCE(m.parent_id::text, ''), COALESCE(m.correlation_id::text, ''), m.body, m.created_at
	FROM messages m JOIN conversations c ON c.id = m.conversation_id
	LEFT JOIN seats ss ON ss.id = m.sender_seat_id LEFT JOIN seats rs ON rs.id = m.recipient_seat_id`

func scanMessage(row pgx.Row) (*Message, error) {
	m := &Message{}
	err := row.Scan(&m.ID, &m.ConversationID, &m.ConversationKind, &m.Binding, &m.Participants, &m.Origin,
		&m.SenderSeatID, &m.SenderSeat, &m.RecipientSeatID, &m.RecipientSeat, &m.RecipientBinding,
		&m.ParentID, &m.CorrelationID, &m.Body, &m.CreatedAt)
	if err != nil {
		return nil, notFound(err)
	}
	return m, nil
}

// Message loads a message of the organisation.
func (s *Store) Message(ctx context.Context, orgID, id string) (*Message, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, ErrNotFound
	}
	return scanMessage(s.pool.QueryRow(ctx, `SELECT `+messageColumns+` WHERE m.organization_id = $1 AND m.id = $2`, orgID, id))
}

// Sent identifies an accepted message.
type Sent struct {
	MessageID      string `json:"message_id"`
	ConversationID string `json:"conversation_id"`
	CorrelationID  string `json:"correlation_id"`
}

// SeatMessage is an internal message between seats. Route authorisation is
// the caller's responsibility; the store enforces conversation membership.
type SeatMessage struct {
	OrganizationID string
	SenderID       string
	RecipientID    string
	// ConversationID continues an internal conversation the sender takes part in.
	ConversationID string
	ParentID       string
	CorrelationID  string
	Route          string
	Body           string
}

// SendSeatMessage persists an internal message and its delivery in one
// fenced transaction. A full recipient inbox is rejected (ADR-0006).
func (s *Store) SendSeatMessage(ctx context.Context, f Fence, in SeatMessage) (*Sent, error) {
	out := &Sent{MessageID: uuid.NewString(), CorrelationID: in.CorrelationID}
	if out.CorrelationID == "" {
		out.CorrelationID = out.MessageID
	} else if _, err := uuid.Parse(out.CorrelationID); err != nil {
		return nil, ErrInvalid
	}
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		if err := checkFence(ctx, tx, f); err != nil {
			return err
		}
		if in.ConversationID != "" {
			if _, err := uuid.Parse(in.ConversationID); err != nil {
				return ErrNotFound
			}
			tag, err := tx.Exec(ctx, `UPDATE conversations SET participants = CASE WHEN $4::uuid = ANY(participants)
					THEN participants ELSE array_append(participants, $4::uuid) END
				WHERE id = $1 AND organization_id = $2 AND kind = 'internal' AND $3::uuid = ANY(participants)`,
				in.ConversationID, in.OrganizationID, in.SenderID, in.RecipientID)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				return ErrNotFound
			}
			out.ConversationID = in.ConversationID
		} else {
			out.ConversationID = uuid.NewString()
			if _, err := tx.Exec(ctx, `INSERT INTO conversations (id, organization_id, kind, opened_by, participants)
				VALUES ($1, $2, 'internal', $3, ARRAY[$3::uuid, $4::uuid])`, out.ConversationID, in.OrganizationID, in.SenderID, in.RecipientID); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `INSERT INTO messages (id, organization_id, conversation_id, origin, sender_seat_id,
				recipient_seat_id, parent_id, correlation_id, body, route, sender_generation)
			VALUES ($1, $2, $3, 'seat', $4, $5, NULLIF($6, '')::uuid, $7, $8, $9, $10)`,
			out.MessageID, in.OrganizationID, out.ConversationID, in.SenderID, in.RecipientID, in.ParentID,
			out.CorrelationID, in.Body, in.Route, f.Generation); err != nil {
			return err
		}
		_, err := insertDelivery(ctx, tx, out.MessageID, in.RecipientID, true)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// HumanReply is an outbound message from a representative to its bound human.
type HumanReply struct {
	OrganizationID string
	SeatID         string
	Binding        string
	Connection     string
	// ConversationID is the human conversation replied to; when empty the
	// representative's latest conversation for the binding is used.
	ConversationID string
	ParentID       string
	Body           string
}

// ReplyToHuman writes the outbound message and its outbox row in one fenced
// transaction; the dispatcher delivers it (§9.2).
func (s *Store) ReplyToHuman(ctx context.Context, f Fence, in HumanReply) (*Sent, error) {
	out := &Sent{MessageID: uuid.NewString()}
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		if err := checkFence(ctx, tx, f); err != nil {
			return err
		}
		convID := in.ConversationID
		if convID == "" {
			err := tx.QueryRow(ctx, `SELECT id FROM conversations WHERE organization_id = $1 AND kind = 'human' AND binding = $2
				AND $3::uuid = ANY(participants) ORDER BY created_at DESC LIMIT 1`, in.OrganizationID, in.Binding, in.SeatID).Scan(&convID)
			if errors.Is(err, pgx.ErrNoRows) {
				convID, err = humanConversation(ctx, tx, in.OrganizationID, in.Binding, "", in.SeatID)
			}
			if err != nil {
				return err
			}
		}
		out.ConversationID = convID
		var corr *string
		if in.ParentID != "" {
			if err := tx.QueryRow(ctx, `SELECT correlation_id::text FROM messages WHERE id = $1`, in.ParentID).Scan(&corr); err != nil {
				return notFound(err)
			}
		}
		if corr == nil {
			corr = &out.MessageID
		}
		out.CorrelationID = *corr
		if _, err := tx.Exec(ctx, `INSERT INTO messages (id, organization_id, conversation_id, origin, sender_seat_id,
				recipient_binding, parent_id, correlation_id, body, route, sender_generation)
			VALUES ($1, $2, $3, 'seat', $4, $5, NULLIF($6, '')::uuid, $7, $8, $9, $10)`,
			out.MessageID, in.OrganizationID, convID, in.SeatID, in.Binding, in.ParentID, out.CorrelationID, in.Body,
			"binding:"+in.Binding, f.Generation); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO outbox (organization_id, message_id, connection) VALUES ($1, $2, $3)`,
			in.OrganizationID, out.MessageID, in.Connection)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// humanConversation returns the conversation for (binding, external ref) in
// which repID takes part. If the existing conversation belongs to another
// identity (the binding moved, or the seat was retired and recreated) it is
// archived under a different ref and a fresh one is started, so a new
// identity never inherits another's private history (A05, A18).
func humanConversation(ctx context.Context, tx pgx.Tx, orgID, binding, ref, repID string) (string, error) {
	for range 2 {
		var id string
		var participants []string
		err := tx.QueryRow(ctx, `SELECT id, participants::text[] FROM conversations
			WHERE organization_id = $1 AND kind = 'human' AND binding = $2 AND external_ref = $3 FOR UPDATE`,
			orgID, binding, ref).Scan(&id, &participants)
		switch {
		case err == nil && slices.Contains(participants, repID):
			return id, nil
		case err == nil:
			if _, err := tx.Exec(ctx, `UPDATE conversations SET external_ref = external_ref || '@archived:' || id::text WHERE id = $1`, id); err != nil {
				return "", err
			}
		case !errors.Is(err, pgx.ErrNoRows):
			return "", err
		}
		id = uuid.NewString()
		tag, err := tx.Exec(ctx, `INSERT INTO conversations (id, organization_id, kind, binding, external_ref, participants)
			VALUES ($1, $2, 'human', $3, $4, ARRAY[$5::uuid])
			ON CONFLICT (organization_id, binding, external_ref) WHERE kind = 'human' DO NOTHING`, id, orgID, binding, ref, repID)
		if err != nil {
			return "", err
		}
		if tag.RowsAffected() == 1 {
			return id, nil
		}
	}
	return "", errors.New("conversation contention")
}

// HumanMessage is a verified inbound event for a representative.
type HumanMessage struct {
	OrganizationID string
	Connection     string
	Binding        string
	ExternalUserID string
	// ExternalRef identifies the external conversation (channel and thread).
	ExternalRef      string
	EventID          string
	RepresentativeID string
	Body             string
}

// Ingested is the outcome of IngestHuman.
type Ingested struct {
	MessageID string
	Duplicate bool
	// OverCap reports that the representative's inbox exceeded its cap. The
	// message is still accepted: accepted human messages are never dropped.
	OverCap bool
}

// IngestHuman persists an inbound human message and its delivery. Events are
// deduplicated on (organisation, connection, external event id) (A09).
func (s *Store) IngestHuman(ctx context.Context, in HumanMessage) (*Ingested, error) {
	out := &Ingested{MessageID: uuid.NewString()}
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		convID, err := humanConversation(ctx, tx, in.OrganizationID, in.Binding, in.ExternalRef, in.RepresentativeID)
		if err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `INSERT INTO messages (id, organization_id, conversation_id, origin, origin_connection,
				origin_external_user, recipient_seat_id, external_event_id, body, route)
			VALUES ($1, $2, $3, 'human', $4, $5, $6, $7, $8, $9)
			ON CONFLICT (organization_id, origin_connection, external_event_id) WHERE external_event_id IS NOT NULL DO NOTHING`,
			out.MessageID, in.OrganizationID, convID, in.Connection, in.ExternalUserID, in.RepresentativeID, in.EventID,
			in.Body, "binding:"+in.Binding)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			out.Duplicate = true
			return tx.QueryRow(ctx, `SELECT id FROM messages WHERE organization_id = $1 AND origin_connection = $2 AND external_event_id = $3`,
				in.OrganizationID, in.Connection, in.EventID).Scan(&out.MessageID)
		}
		out.OverCap, err = insertDelivery(ctx, tx, out.MessageID, in.RepresentativeID, false)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ExternalRef encodes an external channel and thread as a conversation ref.
func ExternalRef(channel, thread string) string {
	if thread == "" {
		return channel
	}
	return channel + "#" + thread
}

// SplitExternalRef reverses ExternalRef.
func SplitExternalRef(ref string) (channel, thread string) {
	channel, thread, _ = strings.Cut(ref, "#")
	return channel, thread
}

// ConversationSummary describes a conversation the seat takes part in.
type ConversationSummary struct {
	ID        string    `json:"conversation_id"`
	Kind      string    `json:"kind"`
	Binding   string    `json:"binding,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	LastAt    time.Time `json:"last_message_at"`
}

// Conversations lists conversations the seat participates in, most recent first.
func (s *Store) Conversations(ctx context.Context, orgID, seatID string, offset, limit int) ([]ConversationSummary, error) {
	rows, err := s.pool.Query(ctx, `SELECT c.id, c.kind, c.binding, c.created_at, COALESCE(max(m.created_at), c.created_at) AS last_at
		FROM conversations c LEFT JOIN messages m ON m.conversation_id = c.id
		WHERE c.organization_id = $1 AND $2::uuid = ANY(c.participants)
		GROUP BY c.id ORDER BY last_at DESC, c.id OFFSET $3 LIMIT $4`, orgID, seatID, offset, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (ConversationSummary, error) {
		var c ConversationSummary
		err := r.Scan(&c.ID, &c.Kind, &c.Binding, &c.CreatedAt, &c.LastAt)
		return c, err
	})
}

// Conversation returns a conversation's kind, binding and participants.
func (s *Store) Conversation(ctx context.Context, orgID, id string) (kind, binding string, participants []string, err error) {
	if _, err := uuid.Parse(id); err != nil {
		return "", "", nil, ErrNotFound
	}
	err = s.pool.QueryRow(ctx, `SELECT kind, binding, participants::text[] FROM conversations WHERE organization_id = $1 AND id = $2`,
		orgID, id).Scan(&kind, &binding, &participants)
	return kind, binding, participants, notFound(err)
}

// ConversationMessages pages a conversation's messages, oldest first.
func (s *Store) ConversationMessages(ctx context.Context, orgID, convID string, offset, limit int) ([]Message, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+messageColumns+` WHERE m.organization_id = $1 AND m.conversation_id = $2
		ORDER BY m.created_at, m.id OFFSET $3 LIMIT $4`, orgID, convID, offset, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}

// systemMessage queues a platform-originated message (probe or schedule) in a
// new system conversation. A non-empty dedupeKey makes it idempotent.
func systemMessage(ctx context.Context, tx pgx.Tx, orgID, seatID, origin, dedupeKey, body string) (string, bool, error) {
	if dedupeKey != "" {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM messages WHERE organization_id = $1 AND origin_connection = ''
			AND external_event_id = $2)`, orgID, dedupeKey).Scan(&exists); err != nil {
			return "", false, err
		}
		if exists {
			return "", false, nil
		}
	}
	convID, msgID := uuid.NewString(), uuid.NewString()
	if _, err := tx.Exec(ctx, `INSERT INTO conversations (id, organization_id, kind, participants) VALUES ($1, $2, 'system', ARRAY[$3::uuid])`,
		convID, orgID, seatID); err != nil {
		return "", false, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO messages (id, organization_id, conversation_id, origin, recipient_seat_id, external_event_id, body)
		VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''), $7)`, msgID, orgID, convID, origin, seatID, dedupeKey, body); err != nil {
		return "", false, err
	}
	if _, err := insertDelivery(ctx, tx, msgID, seatID, false); err != nil {
		return "", false, err
	}
	return msgID, true, nil
}
