package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/darcys22/steadmesh/pkg/runtimeapi"
	"github.com/darcys22/steadmesh/pkg/spec"
)

// BudgetExhausted identifies a scope with no remaining output-token headroom.
// Daily scopes resume at ResetsAt. Execution caps require a policy increase.
type BudgetExhausted struct {
	Scope    string
	ResetsAt time.Time
}

func (e *BudgetExhausted) Error() string {
	if !e.ResetsAt.IsZero() {
		return fmt.Sprintf("%s output-token budget exhausted; resets at %s or increase the declared limit", e.Scope, e.ResetsAt.Format(time.RFC3339))
	}
	return "execution output-token budget exhausted; increase the declared limit to continue this execution"
}

// ModelReservation is a durable debit made before a model request is sent.
type ModelReservation struct {
	ID           string
	OutputTokens int64
}

type budgetScope struct {
	name, id, day string
	limit         int64
}

func modelScopes(org, seat, execution, day string, b spec.ModelBudgets) []budgetScope {
	out := []budgetScope{{"organization", org, day, b.OrganizationDailyOutputTokens}, {"seat", seat, day, b.SeatDailyOutputTokens}}
	if execution != "" {
		out = append(out, budgetScope{"execution", execution, "1970-01-01", b.ExecutionOutputTokens})
	}
	return out
}

// ReserveModelOutput reads the committed budget policy and atomically debits all
// scopes. Serializing briefly on the organization row coordinates platform
// replicas, policy updates and settlements. No lock is held during inference.
// Nil means budgets are disabled. Invalid request reasons matter only when
// enabled, preserving the existing unbudgeted API behavior. trustedChild may
// only be set after task-run authentication validates its running state, PodUID,
// deadline and parent generation; such a task can outlive its parent's turn or
// idle lease, but shares the exact parent execution's cumulative counter.
func (s *Store) ReserveModelOutput(ctx context.Context, org string, f Fence, execution string, trustedChild bool, requested int64, invalid string) (*ModelReservation, error) {
	var out *ModelReservation
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var raw []byte
		if err := tx.QueryRow(ctx, `SELECT manifest->'spec'->'model_budgets' FROM organizations
			WHERE id=$1 AND deleted_at IS NULL FOR UPDATE`, org).Scan(&raw); err != nil {
			return notFound(err)
		}
		if len(raw) == 0 || string(raw) == "null" {
			if _, err := uuid.Parse(f.SeatID); err != nil {
				return nil
			} // preserve unbudgeted legacy calls
			var native bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM native_pool_members WHERE seat_id=$1)`, f.SeatID).Scan(&native); err != nil {
				return err
			}
			if native {
				return fmt.Errorf("%w: native pools require a shared model budget", ErrInvalid)
			}
			return nil
		}
		var b spec.ModelBudgets
		if err := json.Unmarshal(raw, &b); err != nil {
			return err
		}
		if !b.Valid() {
			return fmt.Errorf("%w: invalid model budget policy", ErrInvalid)
		}
		if invalid != "" {
			return fmt.Errorf("%w: %s", ErrInvalid, invalid)
		}
		if requested < 0 {
			return ErrInvalid
		}
		// A seat cannot evade a cumulative execution cap by inventing an ID,
		// omitting it, or replaying a completed/fenced execution.
		if err := checkFence(ctx, tx, f); err != nil {
			return err
		}
		var active bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM seats s JOIN execution_leases l ON l.seat_id=s.id
			WHERE s.id=$1 AND s.organization_id=$2 AND s.retired_at IS NULL
			AND l.generation=$3 AND ($4 OR (l.holder<>'' AND l.expires_at>clock_timestamp())))`, f.SeatID, org, f.Generation, trustedChild).Scan(&active); err != nil {
			return err
		}
		if !active {
			return ErrFenced
		}
		if execution == "" && b.ExecutionOutputTokens > 0 {
			return fmt.Errorf("%w: execution header is required by model budgets", ErrInvalid)
		}
		if execution != "" {
			parsed, err := uuid.Parse(execution)
			if err != nil {
				return fmt.Errorf("%w: invalid execution", ErrInvalid)
			}
			execution = parsed.String()
			// Do not lock the execution row while holding the lease share:
			// Ack updates the execution, then the lease. A row lock here
			// would invert that ordering. Admission may race completion,
			// but it always debits the same immutable execution counter.
			var id string
			err = tx.QueryRow(ctx, `SELECT id FROM executions WHERE id=$1 AND seat_id=$2 AND lease_generation=$3 AND (state='running' OR $4)`, execution, f.SeatID, f.Generation, trustedChild).Scan(&id)
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("%w: execution is not running for this seat and generation", ErrInvalid)
			}
			if err != nil {
				return err
			}
		}
		var day string
		if err := tx.QueryRow(ctx, `SELECT to_char(clock_timestamp() AT TIME ZONE 'UTC', 'YYYY-MM-DD')`).Scan(&day); err != nil {
			return err
		}
		limit := b.MaxRequestOutputTokens
		if limit == 0 {
			limit = spec.DefaultMaxRequestOutputTokens
		}
		if requested > 0 && requested < limit {
			limit = requested
		}
		scopes := modelScopes(org, f.SeatID, execution, day, b)
		for _, sc := range scopes {
			if _, err := tx.Exec(ctx, `INSERT INTO model_budget_counters (organization_id,scope,scope_id,period_start)
				VALUES ($1,$2,$3,$4::date) ON CONFLICT DO NOTHING`, org, sc.name, sc.id, sc.day); err != nil {
				return err
			}
			var used int64
			if err := tx.QueryRow(ctx, `SELECT charged_output_tokens FROM model_budget_counters
				WHERE organization_id=$1 AND scope=$2 AND scope_id=$3 AND period_start=$4::date`, org, sc.name, sc.id, sc.day).Scan(&used); err != nil {
				return err
			}
			if sc.limit > 0 {
				if used >= sc.limit {
					e := &BudgetExhausted{Scope: sc.name}
					if sc.name != "execution" {
						d, _ := time.Parse("2006-01-02", day)
						e.ResetsAt = d.AddDate(0, 0, 1)
					}
					return e
				}
				if left := sc.limit - used; left < limit {
					limit = left
				}
			}
		}
		out = &ModelReservation{ID: uuid.NewString(), OutputTokens: limit}
		for _, sc := range scopes {
			if _, err := tx.Exec(ctx, `UPDATE model_budget_counters SET charged_output_tokens=charged_output_tokens+$5
				WHERE organization_id=$1 AND scope=$2 AND scope_id=$3 AND period_start=$4::date`, org, sc.name, sc.id, sc.day, limit); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, `INSERT INTO model_budget_reservations (id,organization_id,seat_id,execution_id,day,reserved_output_tokens,charged_output_tokens)
			VALUES ($1,$2,$3,NULLIF($4,'')::uuid,$5::date,$6,$6)`, out.ID, org, f.SeatID, execution, day, limit)
		return err
	})
	return out, err
}

