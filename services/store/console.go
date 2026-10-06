package store

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/darcys22/steadmesh/pkg/compile"
	"github.com/darcys22/steadmesh/pkg/runtimeapi"
)

// Read-only queries behind the console API. None of them return secret
// values, memory record bodies or anything a seat could not already see about
// itself; they are organisation-wide because the console is an operator view.

// ConsoleOrganizations lists the active organisations' configuration.
func (s *Store) ConsoleOrganizations(ctx context.Context) ([]runtimeapi.ConsoleOrganization, error) {
	ids, err := s.ActiveOrganizations(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]runtimeapi.ConsoleOrganization, 0, len(ids))
	for _, id := range ids {
		o, err := s.ConsoleOrganization(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, *o)
	}
	return out, nil
}

// ConsoleOrganization returns an organisation's committed configuration with
// its connections' last verification results.
func (s *Store) ConsoleOrganization(ctx context.Context, orgID string) (*runtimeapi.ConsoleOrganization, error) {
	if _, err := uuid.Parse(orgID); err != nil {
		return nil, ErrNotFound
	}
	org, err := s.Organization(ctx, orgID)
	if err != nil {
		return nil, err
	}
	m := org.Manifest
	out := &runtimeapi.ConsoleOrganization{ID: org.ID, Key: org.Key, Namespace: org.Namespace,
		DisplayName: m.Spec.DisplayName, Revision: m.Digest, Teams: sortedKeys(m.Spec.Teams),
		Seats: []runtimeapi.ConsoleSeatConfig{}, Connections: []runtimeapi.ConsoleConnection{}}

	rows, err := s.pool.Query(ctx, `SELECT id, key, policy_revision, created_at, COALESCE(adopted_from::text, '')
		FROM seats WHERE organization_id = $1 AND retired_at IS NULL ORDER BY key`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, key, adopted string
		var policy int64
		var created time.Time
		if err := rows.Scan(&id, &key, &policy, &created, &adopted); err != nil {
			return nil, err
		}
		sm, ok := m.Seats[key]
		if !ok {
			continue
		}
		out.Seats = append(out.Seats, seatConfig(orgID, id, sm, policy, created, adopted))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	checks := map[string]runtimeapi.ConsoleCheck{}
	rows, err = s.pool.Query(ctx, `SELECT connection, ok, detail, checked_at FROM connection_checks WHERE organization_id = $1`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var conn string
		var c runtimeapi.ConsoleCheck
		if err := rows.Scan(&conn, &c.OK, &c.Detail, &c.CheckedAt); err != nil {
			return nil, err
		}
		checks[conn] = c
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, k := range sortedKeys(m.Spec.Connections) {
		c := m.Spec.Connections[k]
		cc := runtimeapi.ConsoleConnection{Key: k, Adapter: c.Adapter, Required: c.Required == nil || *c.Required}
		cc.SecretKind, cc.SecretPath, _ = strings.Cut(c.SecretRef, ":")
		if chk, ok := checks[k]; ok {
			cc.Check = &chk
		}
		out.Connections = append(out.Connections, cc)
	}
	for _, k := range sortedKeys(m.Spec.ChannelBindings) {
		b := m.Spec.ChannelBindings[k]
		out.Bindings = append(out.Bindings, runtimeapi.ConsoleChannelBinding{Key: k, Connection: b.Connection, Seat: b.Seat})
	}
	return out, nil
}

func seatConfig(orgID, id string, sm compile.SeatManifest, policy int64, created time.Time, adopted string) runtimeapi.ConsoleSeatConfig {
	c := runtimeapi.ConsoleSeatConfig{SeatID: id, OrganizationID: orgID, Key: sm.Key, DisplayName: sm.DisplayName, RoleRef: sm.RoleRef,
		Teams: sm.Teams, IsRepresentative: sm.IsRepresentative, HarnessAdapter: sm.Harness.Adapter, Model: sm.Harness.Model,
		ConfigRevision: sm.ConfigRevision, PolicyRevision: policy, PersonalMemory: sm.PersonalMemory,
		ChannelBindings: sm.ChannelBindings, PersistentWorkspace: sm.Workspace.Persistent,
		SharedWorkspaces: sm.Workspace.Shared, CreatedAt: created, AdoptedFrom: adopted}
	for _, e := range sm.SendTo {
		c.SendTo = append(c.SendTo, e.Seat)
	}
	for _, cap := range sm.Capabilities {
		c.Capabilities = append(c.Capabilities, runtimeapi.ConsoleCapability{Resource: cap.Resource, Operations: cap.Operations, Sources: cap.Sources})
	}
	return c
}

// ConsoleSeatStatuses reports the live state of the organisation's active
// seats, optionally restricted to the given keys, ordered by key.
func (s *Store) ConsoleSeatStatuses(ctx context.Context, orgID string, keys []string) ([]runtimeapi.ConsoleSeatStatus, error) {
	if _, err := uuid.Parse(orgID); err != nil {
		return nil, ErrNotFound
	}
	rts, err := s.SeatRuntimes(ctx, orgID, keys)
	if err != nil {
		return nil, err
	}
	byID := map[string]*runtimeapi.ConsoleSeatStatus{}
	out := make([]runtimeapi.ConsoleSeatStatus, 0, len(rts))
	for _, k := range sortedKeys(rts) {
		out = append(out, runtimeapi.ConsoleSeatStatus{SeatRuntime: rts[k]})
	}
	for i := range out {
		byID[out[i].SeatID] = &out[i]
	}

	rows, err := s.pool.Query(ctx, `SELECT DISTINCT ON (e.seat_id) `+executionColumns+`
		WHERE s.organization_id = $1 AND e.state = 'running' ORDER BY e.seat_id, e.started_at DESC`, orgID)
	if err != nil {
		return nil, err
	}
	running, err := pgx.CollectRows(rows, scanExecution)
	if err != nil {
		return nil, err
	}
	for _, e := range running {
		if st, ok := byID[e.SeatID]; ok {
			st.Current = &e
		}
	}

	rows, err = s.pool.Query(ctx, `SELECT e.seat_id::text, max(ev.created_at) FROM execution_events ev
		JOIN executions e ON e.id = ev.execution_id JOIN seats s ON s.id = e.seat_id
		WHERE s.organization_id = $1 AND ev.kind IN ('tool_result', 'output', 'completion') GROUP BY 1`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var at time.Time
		if err := rows.Scan(&id, &at); err != nil {
			return nil, err
		}
		if st, ok := byID[id]; ok {
			st.LastProgressAt = &at
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows, err = s.pool.Query(ctx, `SELECT seat_id::text, count(*) FROM connector_operations
		WHERE organization_id = $1 AND status = 'unknown' AND seat_id IS NOT NULL GROUP BY 1`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		if st, ok := byID[id]; ok {
			st.UnknownOperations = n
		}
	}
	return out, rows.Err()
}

// ConsoleSeat returns an active seat's configuration, live state, recovery
// state, memory metadata and recent runs.
func (s *Store) ConsoleSeat(ctx context.Context, seatID string, runs int) (*runtimeapi.ConsoleSeatDetail, error) {
	if _, err := uuid.Parse(seatID); err != nil {
		return nil, ErrNotFound
	}
	st, err := s.Seat(ctx, seatID)
	if err != nil {
		return nil, err
	}
	var created time.Time
	var adopted string
	if err := s.pool.QueryRow(ctx, `SELECT created_at, COALESCE(adopted_from::text, '') FROM seats WHERE id = $1`, seatID).
		Scan(&created, &adopted); err != nil {
		return nil, notFound(err)
	}
	out := &runtimeapi.ConsoleSeatDetail{Config: seatConfig(st.OrganizationID, st.ID, st.Manifest, st.PolicyRevision, created, adopted)}
	statuses, err := s.ConsoleSeatStatuses(ctx, st.OrganizationID, []string{st.Key})
	if err != nil {
		return nil, err
	}
	if len(statuses) == 1 {
		out.Status = statuses[0]
	}
	if out.Handoff, err = s.Handoff(ctx, seatID); err != nil {
		return nil, err
	}
	cps, err := s.Checkpoints(ctx, seatID)
	if err != nil {
		return nil, err
	}
	if len(cps) > 0 {
		out.Checkpoint = &cps[0]
	}

	var stores []string
	for _, c := range st.Manifest.Capabilities {
		if k, ok := strings.CutPrefix(c.Resource, "memory:"); ok {
			stores = append(stores, k)
		}
	}
	rows, err := s.pool.Query(ctx, `SELECT ms.key, ms.owner_seat_id IS NOT NULL,
			(SELECT count(*) FROM memory_records r WHERE r.store_id = ms.id AND NOT r.archived),
			(SELECT max(r.updated_at) FROM memory_records r WHERE r.store_id = ms.id)
		FROM memory_stores ms WHERE ms.organization_id = $1 AND ms.retired_at IS NULL AND ms.key = ANY($2) ORDER BY ms.key`,
		st.OrganizationID, nonNil(stores))
	if err != nil {
		return nil, err
	}
	out.Memory, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (runtimeapi.ConsoleMemoryStore, error) {
		var m runtimeapi.ConsoleMemoryStore
		err := r.Scan(&m.Key, &m.Personal, &m.Records, &m.LastUpdatedAt)
		return m, err
	})
	if err != nil {
		return nil, err
	}
	if out.Executions, err = s.ConsoleExecutions(ctx, seatID, time.Time{}, runs); err != nil {
		return nil, err
	}
	return out, nil
}

const executionColumns = `e.id, e.seat_id, s.key, e.lease_generation, e.config_revision, e.harness_adapter,
	COALESCE(e.trigger_message_id::text, ''), COALESCE(left(tm.body, 200), ''), COALESCE(tm.origin, ''),
	e.state, e.error, e.started_at, e.finished_at,
	(SELECT count(*) FROM execution_events ev WHERE ev.execution_id = e.id)
	FROM executions e JOIN seats s ON s.id = e.seat_id LEFT JOIN messages tm ON tm.id = e.trigger_message_id`

func scanExecution(r pgx.CollectableRow) (runtimeapi.ConsoleExecution, error) {
	var e runtimeapi.ConsoleExecution
	err := r.Scan(&e.ID, &e.SeatID, &e.SeatKey, &e.LeaseGeneration, &e.ConfigRevision, &e.HarnessAdapter,
		&e.TriggerMessageID, &e.TriggerSummary, &e.TriggerOrigin, &e.State, &e.Error, &e.StartedAt, &e.FinishedAt, &e.Events)
	return e, err
}

// ConsoleExecutions pages a seat's runs, newest first, started before before
// (zero for the latest). Runs from every lease generation are included, so
// the history outlives any one Pod.
func (s *Store) ConsoleExecutions(ctx context.Context, seatID string, before time.Time, limit int) ([]runtimeapi.ConsoleExecution, error) {
	if _, err := uuid.Parse(seatID); err != nil {
		return nil, ErrNotFound
	}
	if before.IsZero() {
		before = time.Now().Add(time.Hour)
	}
	rows, err := s.pool.Query(ctx, `SELECT `+executionColumns+` WHERE e.seat_id = $1 AND e.started_at < $2
		ORDER BY e.started_at DESC LIMIT $3`, seatID, before, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanExecution)
}

// MaxConsoleEvents bounds the events returned for one run.
const MaxConsoleEvents = 2000

// ConsoleExecution returns a run with its trigger message, events, connector
// operations and the messages its seat sent under that run's lease.
func (s *Store) ConsoleExecution(ctx context.Context, id string) (*runtimeapi.ConsoleExecutionDetail, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, ErrNotFound
	}
	rows, err := s.pool.Query(ctx, `SELECT `+executionColumns+` WHERE e.id = $1`, id)
	if err != nil {
		return nil, err
	}
	e, err := pgx.CollectExactlyOneRow(rows, scanExecution)
	if err != nil {
		return nil, notFound(err)
	}
	out := &runtimeapi.ConsoleExecutionDetail{Execution: e}
	if e.TriggerMessageID != "" {
		msgs, err := s.consoleMessages(ctx, `m.id = $1`, e.TriggerMessageID)
		if err != nil {
			return nil, err
		}
		if len(msgs) == 1 {
			out.Trigger = &msgs[0]
		}
	}
	rows, err = s.pool.Query(ctx, `SELECT id, kind, correlation_id, data, created_at FROM execution_events
		WHERE execution_id = $1 ORDER BY id LIMIT $2`, id, MaxConsoleEvents)
	if err != nil {
		return nil, err
	}
	out.Events, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (runtimeapi.ConsoleEvent, error) {
		var ev runtimeapi.ConsoleEvent
		var data []byte
		err := r.Scan(&ev.ID, &ev.Kind, &ev.CorrelationID, &data, &ev.CreatedAt)
		ev.Data = data
		return ev, err
	})
	if err != nil {
		return nil, err
	}
	if out.Operations, err = s.consoleOperations(ctx, `o.execution_id = $1 ORDER BY o.created_at`, id); err != nil {
		return nil, err
	}
	out.Sent, err = s.consoleMessages(ctx, `m.sender_seat_id = $1 AND m.sender_generation = $2
		AND m.created_at >= $3::timestamptz AND m.created_at <= COALESCE($4::timestamptz, now())`, e.SeatID, e.LeaseGeneration, e.StartedAt, e.FinishedAt)
	return out, err
}

const consoleMessageColumns = `SELECT m.id, m.conversation_id, c.kind, m.origin, COALESCE(ss.key, ''), COALESCE(rs.key, ''),
	m.recipient_binding, m.route, COALESCE(m.parent_id::text, ''), COALESCE(m.correlation_id::text, ''), m.body, m.created_at,
	COALESCE((SELECT json_agg(json_build_object('seat_key', ds.key, 'state', d.state, 'attempts', d.attempts,
		'execution_id', COALESCE(d.execution_id::text, ''), 'last_error', d.last_error) ORDER BY d.id)
		FROM deliveries d JOIN seats ds ON ds.id = d.seat_id WHERE d.message_id = m.id), '[]'::json),
	COALESCE((SELECT ob.state FROM outbox ob WHERE ob.message_id = m.id), '')
	FROM messages m JOIN conversations c ON c.id = m.conversation_id
	LEFT JOIN seats ss ON ss.id = m.sender_seat_id LEFT JOIN seats rs ON rs.id = m.recipient_seat_id
	WHERE `

// consoleMessages returns the messages matching where, oldest first.
func (s *Store) consoleMessages(ctx context.Context, where string, args ...any) ([]runtimeapi.ConsoleMessage, error) {
	rows, err := s.pool.Query(ctx, consoleMessageColumns+where+` ORDER BY m.created_at, m.id LIMIT 500`, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (runtimeapi.ConsoleMessage, error) {
		var m runtimeapi.ConsoleMessage
		var deliveries []byte
		if err := r.Scan(&m.ID, &m.ConversationID, &m.ConversationKind, &m.Origin, &m.SenderSeat, &m.RecipientSeat,
			&m.RecipientBinding, &m.Route, &m.ParentID, &m.CorrelationID, &m.Body, &m.CreatedAt, &deliveries, &m.Outbox); err != nil {
			return m, err
		}
		return m, json.Unmarshal(deliveries, &m.Deliveries)
	})
}

// ConsoleConversations lists the organisation's conversations, most recently
// active first. System conversations (probes, schedules) are included only
// when asked for.
func (s *Store) ConsoleConversations(ctx context.Context, orgID string, includeSystem bool, limit int) ([]runtimeapi.ConsoleConversation, error) {
	if _, err := uuid.Parse(orgID); err != nil {
		return nil, ErrNotFound
	}
	rows, err := s.pool.Query(ctx, consoleConversationColumns+` WHERE c.organization_id = $1 AND ($2 OR c.kind <> 'system')
		GROUP BY c.id ORDER BY last_at DESC, c.id LIMIT $3`, orgID, includeSystem, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanConsoleConversation)
}

const consoleConversationColumns = `SELECT c.id, c.kind, c.binding,
	ARRAY(SELECT ps.key FROM seats ps WHERE ps.id = ANY(c.participants) ORDER BY ps.key),
	count(m.id), c.created_at, COALESCE(max(m.created_at), c.created_at) AS last_at
	FROM conversations c LEFT JOIN messages m ON m.conversation_id = c.id`

func scanConsoleConversation(r pgx.CollectableRow) (runtimeapi.ConsoleConversation, error) {
	var c runtimeapi.ConsoleConversation
	err := r.Scan(&c.ID, &c.Kind, &c.Binding, &c.Participants, &c.Messages, &c.CreatedAt, &c.LastAt)
	return c, err
}

// ConsoleConversation returns a conversation with its messages and their
// deliveries.
func (s *Store) ConsoleConversation(ctx context.Context, id string) (*runtimeapi.ConsoleConversationDetail, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, ErrNotFound
	}
	rows, err := s.pool.Query(ctx, consoleConversationColumns+` WHERE c.id = $1 GROUP BY c.id`, id)
	if err != nil {
		return nil, err
	}
	c, err := pgx.CollectExactlyOneRow(rows, scanConsoleConversation)
	if err != nil {
		return nil, notFound(err)
	}
	msgs, err := s.consoleMessages(ctx, `m.conversation_id = $1`, id)
	if err != nil {
		return nil, err
	}
	return &runtimeapi.ConsoleConversationDetail{Conversation: c, Messages: msgs}, nil
}

