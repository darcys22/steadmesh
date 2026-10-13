package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"

	"github.com/darcys22/steadmesh/pkg/runtimeapi"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const taskColumns = `id, parent_seat_id, parent_execution_id, parent_generation, config_revision,
 service_account, pod_uid, state, request, result, created_at, deadline, finished_at, cleaned_at`

func scanTask(row pgx.Row) (*runtimeapi.TaskRun, error) {
	var t runtimeapi.TaskRun
	var req, res []byte
	err := row.Scan(&t.ID, &t.ParentSeatID, &t.ParentExecutionID, &t.ParentGeneration, &t.ConfigRevision, &t.ServiceAccount,
		&t.PodUID, &t.State, &req, &res, &t.CreatedAt, &t.Deadline, &t.FinishedAt, &t.CleanedAt)
	if err != nil {
		return nil, notFound(err)
	}
	if err = json.Unmarshal(req, &t.Request); err != nil {
		return nil, err
	}
	if len(res) > 0 {
		err = json.Unmarshal(res, &t.Result)
	}
	return &t, err
}

func (s *Store) CreateTaskRun(ctx context.Context, f Fence, parentExecutionID string, req runtimeapi.TaskRunRequest) (out *runtimeapi.TaskRun, err error) {
	err = s.inTx(ctx, func(tx pgx.Tx) error { out, err = createTaskRunTx(ctx, tx, f, parentExecutionID, req); return err })
	return
}