// SettleModelOutput idempotently refunds unused output tokens only when a
// complete response reported trustworthy usage. Crashes, cancellation, missing
// usage and settlement failures keep the original reservation charged. A
// misbehaving endpoint's overrun is charged, never clamped out of the ledger.
func (s *Store) SettleModelOutput(ctx context.Context, org, id string, output int64, known bool) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		var locked string
		if err := tx.QueryRow(ctx, `SELECT id FROM organizations WHERE id=$1 FOR UPDATE`, org).Scan(&locked); err != nil {
			return notFound(err)
		}
		var seat, execution, day string
		var reserved int64
		var settled bool
		if err := tx.QueryRow(ctx, `SELECT seat_id,COALESCE(execution_id::text,''),day::text,reserved_output_tokens,settled
			FROM model_budget_reservations WHERE id=$1 AND organization_id=$2 FOR UPDATE`, id, org).Scan(&seat, &execution, &day, &reserved, &settled); err != nil {
			return notFound(err)
		}
		if settled {
			return nil
		}
		if !known || output < 0 {
			output, known = reserved, false
		}
		for _, sc := range modelScopes(org, seat, execution, day, spec.ModelBudgets{}) {
			if _, err := tx.Exec(ctx, `UPDATE model_budget_counters SET charged_output_tokens=charged_output_tokens+$5
				WHERE organization_id=$1 AND scope=$2 AND scope_id=$3 AND period_start=$4::date`, org, sc.name, sc.id, sc.day, output-reserved); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, `UPDATE model_budget_reservations SET charged_output_tokens=$3,usage_known=$4,settled=true,settled_at=now()
			WHERE id=$1 AND organization_id=$2`, id, org, output, known)
		return err
	})
}

// ModelBudgetStatus returns the current policy and this seat's counters. The
// execution lookup is seat-scoped, so a forged header cannot reveal other runs.
func (s *Store) ModelBudgetStatus(ctx context.Context, org, seat, execution string) (*runtimeapi.ModelBudgetStatus, error) {
	if id, err := uuid.Parse(execution); err == nil {
		execution = id.String()
	}
	out := &runtimeapi.ModelBudgetStatus{}
	var raw []byte
	err := s.pool.QueryRow(ctx, `SELECT o.manifest->'spec'->'model_budgets', d.day::text,
		COALESCE((SELECT charged_output_tokens FROM model_budget_counters WHERE organization_id=o.id AND scope='organization' AND scope_id=o.id::text AND period_start=d.day),0),
		COALESCE((SELECT charged_output_tokens FROM model_budget_counters WHERE organization_id=o.id AND scope='seat' AND scope_id=$2 AND period_start=d.day),0),
		COALESCE((SELECT c.charged_output_tokens FROM model_budget_counters c JOIN executions e ON e.id::text=c.scope_id
		 WHERE c.organization_id=o.id AND c.scope='execution' AND c.scope_id=$3 AND e.seat_id::text=$2),0)
		FROM organizations o CROSS JOIN (SELECT (clock_timestamp() AT TIME ZONE 'UTC')::date AS day) d
		WHERE o.id=$1 AND o.deleted_at IS NULL`, org, seat, execution).Scan(&raw, &out.Day, &out.OrganizationOutputTokens, &out.SeatOutputTokens, &out.ExecutionOutputTokens)
	if err != nil {
		return nil, notFound(err)
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out.Limits); err != nil {
			return nil, err
		}
	}
	d, _ := time.Parse("2006-01-02", out.Day)
	out.ResetsAt = d.AddDate(0, 0, 1)
	return out, nil
}
