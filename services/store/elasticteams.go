package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/darcys22/steadmesh/pkg/runtimeapi"
	"github.com/darcys22/steadmesh/pkg/spec"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// lockWorkerOrganization is the first lock in every disposable-worker admission
// transaction. This also serializes with budget reservations and policy updates.
func lockWorkerOrganization(ctx context.Context, tx pgx.Tx, seat string) (string, *spec.OrganizationSpec, error) {
	var org string
	var raw []byte
	err := tx.QueryRow(ctx, `SELECT o.id,o.manifest->'spec' FROM organizations o JOIN seats s ON s.organization_id=o.id WHERE s.id=$1 AND o.deleted_at IS NULL FOR UPDATE OF o`, seat).Scan(&org, &raw)
	if err != nil {
		return "", nil, notFound(err)
	}
	var policy spec.OrganizationSpec
	if err = json.Unmarshal(raw, &policy); err != nil {
		return "", nil, err
	}
	return org, &policy, nil
}

// workerAdmission checks all task routes, including branches and legacy tasks.
// Uncleaned terminal runs continue to consume capacity until cleanup is confirmed.
func workerAdmission(ctx context.Context, tx pgx.Tx, org, seat, execution string, p *spec.OrganizationSpec, req runtimeapi.TaskRunRequest) error {
	var native bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM native_pool_members WHERE seat_id=$1)`, seat).Scan(&native); err != nil {
		return err
	}
	if native {
		return fmt.Errorf("%w: native pool participants use peer admission, not inherited child workers", ErrInvalid)
	}
	e := p.ElasticTeams
	if e == nil {
		return nil
	}
	if e.Draining {
		return fmt.Errorf("%w: elastic worker admission is draining", ErrConflict)
	}
	if req.TimeoutSeconds > e.MaxTimeoutSeconds {
		return fmt.Errorf("%w: worker timeout exceeds policy maximum %d seconds", ErrInvalid, e.MaxTimeoutSeconds)
	}
	var orgActive, seatActive, executionActive, launches int64
	err := tx.QueryRow(ctx, `SELECT count(*) FILTER(WHERE t.cleaned_at IS NULL), count(*) FILTER(WHERE t.cleaned_at IS NULL AND t.parent_seat_id=$2), count(*) FILTER(WHERE t.cleaned_at IS NULL AND t.parent_execution_id=$3), count(*) FILTER(WHERE t.parent_execution_id=$3) FROM task_runs t JOIN seats s ON s.id=t.parent_seat_id WHERE s.organization_id=$1`, org, seat, execution).Scan(&orgActive, &seatActive, &executionActive, &launches)
	if err != nil {
		return err
	}
	var seatKey string
	if err = tx.QueryRow(ctx, `SELECT key FROM seats WHERE id=$1`, seat).Scan(&seatKey); err != nil {
		return err
	}
	enabled := false
	for _, key := range e.EnabledSeats {
		enabled = enabled || key == seatKey
	}
	if !enabled && (seatActive >= runtimeapi.MaxActiveTaskRuns || launches >= runtimeapi.MaxTaskRunsPerExecution) {
		return fmt.Errorf("%w: legacy worker limit reached for seat not enabled for elastic teams", ErrConflict)
	}
	if orgActive >= int64(e.MaxActiveWorkers) {
		return fmt.Errorf("%w: organization worker capacity %d/%d; wait for cleanup or increase max_active_workers", ErrConflict, orgActive, e.MaxActiveWorkers)
	}
	if executionActive >= int64(e.MaxActivePerExecution) {
		return fmt.Errorf("%w: execution worker capacity %d/%d", ErrConflict, executionActive, e.MaxActivePerExecution)
	}
	if launches >= int64(e.MaxLaunchesPerExecution) {
		return fmt.Errorf("%w: execution launch limit %d/%d (including failed runs)", ErrConflict, launches, e.MaxLaunchesPerExecution)
	}
	b := p.ModelBudgets
	if b == nil || !b.Valid() || b.OrganizationDailyOutputTokens <= 0 || b.ExecutionOutputTokens <= 0 {
		return fmt.Errorf("%w: elastic workers require organization and execution output-token budgets", ErrInvalid)
	}
	allowance := b.MaxRequestOutputTokens
	if allowance == 0 {
		allowance = spec.DefaultMaxRequestOutputTokens
	}
	var day string
	if err = tx.QueryRow(ctx, `SELECT to_char(clock_timestamp() AT TIME ZONE 'UTC','YYYY-MM-DD')`).Scan(&day); err != nil {
		return err
	}
	counts := map[string]int64{"organization": orgActive, "seat": seatActive, "execution": executionActive}
	for _, scope := range modelScopes(org, seat, execution, day, *b) {
		if scope.limit == 0 {
			continue
		}
		var used int64
		if err = tx.QueryRow(ctx, `SELECT COALESCE((SELECT charged_output_tokens FROM model_budget_counters WHERE organization_id=$1 AND scope=$2 AND scope_id=$3 AND period_start=$4::date),0)`, org, scope.name, scope.id, scope.day).Scan(&used); err != nil {
			return err
		}
		// Existing counters include in-flight inference. Require an additional full
		// request allowance for each uncleaned worker, including the new admission.
		needed := (counts[scope.name] + 1) * allowance
		if used > scope.limit || needed > scope.limit-used {
			return fmt.Errorf("%w: %s output-token headroom requires %d tokens for workers; %d available after charged/in-flight usage", ErrConflict, scope.name, needed, scope.limit-used)
		}
	}
	return nil
}

func (s *Store) CreateElasticTeam(ctx context.Context, f Fence, execution string, req runtimeapi.ElasticTeamRequest) (*runtimeapi.ElasticTeam, error) {
	requestID, err := uuid.Parse(req.RequestID)
	if err != nil {
		return nil, fmt.Errorf("%w: request_id must be a UUID", ErrInvalid)
	}
	parentID, err := uuid.Parse(execution)
	if err != nil {
		return nil, fmt.Errorf("%w: active parent execution is required", ErrInvalid)
	}
	execution = parentID.String()
	req.RequestID = requestID.String()
	if err := runtimeapi.ValidateElasticTeam(&req); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	raw, _ := json.Marshal(req)
	sum := sha256.Sum256(raw)
	digest := hex.EncodeToString(sum[:])
	id := uuid.NewString()
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		_, p, err := lockWorkerOrganization(ctx, tx, f.SeatID)
		if err != nil {
			return err
		}
		if err = checkFence(ctx, tx, f); err != nil {
			return err
		}
		var previous, oldExecution string
		err = tx.QueryRow(ctx, `SELECT id,request_sha256,parent_execution_id FROM elastic_teams WHERE parent_seat_id=$1 AND request_id=$2`, f.SeatID, req.RequestID).Scan(&id, &previous, &oldExecution)
		if err == nil {
			if previous != digest || oldExecution != execution {
				return fmt.Errorf("%w: request_id already used for different input or execution", ErrConflict)
			}
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if p.ElasticTeams == nil {
			return fmt.Errorf("%w: elastic teams are not configured", ErrInvalid)
		}
		var key string
		if err = tx.QueryRow(ctx, `SELECT key FROM seats WHERE id=$1`, f.SeatID).Scan(&key); err != nil {
			return err
		}
		enabled := false
		for _, k := range p.ElasticTeams.EnabledSeats {
			enabled = enabled || k == key
		}
		if !enabled {
			return fmt.Errorf("%w: seat is not enabled for elastic teams", ErrInvalid)
		}
		if _, err = tx.Exec(ctx, `INSERT INTO elastic_teams(id,parent_seat_id,parent_execution_id,request_id,request,request_sha256) VALUES($1,$2,$3,$4,$5,$6)`, id, f.SeatID, execution, req.RequestID, raw, digest); err != nil {
			return err
		}
		for i, w := range req.Workers {
			template, ok := p.ElasticTeams.Templates[w.Template]
			if !ok {
				return fmt.Errorf("%w: unknown worker template %q", ErrInvalid, w.Template)
			}
			task := w.Task
			if task.TimeoutSeconds == 0 {
				task.TimeoutSeconds = min(600, p.ElasticTeams.MaxTimeoutSeconds)
			}
			task.Template = w.Template
			task.RequestID = uuid.NewSHA1(requestID, []byte(w.Name)).String()
			{
				for _, name := range task.ToolNames {
					allowed := false
					for _, n := range template.ToolNames {
						allowed = allowed || n == name
					}
					if !allowed {
						return fmt.Errorf("%w: tool %q exceeds template grants", ErrInvalid, name)
					}
				}
			}
			run, err := createTaskRunTx(ctx, tx, f, execution, task)
			if err != nil {
				return err
			}
			if _, err = tx.Exec(ctx, `INSERT INTO elastic_team_members(team_id,task_run_id,name,template,ordinal) VALUES($1,$2,$3,$4,$5)`, id, run.ID, w.Name, w.Template, i); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.GetElasticTeam(ctx, f.SeatID, id)
}

func (s *Store) GetElasticTeam(ctx context.Context, seat, id string) (*runtimeapi.ElasticTeam, error) {
	if _, err := uuid.Parse(id); err != nil {
		return nil, ErrNotFound
	}
	out := &runtimeapi.ElasticTeam{Workers: []runtimeapi.ElasticTeamWorker{}}
	err := pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT id,request_id,parent_execution_id FROM elastic_teams WHERE parent_seat_id=$1 AND id=$2`, seat, id).Scan(&out.ID, &out.RequestID, &out.ParentExecutionID); err != nil {
			return notFound(err)
		}
		rows, err := tx.Query(ctx, `SELECT name,template,task_run_id FROM elastic_team_members WHERE team_id=$1 ORDER BY ordinal`, id)
		if err != nil {
			return err
		}
		type member struct{ name, template, id string }
		var members []member
		for rows.Next() {
			var m member
			if err = rows.Scan(&m.name, &m.template, &m.id); err != nil {
				rows.Close()
				return err
			}
			members = append(members, m)
		}
		rows.Close()
		if err = rows.Err(); err != nil {
			return err
		}
		for _, m := range members {
			run, err := scanTask(tx.QueryRow(ctx, `SELECT `+taskColumns+` FROM task_runs WHERE parent_seat_id=$1 AND id=$2`, seat, m.id))
			if err != nil {
				return err
			}
			out.Workers = append(out.Workers, runtimeapi.ElasticTeamWorker{Name: m.name, Template: m.template, Run: *run})
		}
		return nil
	})
	return out, err
}

func (s *Store) CancelElasticTeam(ctx context.Context, f Fence, id string) error {
	if _, err := uuid.Parse(id); err != nil {
		return ErrNotFound
	}
	return s.inTx(ctx, func(tx pgx.Tx) error {
		if err := checkFence(ctx, tx, f); err != nil {
			return err
		}
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM elastic_teams WHERE parent_seat_id=$1 AND id=$2)`, f.SeatID, id).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return ErrNotFound
		}
		_, err := tx.Exec(ctx, `UPDATE task_runs t SET state='cancelled',finished_at=now() FROM elastic_team_members m WHERE m.team_id=$1 AND m.task_run_id=t.id AND t.parent_seat_id=$2 AND t.state IN ('pending','running')`, id, f.SeatID)
		return err
	})
}