// ConsoleOperationFilter narrows ConsoleOperations.
type ConsoleOperationFilter struct {
	SeatKey string
	Status  string
	Limit   int
}

// ConsoleOperations lists the organisation's connector operations, newest first.
func (s *Store) ConsoleOperations(ctx context.Context, orgID string, f ConsoleOperationFilter) ([]runtimeapi.ConsoleOperation, error) {
	if _, err := uuid.Parse(orgID); err != nil {
		return nil, ErrNotFound
	}
	return s.consoleOperations(ctx, `o.organization_id = $1 AND ($2 = '' OR os.key = $2) AND ($3 = '' OR o.status = $3)
		ORDER BY o.created_at DESC LIMIT $4`, orgID, f.SeatKey, f.Status, f.Limit)
}

func (s *Store) consoleOperations(ctx context.Context, where string, args ...any) ([]runtimeapi.ConsoleOperation, error) {
	rows, err := s.pool.Query(ctx, `SELECT o.id, o.connection, o.operation, o.target, o.idempotency_key, o.status,
			o.external_receipt, COALESCE(o.result, 'null'::jsonb), o.error, o.created_at,
			COALESCE(os.key, ''), COALESCE(o.execution_id::text, ''), o.attempts, o.updated_at
		FROM connector_operations o LEFT JOIN seats os ON os.id = o.seat_id WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (runtimeapi.ConsoleOperation, error) {
		var op runtimeapi.ConsoleOperation
		var result []byte
		err := r.Scan(&op.ID, &op.Connection, &op.Operation.Operation, &op.Target, &op.IdempotencyKey, &op.Status,
			&op.ExternalReceipt, &result, &op.Error, &op.CreatedAt, &op.SeatKey, &op.ExecutionID, &op.Attempts, &op.UpdatedAt)
		if string(result) != "null" {
			op.Result = result
		}
		return op, err
	})
}

// ConsoleArtifacts lists the organisation's artifacts, newest first.
func (s *Store) ConsoleArtifacts(ctx context.Context, orgID string, limit int) ([]runtimeapi.ConsoleArtifact, error) {
	if _, err := uuid.Parse(orgID); err != nil {
		return nil, ErrNotFound
	}
	rows, err := s.pool.Query(ctx, `SELECT a.id, COALESCE(os.key, ''), COALESCE(ms.key, ''), a.location, a.content_digest,
			a.size_bytes, a.created_at
		FROM artifacts a LEFT JOIN seats os ON os.id = a.owner_seat_id LEFT JOIN memory_stores ms ON ms.id = a.store_id
		WHERE a.organization_id = $1 ORDER BY a.created_at DESC LIMIT $2`, orgID, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[runtimeapi.ConsoleArtifact])
}

// ConsoleActivity returns the organisation's activity after the given time,
// oldest first: messages, runs starting and finishing, tool calls, harness
// errors and connector operation status changes. With a zero after it returns
// the most recent items instead.
func (s *Store) ConsoleActivity(ctx context.Context, orgID string, after time.Time, limit int) ([]runtimeapi.ConsoleActivityItem, error) {
	if _, err := uuid.Parse(orgID); err != nil {
		return nil, ErrNotFound
	}
	latest := after.IsZero()
	order := `ORDER BY at, key`
	if latest {
		after, order = time.Unix(0, 0), `ORDER BY at DESC, key DESC`
	}
	rows, err := s.pool.Query(ctx, `SELECT * FROM (
		SELECT 'm:' || m.id AS key, 'message' AS kind, m.created_at AS at, COALESCE(ss.key, '') AS seat_key,
			COALESCE(rs.key, '') AS peer_seat,
			CASE WHEN m.origin = 'human' THEN c.binding ELSE m.recipient_binding END AS peer,
			COALESCE(d.execution_id::text, '') AS execution_id, m.id::text AS message_id, m.conversation_id::text AS conversation_id,
			m.origin AS status, left(m.body, 240) AS summary
		FROM messages m JOIN conversations c ON c.id = m.conversation_id
		LEFT JOIN seats ss ON ss.id = m.sender_seat_id LEFT JOIN seats rs ON rs.id = m.recipient_seat_id
		LEFT JOIN LATERAL (SELECT execution_id FROM deliveries WHERE message_id = m.id AND execution_id IS NOT NULL
			ORDER BY id DESC LIMIT 1) d ON true
		WHERE m.organization_id = $1 AND m.created_at > $2 AND c.kind <> 'system'
		UNION ALL
		SELECT 'rs:' || e.id, 'run_started', e.started_at, s.key, '', '', e.id::text,
			COALESCE(e.trigger_message_id::text, ''), COALESCE(tm.conversation_id::text, ''), 'running', COALESCE(left(tm.body, 240), '')
		FROM executions e JOIN seats s ON s.id = e.seat_id LEFT JOIN messages tm ON tm.id = e.trigger_message_id
		WHERE s.organization_id = $1 AND e.started_at > $2
		UNION ALL
		SELECT 'rf:' || e.id, 'run_finished', e.finished_at, s.key, '', '', e.id::text,
			COALESCE(e.trigger_message_id::text, ''), COALESCE(tm.conversation_id::text, ''), e.state, left(e.error, 240)
		FROM executions e JOIN seats s ON s.id = e.seat_id LEFT JOIN messages tm ON tm.id = e.trigger_message_id
		WHERE s.organization_id = $1 AND e.finished_at > $2
		UNION ALL
		SELECT 'ev:' || ev.id, ev.kind, ev.created_at, s.key, '', '', e.id::text,
			COALESCE(e.trigger_message_id::text, ''), COALESCE(tm.conversation_id::text, ''),
			CASE WHEN ev.kind = 'error' OR ev.data->'is_error' = 'true'::jsonb THEN 'error' ELSE '' END,
			left(CASE ev.kind
				WHEN 'error' THEN COALESCE(ev.data->>'error', ev.data->>'message', ev.data::text)
				ELSE COALESCE(ev.data->>'name', (SELECT rq.data->>'name' FROM execution_events rq WHERE rq.execution_id = ev.execution_id
					AND rq.kind = 'tool_request' AND rq.correlation_id = ev.correlation_id AND ev.correlation_id <> '' LIMIT 1), '')
			END, 240)
		FROM execution_events ev JOIN executions e ON e.id = ev.execution_id JOIN seats s ON s.id = e.seat_id
		LEFT JOIN messages tm ON tm.id = e.trigger_message_id
		WHERE s.organization_id = $1 AND ev.created_at > $2 AND ev.kind IN ('tool_request', 'tool_result', 'error')
		UNION ALL
		SELECT 'op:' || o.id || ':' || o.status, 'operation', o.updated_at, COALESCE(s.key, ''), '', o.connection,
			COALESCE(o.execution_id::text, ''), '', '', o.status,
			left(o.operation || CASE WHEN o.target <> '' THEN ' ' || o.target ELSE '' END, 240)
		FROM connector_operations o LEFT JOIN seats s ON s.id = o.seat_id
		WHERE o.organization_id = $1 AND o.updated_at > $2
	) a `+order+` LIMIT $3`, orgID, after, limit)
	if err != nil {
		return nil, err
	}
	items, err := pgx.CollectRows(rows, pgx.RowToStructByPos[runtimeapi.ConsoleActivityItem])
	if err != nil {
		return nil, err
	}
	if latest {
		slices.Reverse(items)
	}
	return items, nil
}

// ConsoleOrganizationExecutions pages the organisation's runs, newest first,
// optionally only those in the given state (running, completed, failed,
// interrupted).
func (s *Store) ConsoleOrganizationExecutions(ctx context.Context, orgID, state string, limit int) ([]runtimeapi.ConsoleExecution, error) {
	if _, err := uuid.Parse(orgID); err != nil {
		return nil, ErrNotFound
	}
	rows, err := s.pool.Query(ctx, `SELECT `+executionColumns+` WHERE s.organization_id = $1 AND ($2 = '' OR e.state = $2)
		ORDER BY e.started_at DESC LIMIT $3`, orgID, state, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanExecution)
}