// createTaskRunTx is shared with atomic branch-group creation. The seat row
// serializes admission, so concurrent calls cannot exceed either bound.
func createTaskRunTx(ctx context.Context, tx pgx.Tx, f Fence, parentExecutionID string, req runtimeapi.TaskRunRequest) (*runtimeapi.TaskRun, error) {
	requestID, err := uuid.Parse(req.RequestID)
	if err != nil {
		return nil, fmt.Errorf("%w: request_id must be a UUID", ErrInvalid)
	}
	parentID, err := uuid.Parse(parentExecutionID)
	if err != nil {
		return nil, fmt.Errorf("%w: active parent execution is required", ErrInvalid)
	}
	req.RequestID = requestID.String()
	parentExecutionID = parentID.String()
	if err := runtimeapi.ValidateTaskRun(&req); err != nil {
		return nil, fmt.Errorf("%w: %s", ErrInvalid, err)
	}
	// Normalize omitted and empty slices identically for idempotent retries.
	normalized, _ := json.Marshal(req)
	req = runtimeapi.TaskRunRequest{}
	_ = json.Unmarshal(normalized, &req)
	if err := checkFence(ctx, tx, f); err != nil {
		return nil, err
	}
	var revision string
	err = tx.QueryRow(ctx, `SELECT s.config_revision FROM seats s JOIN organizations o ON o.id=s.organization_id
  WHERE s.id=$1 AND s.retired_at IS NULL AND s.retiring_at IS NULL AND o.deleted_at IS NULL FOR UPDATE OF s`, f.SeatID).Scan(&revision)
	if err != nil {
		return nil, notFound(err)
	}
	old, err := scanTask(tx.QueryRow(ctx, `SELECT `+taskColumns+` FROM task_runs WHERE parent_seat_id=$1 AND request_id=$2`, f.SeatID, req.RequestID))
	if err == nil {
		if old.ParentExecutionID != parentExecutionID || !reflect.DeepEqual(old.Request, req) {
			return nil, fmt.Errorf("%w: request_id already used for different input", ErrConflict)
		}
		return old, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	var active bool
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM executions e JOIN execution_leases l ON l.seat_id=e.seat_id
  WHERE e.id=$1 AND e.seat_id=$2 AND e.lease_generation=$3 AND e.state='running' AND l.generation=$3 AND l.expires_at>now())`, parentExecutionID, f.SeatID, f.Generation).Scan(&active)
	if err != nil {
		return nil, err
	}
	if !active {
		return nil, fmt.Errorf("%w: parent execution is not active", ErrFenced)
	}
	var activeCount, totalCount int
	err = tx.QueryRow(ctx, `SELECT count(*) FILTER(WHERE cleaned_at IS NULL),count(*) FILTER(WHERE parent_execution_id=$2)
 FROM task_runs WHERE parent_seat_id=$1`, f.SeatID, parentExecutionID).Scan(&activeCount, &totalCount)
	if err != nil {
		return nil, err
	}
	if activeCount >= runtimeapi.MaxActiveTaskRuns || totalCount >= runtimeapi.MaxTaskRunsPerExecution {
		return nil, fmt.Errorf("%w: task worker limit reached", ErrConflict)
	}
	id := uuid.NewString()
	raw, _ := json.Marshal(req)
	return scanTask(tx.QueryRow(ctx, `INSERT INTO task_runs(id,parent_seat_id,parent_execution_id,parent_generation,config_revision,service_account,request_id,request,deadline)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,now()+make_interval(secs=>$9)) RETURNING `+taskColumns,
		id, f.SeatID, parentExecutionID, f.Generation, revision, "task-"+id, req.RequestID, raw, req.TimeoutSeconds))
}

func (s *Store) TaskRun(ctx context.Context, parentSeatID, id string) (*runtimeapi.TaskRun, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, ErrNotFound
	}
	return scanTask(s.pool.QueryRow(ctx, `SELECT `+taskColumns+` FROM task_runs WHERE parent_seat_id=$1 AND id=$2`, parentSeatID, id))
}

// ListTaskRuns returns a bounded recent history. Controller calls use
// cleanupOnly to retain every unfinished cleanup rather than paging history.
func (s *Store) ListTaskRuns(ctx context.Context, parentSeatID string, cleanupOnly bool) ([]runtimeapi.TaskRun, error) {
	// Revoke immediately on deadline, generation change, revision change or retirement.
	_, err := s.pool.Exec(ctx, `UPDATE task_runs t SET state=CASE WHEN deadline<=now() THEN 'timed_out' ELSE 'cancelled' END,
 finished_at=now(),result=jsonb_build_object('error','deadline or parent authority expired')
 FROM seats s JOIN execution_leases l ON l.seat_id=s.id JOIN organizations o ON o.id=s.organization_id
 WHERE t.parent_seat_id=$1 AND s.id=t.parent_seat_id AND t.state IN ('pending','running')
 AND (t.deadline<=now() OR l.generation<>t.parent_generation OR s.config_revision<>t.config_revision OR s.retired_at IS NOT NULL OR s.retiring_at IS NOT NULL OR o.deleted_at IS NOT NULL)`, parentSeatID)
	if err != nil {
		return nil, err
	}
	query := `SELECT ` + taskColumns + ` FROM task_runs WHERE parent_seat_id=$1`
	if cleanupOnly {
		query += ` AND cleaned_at IS NULL`
	}
	query += ` ORDER BY created_at DESC LIMIT 100`
	rows, err := s.pool.Query(ctx, query, parentSeatID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []runtimeapi.TaskRun{}
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

// TaskRunByServiceAccount resolves a dedicated worker identity, never a parent
// ServiceAccount. Terminal/expired/revoked identities cease resolving.
func (s *Store) TaskRunByServiceAccount(ctx context.Context, namespace, sa string) (string, string, error) {
	var seat, id string
	err := s.pool.QueryRow(ctx, `SELECT t.parent_seat_id,t.id FROM task_runs t JOIN seats s ON s.id=t.parent_seat_id
 JOIN organizations o ON o.id=s.organization_id JOIN execution_leases l ON l.seat_id=s.id
 WHERE o.namespace=$1 AND t.service_account=$2 AND t.state IN ('pending','running') AND t.deadline>now()
 AND s.config_revision=t.config_revision AND l.generation=t.parent_generation
 AND s.retired_at IS NULL AND s.retiring_at IS NULL AND o.deleted_at IS NULL`, namespace, sa).Scan(&seat, &id)
	return seat, id, notFound(err)
}

// TaskRunAuthority revalidates the durable capability independently of the
// token authenticator. It deliberately does not require a live parent turn
// or lease holder: children survive idle suspension, not generation changes.
func (s *Store) TaskRunAuthority(ctx context.Context, seatID, id string) (bool, error) {
	var allowed bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM task_runs t JOIN seats s ON s.id=t.parent_seat_id
 JOIN organizations o ON o.id=s.organization_id JOIN execution_leases l ON l.seat_id=s.id
 WHERE t.parent_seat_id=$1 AND t.id=$2 AND t.state IN ('pending','running') AND t.deadline>now()
 AND s.config_revision=t.config_revision AND l.generation=t.parent_generation
 AND s.retired_at IS NULL AND s.retiring_at IS NULL AND o.deleted_at IS NULL)`, seatID, id).Scan(&allowed)
	return allowed, err
}

func (s *Store) ClaimTaskRun(ctx context.Context, seatID, id, podUID string) (*runtimeapi.TaskRun, error) {
	if podUID == "" {
		return nil, ErrFenced
	}
	return scanTask(s.pool.QueryRow(ctx, `UPDATE task_runs SET state='running',pod_uid=$3
 WHERE parent_seat_id=$1 AND id=$2 AND state IN ('pending','running') AND deadline>now() AND (pod_uid='' OR pod_uid=$3)
 RETURNING `+taskColumns, seatID, id, podUID))
}

