//go:build integration

package store

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/darcys22/steadmesh/pkg/spec"
	"github.com/darcys22/steadmesh/services/internal/orgfixture"
	"github.com/google/uuid"
)

func budgetFixture(t *testing.T, b spec.ModelBudgets) (*Store, string, Fence, string) {
	t.Helper()
	s := open(t)
	sp := orgfixture.Spec()
	sp.ModelBudgets = &b
	o := syncOrg(t, s, orgfixture.Compile(t, sp))
	f := lease(t, s, o.Seats["lead"].SeatID)
	id := uuid.NewString()
	_, err := s.pool.Exec(context.Background(), `INSERT INTO executions (id,seat_id,lease_generation,config_revision,harness_adapter) VALUES ($1,$2,$3,'r','fake')`, id, f.SeatID, f.Generation)
	if err != nil {
		t.Fatal(err)
	}
	return s, o.OrganizationID, f, id
}

func TestModelBudgetConcurrentReservationsAcrossReplicas(t *testing.T) {
	ctx := context.Background()
	s, org, f, id := budgetFixture(t, spec.ModelBudgets{OrganizationDailyOutputTokens: 100, SeatDailyOutputTokens: 100, ExecutionOutputTokens: 100, MaxRequestOutputTokens: 10})
	// Separate Store/pool proves no process-local mutex is the authority.
	other, err := Open(ctx, s.pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	var wg sync.WaitGroup
	var accepted, denied atomic.Int64
	for i := range 30 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			st := s
			if i%2 == 1 {
				st = other
			}
			r, err := st.ReserveModelOutput(ctx, org, f, id, false, 10, "")
			var exhausted *BudgetExhausted
			switch {
			case err == nil:
				if r.OutputTokens != 10 {
					t.Errorf("reservation=%d", r.OutputTokens)
				}
				accepted.Add(1)
			case errors.As(err, &exhausted):
				denied.Add(1)
			default:
				t.Errorf("reserve: %v", err)
			}
		}(i)
	}
	wg.Wait()
	if accepted.Load() != 10 || denied.Load() != 20 {
		t.Fatalf("accepted=%d denied=%d", accepted.Load(), denied.Load())
	}
	status, err := other.ModelBudgetStatus(ctx, org, f.SeatID, id)
	if err != nil || status.OrganizationOutputTokens != 100 || status.SeatOutputTokens != 100 || status.ExecutionOutputTokens != 100 {
		t.Fatalf("status=%+v err=%v", status, err)
	}
}

