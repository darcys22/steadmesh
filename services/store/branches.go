package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/darcys22/steadmesh/pkg/runtimeapi"
)

// CreateRunBranch atomically creates the immutable snapshot and every child.
// Any validation, capacity or authority failure rolls the whole group back.
// A retry with the same request_id returns the existing group; changed payloads
// conflict instead of silently launching more alternatives.
func (s *Store) CreateRunBranch(ctx context.Context, f Fence, executionID string, req runtimeapi.RunBranchRequest) (*runtimeapi.RunBranchComparison, error) {
	requestID, err := uuid.Parse(req.RequestID)
	if err != nil {
		return nil, fmt.Errorf("%w: request_id must be a UUID", ErrInvalid)
	}
	req.RequestID = requestID.String()
	if _, err := uuid.Parse(executionID); err != nil {
		return nil, fmt.Errorf("%w: a current parent execution is required", ErrInvalid)
	}
	if err := runtimeapi.ValidateRunBranch(&req); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	raw, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(raw)
	digest := hex.EncodeToString(sum[:])
	id := uuid.NewString()
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		if err := checkFence(ctx, tx, f); err != nil {
			return err
		}
		err := tx.QueryRow(ctx, `INSERT INTO run_branches
			(id, parent_seat_id, parent_execution_id, request_id, request, request_sha256, snapshot_sha256)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			ON CONFLICT (parent_seat_id, request_id) DO NOTHING RETURNING id`,
			id, f.SeatID, executionID, req.RequestID, raw, digest, runtimeapi.BranchSnapshotSHA256(req)).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			var previous string
			if err := tx.QueryRow(ctx, `SELECT id, request_sha256 FROM run_branches
				WHERE parent_seat_id = $1 AND request_id = $2`, f.SeatID, req.RequestID).Scan(&id, &previous); err != nil {
				return err
			}
			if previous != digest {
				return fmt.Errorf("%w: request_id already belongs to a different branch request", ErrConflict)
			}
			return nil
		}
		if err != nil {
			return err
		}
		for i, a := range req.Alternatives {
			task, err := createTaskRunTx(ctx, tx, f, executionID, runtimeapi.BranchTaskRequest(req, a, uuid.NewString()))
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO run_branch_members (branch_id, task_run_id, name, ordinal)
				VALUES ($1, $2, $3, $4)`, id, task.ID, a.Name, i); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.CompareRunBranch(ctx, f.SeatID, id)
}

type branchQuery interface {
	QueryRow(context.Context, string, ...any) pgx.Row
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

// CompareRunBranch reads only groups owned by this parent seat. A repeatable
// read gives a coherent comparison even while child completions/choices arrive.
func (s *Store) CompareRunBranch(ctx context.Context, seatID, id string) (*runtimeapi.RunBranchComparison, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, ErrNotFound
	}
	var out *runtimeapi.RunBranchComparison
	err := pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		var err error
		out, err = compareRunBranch(ctx, tx, seatID, id)
		return err
	})
	return out, err
}

func compareRunBranch(ctx context.Context, q branchQuery, seatID, id string) (*runtimeapi.RunBranchComparison, error) {
	out := &runtimeapi.RunBranchComparison{AllTerminal: true, Alternatives: []runtimeapi.RunAlternativeState{}}
	var selectedID, selectedDigest *string
	var selectedAt *time.Time
	if err := q.QueryRow(ctx, `SELECT id, request_id, parent_execution_id, snapshot_sha256,
		COALESCE(jsonb_array_length(request->'input_files'), 0), revision, selected_task_run_id,
		selected_result_sha256, selected_at, created_at FROM run_branches
		WHERE parent_seat_id = $1 AND id = $2`, seatID, id).Scan(
		&out.ID, &out.RequestID, &out.ParentExecutionID, &out.SnapshotSHA256,
		&out.InputFileCount, &out.Revision, &selectedID, &selectedDigest, &selectedAt, &out.CreatedAt); err != nil {
		return nil, notFound(err)
	}
	if selectedID != nil && selectedDigest != nil && selectedAt != nil {
		out.Selection = &runtimeapi.RunBranchSelection{TaskRunID: *selectedID, ResultSHA256: *selectedDigest, SelectedAt: *selectedAt}
	}
	rows, err := q.Query(ctx, `SELECT m.name, t.id, t.state, t.result, t.finished_at
		FROM run_branch_members m JOIN task_runs t ON t.id = m.task_run_id
		WHERE m.branch_id = $1 AND t.parent_seat_id = $2 ORDER BY m.ordinal`, id, seatID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var alt runtimeapi.RunAlternativeState
		var raw []byte
		if err := rows.Scan(&alt.Name, &alt.TaskRunID, &alt.State, &raw, &alt.FinishedAt); err != nil {
			return nil, err
		}
		if !(runtimeapi.TaskRun{State: alt.State}).Terminal() {
			out.AllTerminal = false
		}
		if len(raw) > 0 && string(raw) != "null" {
			var result runtimeapi.TaskRunResult
			if err := json.Unmarshal(raw, &result); err != nil {
				return nil, err
			}
			alt.ResultSHA256 = runtimeapi.TaskResultSHA256(&result)
			alt.OutputPreview, alt.OutputTruncated = branchPreview(result.Output, 256)
			alt.ErrorPreview, alt.ErrorTruncated = branchPreview(result.Error, 128)
			alt.ArtifactCount = len(result.Artifacts)
			for _, file := range result.Artifacts {
				alt.ArtifactBytes += len(file.Content)
			}
		}
		out.Alternatives = append(out.Alternatives, alt)
	}
	return out, rows.Err()
}

// SelectRunBranch records an exact completed result and an append-only audit
// entry. It never cancels siblings or copies artifacts to any workspace. The
// result digest and group revision prevent choosing stale or different output.
func (s *Store) SelectRunBranch(ctx context.Context, f Fence, req runtimeapi.RunBranchSelectRequest) (*runtimeapi.RunBranchComparison, error) {
	for _, id := range []string{req.ID, req.TaskRunID} {
		if _, err := uuid.Parse(id); err != nil {
			return nil, ErrNotFound
		}
	}
	if req.ExpectedRevision < 1 || len(req.ExpectedResultSHA256) != 64 {
		return nil, fmt.Errorf("%w: expected_revision and expected_result_sha256 from comparison are required", ErrInvalid)
	}
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		if err := checkFence(ctx, tx, f); err != nil {
			return err
		}
		var revision int
		var selectedID *string
		if err := tx.QueryRow(ctx, `SELECT revision, selected_task_run_id FROM run_branches
			WHERE id = $1 AND parent_seat_id = $2 FOR UPDATE`, req.ID, f.SeatID).Scan(&revision, &selectedID); err != nil {
			return notFound(err)
		}
		if revision != req.ExpectedRevision {
			return &RevisionConflict{Current: revision}
		}
		var state string
		var raw []byte
		if err := tx.QueryRow(ctx, `SELECT t.state, t.result FROM task_runs t
			JOIN run_branch_members m ON m.task_run_id = t.id
			WHERE m.branch_id = $1 AND t.id = $2 AND t.parent_seat_id = $3 FOR SHARE OF t`,
			req.ID, req.TaskRunID, f.SeatID).Scan(&state, &raw); err != nil {
			return notFound(err)
		}
		if state != runtimeapi.TaskCompleted || len(raw) == 0 || string(raw) == "null" {
			return fmt.Errorf("%w: only a completed alternative with a result can be selected", ErrInvalid)
		}
		var result runtimeapi.TaskRunResult
		if err := json.Unmarshal(raw, &result); err != nil {
			return err
		}
		digest := runtimeapi.TaskResultSHA256(&result)
		if req.ExpectedResultSHA256 != digest {
			return fmt.Errorf("%w: result digest changed; compare and review the result again", ErrConflict)
		}
		if selectedID != nil && *selectedID == req.TaskRunID {
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE run_branches SET selected_task_run_id = $2,
			selected_result_sha256 = $3, selected_at = now(), revision = revision + 1 WHERE id = $1`,
			req.ID, req.TaskRunID, digest); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO run_branch_selections (branch_id, revision, task_run_id, result_sha256)
			VALUES ($1, $2, $3, $4)`, req.ID, revision+1, req.TaskRunID, digest)
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.CompareRunBranch(ctx, f.SeatID, req.ID)
}

func branchPreview(s string, n int) (string, bool) {
	if len(s) <= n {
		return s, false
	}
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n], true
}