func (s *Store) FinishTaskRun(ctx context.Context, seatID, id, podUID string, finish runtimeapi.TaskRunFinish) error {
	if finish.State != runtimeapi.TaskCompleted && finish.State != runtimeapi.TaskFailed && finish.State != runtimeapi.TaskTimedOut {
		return ErrInvalid
	}
	if len(finish.Result.Output) > runtimeapi.MaxTaskOutputBytes || len(finish.Result.Error) > 2048 {
		return ErrInvalid
	}
	if err := runtimeapi.ValidateTaskFiles(finish.Result.Artifacts); err != nil {
		return fmt.Errorf("%w: %s", ErrInvalid, err)
	}
	t, err := s.TaskRun(ctx, seatID, id)
	if err != nil {
		return err
	}
	for _, file := range finish.Result.Artifacts {
		found := false
		for _, p := range t.Request.ArtifactPaths {
			found = found || p == file.Path
		}
		if !found {
			return ErrInvalid
		}
	}
	raw, _ := json.Marshal(finish.Result)
	tag, err := s.pool.Exec(ctx, `UPDATE task_runs SET state=$4,result=$5,finished_at=now() WHERE parent_seat_id=$1 AND id=$2 AND pod_uid=$3 AND state='running' AND deadline>now()`, seatID, id, podUID, finish.State, raw)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrConflict
	}
	return nil
}

func (s *Store) CancelTaskRun(ctx context.Context, f Fence, id string) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		if err := checkFence(ctx, tx, f); err != nil {
			return err
		}
		return terminateTaskRun(ctx, tx, f.SeatID, id, runtimeapi.TaskCancelled)
	})
}

func terminateTaskRun(ctx context.Context, tx pgx.Tx, seatID, id, state string) error {
	if _, err := uuid.Parse(id); err != nil {
		return ErrNotFound
	}
	tag, err := tx.Exec(ctx, `UPDATE task_runs SET state=$3,finished_at=now() WHERE parent_seat_id=$1 AND id=$2 AND state IN ('pending','running')`, seatID, id, state)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		var exists bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM task_runs WHERE parent_seat_id=$1 AND id=$2)`, seatID, id).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return ErrNotFound
		}
	}
	return nil
}

// ReconcileTaskRun records runtime failures or confirmed cleanup. It cannot
// mark work successful; only the authenticated bound worker returns a result.
func (s *Store) ReconcileTaskRun(ctx context.Context, seatID, id, action string) error {
	if action == "cleaned" {
		return s.inTx(ctx, func(tx pgx.Tx) error {
			var state, parent, org string
			err := tx.QueryRow(ctx, `SELECT t.state,t.parent_execution_id,s.organization_id FROM task_runs t JOIN seats s ON s.id=t.parent_seat_id
    WHERE t.parent_seat_id=$1 AND t.id=$2 AND t.state NOT IN ('pending','running') FOR UPDATE OF t`, seatID, id).Scan(&state, &parent, &org)
			if err != nil {
				return notFound(err)
			}
			var active bool
			if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM seats s JOIN organizations o ON o.id=s.organization_id WHERE s.id=$1 AND s.retired_at IS NULL AND s.retiring_at IS NULL AND o.deleted_at IS NULL)`, seatID).Scan(&active); err != nil {
				return err
			}
			if active {
				body := fmt.Sprintf("Disposable task worker %s finished with technical state %s. Parent execution: %s. The temporary workspace has been removed. Use tasks.get with this task ID to inspect its untrusted output and artifact list. Nothing has been applied to your persistent workspace.", id, state, parent)
				msg, inserted, err := systemMessage(ctx, tx, org, seatID, OriginSystem, "task-result:"+id, body)
				if err != nil {
					return err
				}
				if inserted {
					if _, err = tx.Exec(ctx, `UPDATE messages SET correlation_id=$2 WHERE id=$1`, msg, id); err != nil {
						return err
					}
				}
			}
			_, err = tx.Exec(ctx, `UPDATE task_runs SET cleaned_at=COALESCE(cleaned_at,now()) WHERE parent_seat_id=$1 AND id=$2`, seatID, id)
			return err
		})
	}
	state := runtimeapi.TaskFailed
	if action == "cancel" {
		state = runtimeapi.TaskCancelled
	} else if action != "failed" {
		return ErrInvalid
	}
	return s.inTx(ctx, func(tx pgx.Tx) error { return terminateTaskRun(ctx, tx, seatID, id, state) })
}