func TestModelBudgetSettlementIsDurableIdempotentAndConservative(t *testing.T) {
	ctx := context.Background()
	s, org, f, id := budgetFixture(t, spec.ModelBudgets{SeatDailyOutputTokens: 100, ExecutionOutputTokens: 100, MaxRequestOutputTokens: 10})
	r, err := s.ReserveModelOutput(ctx, org, f, id, false, 10, "")
	if err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := s.SettleModelOutput(canceled, org, r.ID, 2, true); err == nil {
		t.Fatal("canceled settlement unexpectedly committed")
	}
	status, _ := s.ModelBudgetStatus(ctx, org, f.SeatID, id)
	if status.SeatOutputTokens != 10 {
		t.Fatal("lost reservation on cancellation")
	}
	for range 3 {
		if err := s.SettleModelOutput(ctx, org, r.ID, 2, true); err != nil {
			t.Fatal(err)
		}
	}
	r, err = s.ReserveModelOutput(ctx, org, f, id, false, 10, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SettleModelOutput(ctx, org, r.ID, 0, false); err != nil {
		t.Fatal(err)
	}
	// Late retries cannot refund an already settled unknown response.
	if err := s.SettleModelOutput(ctx, org, r.ID, 0, true); err != nil {
		t.Fatal(err)
	}
	other, err := Open(ctx, s.pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	status, err = other.ModelBudgetStatus(ctx, org, f.SeatID, id)
	if err != nil || status.SeatOutputTokens != 12 || status.ExecutionOutputTokens != 12 {
		t.Fatalf("status=%+v %v", status, err)
	}
	// A request canceled before admission makes no reservation.
	if _, err := s.ReserveModelOutput(canceled, org, f, id, false, 10, ""); err == nil {
		t.Fatal("accepted canceled request")
	}
}

func TestModelBudgetExhaustionPolicyIncreaseAndUTCRollover(t *testing.T) {
	ctx := context.Background()
	s, org, f, id := budgetFixture(t, spec.ModelBudgets{OrganizationDailyOutputTokens: 10, ExecutionOutputTokens: 50, MaxRequestOutputTokens: 8})
	for _, want := range []int64{8, 2} {
		r, err := s.ReserveModelOutput(ctx, org, f, id, false, 100, "")
		if err != nil || r.OutputTokens != want {
			t.Fatalf("r=%+v err=%v", r, err)
		}
	}
	_, err := s.ReserveModelOutput(ctx, org, f, id, false, 1, "")
	var exhausted *BudgetExhausted
	if !errors.As(err, &exhausted) || exhausted.Scope != "organization" || exhausted.ResetsAt.IsZero() {
		t.Fatalf("exhaustion=%v", err)
	}
	// Raising the committed policy resumes without deleting prior debits.
	sp := orgfixture.Spec()
	sp.ModelBudgets = &spec.ModelBudgets{OrganizationDailyOutputTokens: 20, ExecutionOutputTokens: 50, MaxRequestOutputTokens: 8}
	syncOrg(t, s, orgfixture.Compile(t, sp))
	if r, err := s.ReserveModelOutput(ctx, org, f, id, false, 10, ""); err != nil || r.OutputTokens != 8 {
		t.Fatalf("resume=%+v %v", r, err)
	}
	// Put historical daily counters in yesterday's partition. Execution
	// counters deliberately retain their lifetime epoch partition.
	if _, err := s.pool.Exec(ctx, `UPDATE model_budget_counters SET period_start=period_start-1 WHERE organization_id=$1 AND scope<>'execution'`, org); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE model_budget_reservations SET day=day-1 WHERE organization_id=$1`, org); err != nil {
		t.Fatal(err)
	}
	status, err := s.ModelBudgetStatus(ctx, org, f.SeatID, id)
	if err != nil || status.OrganizationOutputTokens != 0 || status.ExecutionOutputTokens != 18 {
		t.Fatalf("rollover=%+v %v", status, err)
	}
	if _, err := s.ReserveModelOutput(ctx, org, f, id, false, 8, ""); err != nil {
		t.Fatal(err)
	}
	status, _ = s.ModelBudgetStatus(ctx, org, f.SeatID, id)
	if status.OrganizationOutputTokens != 8 || status.ExecutionOutputTokens != 26 {
		t.Fatalf("new day=%+v", status)
	}
}

func TestModelBudgetExecutionIdentityAndScopeIsolation(t *testing.T) {
	ctx := context.Background()
	s, org, f, id := budgetFixture(t, spec.ModelBudgets{SeatDailyOutputTokens: 100, ExecutionOutputTokens: 5})
	for _, wrong := range []string{"", uuid.NewString(), "not-an-id"} {
		if _, err := s.ReserveModelOutput(ctx, org, f, wrong, false, 1, ""); !errors.Is(err, ErrInvalid) {
			t.Fatalf("execution %q: %v", wrong, err)
		}
	}
	stale := f
	stale.Generation++
	if _, err := s.ReserveModelOutput(ctx, org, stale, id, false, 1, ""); !errors.Is(err, ErrFenced) {
		t.Fatalf("stale: %v", err)
	}
	r, err := s.ReserveModelOutput(ctx, org, f, id, false, 100, "")
	if err != nil || r.OutputTokens != 5 {
		t.Fatalf("r=%+v %v", r, err)
	}
	if _, err := s.ReserveModelOutput(ctx, org, f, id, false, 1, ""); err == nil {
		t.Fatal("execution cap bypassed")
	}
	for _, alias := range []string{strings.ToUpper(id), strings.ReplaceAll(id, "-", ""), "{" + id + "}"} {
		if _, err := s.ReserveModelOutput(ctx, org, f, alias, false, 1, ""); err == nil {
			t.Fatalf("UUID alias bypassed cap: %q", alias)
		}
		status, err := s.ModelBudgetStatus(ctx, org, f.SeatID, alias)
		if err != nil || status.ExecutionOutputTokens != 5 {
			t.Fatalf("UUID alias status: %+v %v", status, err)
		}
	}
	status, err := s.ModelBudgetStatus(ctx, org, uuid.NewString(), id)
	if err != nil || status.ExecutionOutputTokens != 0 {
		t.Fatalf("cross-seat execution exposed: %+v %v", status, err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE executions SET state='completed' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReserveModelOutput(ctx, org, f, id, false, 1, ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("completed execution: %v", err)
	}
	// A trusted child's completed parent remains charged against the same cap.
	if _, err := s.ReserveModelOutput(ctx, org, f, id, true, 1, ""); err == nil || errors.Is(err, ErrInvalid) {
		t.Fatalf("trusted child should hit shared cap: %v", err)
	}
}

func TestModelBudgetDisabledAndInvalidRequests(t *testing.T) {
	ctx := context.Background()
	s, org, f, id := budgetFixture(t, spec.ModelBudgets{SeatDailyOutputTokens: 10})
	if _, err := s.ReserveModelOutput(ctx, org, f, id, false, 0, "unsupported endpoint"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid request: %v", err)
	}
	status, _ := s.ModelBudgetStatus(ctx, org, f.SeatID, id)
	if status.SeatOutputTokens != 0 {
		t.Fatal("invalid request charged")
	}
	syncOrg(t, s, orgfixture.Manifest(t))
	if r, err := s.ReserveModelOutput(ctx, org, Fence{}, "", false, 0, "unsupported endpoint"); err != nil || r != nil {
		t.Fatalf("disabled: %+v %v", r, err)
	}
}

func TestModelBudgetAcrossSeatsAndPurge(t *testing.T) {
	ctx := context.Background()
	s, org, lead, _ := budgetFixture(t, spec.ModelBudgets{OrganizationDailyOutputTokens: 8, SeatDailyOutputTokens: 5, MaxRequestOutputTokens: 10})
	seat, err := s.SeatByKey(ctx, org, "engineer")
	if err != nil {
		t.Fatal(err)
	}
	engineer := lease(t, s, seat.ID)
	for i, f := range []Fence{lead, engineer} {
		r, err := s.ReserveModelOutput(ctx, org, f, "", false, 10, "")
		want := int64(5)
		if i == 1 {
			want = 3
		}
		if err != nil || r.OutputTokens != want {
			t.Fatalf("seat%d=%+v %v", i, r, err)
		}
	}
	if _, err := s.ReserveModelOutput(ctx, org, engineer, "", false, 1, ""); err == nil {
		t.Fatal("organization cap did not span seats")
	}
	if err := s.DeleteOrganization(ctx, org, true); err != nil {
		t.Fatalf("purge reservations: %v", err)
	}
}

func TestModelBudgetLateSettlementStaysInOriginalDay(t *testing.T) {
	ctx := context.Background()
	s, org, f, id := budgetFixture(t, spec.ModelBudgets{SeatDailyOutputTokens: 10, ExecutionOutputTokens: 100, MaxRequestOutputTokens: 10})
	r, err := s.ReserveModelOutput(ctx, org, f, id, false, 10, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE model_budget_counters SET period_start=period_start-1 WHERE organization_id=$1 AND scope<>'execution'`, org); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE model_budget_reservations SET day=day-1 WHERE id=$1`, r.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReserveModelOutput(ctx, org, f, id, false, 10, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.SettleModelOutput(ctx, org, r.ID, 2, true); err != nil {
		t.Fatal(err)
	}
	status, err := s.ModelBudgetStatus(ctx, org, f.SeatID, id)
	if err != nil || status.SeatOutputTokens != 10 || status.ExecutionOutputTokens != 12 {
		t.Fatalf("late settlement=%+v %v", status, err)
	}
}

func TestModelBudgetReservationDoesNotDeadlockWithAck(t *testing.T) {
	ctx := context.Background()
	s, org, f, id := budgetFixture(t, spec.ModelBudgets{ExecutionOutputTokens: 100})
	// Reproduce Ack's ordering: lease FOR SHARE, execution UPDATE,
	// then lease UPDATE. Admission must not wait on its execution row
	// while holding another share of the lease row.
	ack, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer ack.Rollback(ctx)
	if err := checkFence(ctx, ack, f); err != nil {
		t.Fatal(err)
	}
	if _, err := ack.Exec(ctx, `UPDATE executions SET state='completed' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	bounded, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if _, err := s.ReserveModelOutput(bounded, org, f, id, false, 10, ""); err != nil {
		t.Fatalf("reservation blocked by concurrent ack: %v", err)
	}
	if _, err := ack.Exec(bounded, `UPDATE execution_leases SET last_activity=now() WHERE seat_id=$1`, f.SeatID); err != nil {
		t.Fatal(err)
	}
	if err := ack.Commit(bounded); err != nil {
		t.Fatal(err)
	}
}

func TestModelBudgetTrustedChildAfterParentIdle(t *testing.T) {
	ctx := context.Background()
	s, org, f, id := budgetFixture(t, spec.ModelBudgets{SeatDailyOutputTokens: 100, ExecutionOutputTokens: 100})
	if _, err := s.pool.Exec(ctx, `UPDATE executions SET state='completed' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE execution_leases SET holder='',expires_at='epoch' WHERE seat_id=$1`, f.SeatID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReserveModelOutput(ctx, org, f, id, false, 10, ""); !errors.Is(err, ErrFenced) {
		t.Fatalf("ordinary idle request: %v", err)
	}
	if _, err := s.ReserveModelOutput(ctx, org, f, id, true, 10, ""); err != nil {
		t.Fatalf("verified child after idle: %v", err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE execution_leases SET generation=generation+1 WHERE seat_id=$1`, f.SeatID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReserveModelOutput(ctx, org, f, id, true, 10, ""); !errors.Is(err, ErrFenced) {
		t.Fatalf("stale child request: %v", err)
	}
}
