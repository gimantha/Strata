package ledger

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gimantha/strata/internal/domain"
)

// ensurePipelineRunTx seeds a pending run so processing status is queryable as soon
// as ingestion is acknowledged. It is idempotent on (workspace, event, version).
func (s *Store) ensurePipelineRunTx(ctx context.Context, tx pgx.Tx, e domain.SourceEvent, pipelineVersion int) error {
	const op = "ledger.ensurePipelineRun"

	_, err := tx.Exec(ctx, `
		INSERT INTO pipeline_runs (id, workspace_id, graph_space_id, source_event_id, pipeline_version, status)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (workspace_id, source_event_id, pipeline_version) DO NOTHING`,
		domain.NewPipelineRunID(), e.WorkspaceID, e.GraphSpaceID, e.ID, pipelineVersion, domain.RunPending)
	return mapError(err, op, "cannot seed pipeline run")
}

const pipelineRunColumns = `id, workspace_id, graph_space_id, source_event_id, pipeline_version,
	status, attempts, last_error, error_class, started_at, finished_at, created_at, updated_at`

// ClaimPipelineRun marks a run as running and increments its attempt counter,
// creating the row if ingestion did not seed one. The returned run is the durable
// handle stages record against.
func (s *Store) ClaimPipelineRun(ctx context.Context, e domain.SourceEvent, pipelineVersion int) (domain.PipelineRun, error) {
	const op = "ledger.ClaimPipelineRun"

	row := s.pool.QueryRow(ctx, `
		INSERT INTO pipeline_runs (id, workspace_id, graph_space_id, source_event_id, pipeline_version,
		                           status, attempts, started_at)
		VALUES ($1, $2, $3, $4, $5, 'running', 1, now())
		ON CONFLICT (workspace_id, source_event_id, pipeline_version) DO UPDATE
		SET status = 'running',
		    attempts = pipeline_runs.attempts + 1,
		    started_at = coalesce(pipeline_runs.started_at, now()),
		    updated_at = now()
		RETURNING `+pipelineRunColumns,
		domain.NewPipelineRunID(), e.WorkspaceID, e.GraphSpaceID, e.ID, pipelineVersion)

	run, err := scanPipelineRun(row, op)
	if err != nil {
		return domain.PipelineRun{}, err
	}
	return run, nil
}

// FinishPipelineRun records a terminal outcome for a run.
func (s *Store) FinishPipelineRun(ctx context.Context, id domain.PipelineRunID, status domain.RunStatus, cause error) error {
	const op = "ledger.FinishPipelineRun"

	if _, err := domain.ParseRunStatus(string(status)); err != nil {
		return err
	}
	lastError, class := describeError(cause)

	tag, err := s.pool.Exec(ctx, `
		UPDATE pipeline_runs
		SET status = $2, last_error = $3, error_class = $4, finished_at = now(), updated_at = now()
		WHERE id = $1`, id, status, lastError, class)
	if err != nil {
		return mapError(err, op, "cannot finish pipeline run")
	}
	if tag.RowsAffected() == 0 {
		return domain.Errorf(domain.CodeNotFound, op, "pipeline run not found")
	}
	return nil
}

// LatestPipelineRun returns the highest-version run for an event.
func (s *Store) LatestPipelineRun(ctx context.Context, ws domain.WorkspaceID, eventID domain.SourceEventID) (domain.PipelineRun, bool, error) {
	const op = "ledger.LatestPipelineRun"

	row := s.pool.QueryRow(ctx, `SELECT `+pipelineRunColumns+` FROM pipeline_runs
		WHERE workspace_id = $1 AND source_event_id = $2
		ORDER BY pipeline_version DESC LIMIT 1`, ws, eventID)

	run, err := scanPipelineRun(row, op)
	if err != nil {
		if domain.IsCode(err, domain.CodeNotFound) {
			return domain.PipelineRun{}, false, nil
		}
		return domain.PipelineRun{}, false, err
	}
	return run, true, nil
}

func scanPipelineRun(row pgx.Row, op string) (domain.PipelineRun, error) {
	var r domain.PipelineRun
	err := row.Scan(&r.ID, &r.WorkspaceID, &r.GraphSpaceID, &r.SourceEventID, &r.PipelineVersion,
		&r.Status, &r.Attempts, &r.LastError, &r.ErrorClass, &r.StartedAt, &r.FinishedAt,
		&r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		if isNoRows(err) {
			return domain.PipelineRun{}, domain.Errorf(domain.CodeNotFound, op, "pipeline run not found")
		}
		return domain.PipelineRun{}, mapError(err, op, "cannot scan pipeline run")
	}
	return r, nil
}

const stageRunColumns = `id, pipeline_run_id, workspace_id, source_event_id, stage_name, stage_version,
	status, attempts, output_ref, last_error, error_class, started_at, finished_at, created_at, updated_at`

// BeginStageRun claims a stage for execution and reports whether the stage already
// succeeded.
//
// This is the durable stage execution key from AGENTS.md section 10.4: re-running a
// succeeded stage returns its recorded output instead of executing again, unless the
// caller forces it. That is what makes the whole pipeline safe to replay.
func (s *Store) BeginStageRun(ctx context.Context, runID domain.PipelineRunID, ws domain.WorkspaceID, eventID domain.SourceEventID, stageName string, stageVersion int, force bool) (domain.StageRun, bool, error) {
	const op = "ledger.BeginStageRun"

	if !force {
		row := s.pool.QueryRow(ctx, `SELECT `+stageRunColumns+` FROM pipeline_stage_runs
			WHERE pipeline_run_id = $1 AND stage_name = $2 AND stage_version = $3`,
			runID, stageName, stageVersion)
		existing, err := scanStageRun(row, op)
		if err == nil && existing.Status == domain.RunSucceeded {
			return existing, true, nil
		}
		if err != nil && !domain.IsCode(err, domain.CodeNotFound) {
			return domain.StageRun{}, false, err
		}
	}

	row := s.pool.QueryRow(ctx, `
		INSERT INTO pipeline_stage_runs (id, pipeline_run_id, workspace_id, source_event_id,
		                                 stage_name, stage_version, status, attempts, started_at)
		VALUES ($1, $2, $3, $4, $5, $6, 'running', 1, now())
		ON CONFLICT (pipeline_run_id, stage_name, stage_version) DO UPDATE
		SET status = 'running',
		    attempts = pipeline_stage_runs.attempts + 1,
		    started_at = coalesce(pipeline_stage_runs.started_at, now()),
		    updated_at = now()
		RETURNING `+stageRunColumns,
		domain.NewStageRunID(), runID, ws, eventID, stageName, stageVersion)

	run, err := scanStageRun(row, op)
	if err != nil {
		return domain.StageRun{}, false, err
	}
	return run, false, nil
}

// FinishStageRun records a stage outcome and its output summary.
func (s *Store) FinishStageRun(ctx context.Context, id domain.StageRunID, status domain.RunStatus, outputRef any, cause error) error {
	const op = "ledger.FinishStageRun"

	if _, err := domain.ParseRunStatus(string(status)); err != nil {
		return err
	}
	payload := []byte("{}")
	if outputRef != nil {
		encoded, err := jsonValue(outputRef)
		if err != nil {
			return domain.Wrap(err, domain.CodeInternal, op, "cannot encode stage output")
		}
		payload = encoded
	}
	lastError, class := describeError(cause)

	tag, err := s.pool.Exec(ctx, `
		UPDATE pipeline_stage_runs
		SET status = $2, output_ref = $3, last_error = $4, error_class = $5,
		    finished_at = now(), updated_at = now()
		WHERE id = $1`, id, status, payload, lastError, class)
	if err != nil {
		return mapError(err, op, "cannot finish stage run")
	}
	if tag.RowsAffected() == 0 {
		return domain.Errorf(domain.CodeNotFound, op, "stage run not found")
	}
	return nil
}

// ListStageRuns returns a run's stages in execution order.
func (s *Store) ListStageRuns(ctx context.Context, runID domain.PipelineRunID) ([]domain.StageRun, error) {
	const op = "ledger.ListStageRuns"

	rows, err := s.pool.Query(ctx, `SELECT `+stageRunColumns+` FROM pipeline_stage_runs
		WHERE pipeline_run_id = $1 ORDER BY created_at, stage_name`, runID)
	if err != nil {
		return nil, mapError(err, op, "cannot list stage runs")
	}
	defer rows.Close()

	var out []domain.StageRun
	for rows.Next() {
		var (
			r      domain.StageRun
			output []byte
		)
		if err := rows.Scan(&r.ID, &r.PipelineRunID, &r.WorkspaceID, &r.SourceEventID, &r.StageName,
			&r.StageVersion, &r.Status, &r.Attempts, &output, &r.LastError, &r.ErrorClass,
			&r.StartedAt, &r.FinishedAt, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, mapError(err, op, "cannot scan stage run")
		}
		r.OutputRef = json.RawMessage(output)
		out = append(out, r)
	}
	return out, mapError(rows.Err(), op, "cannot list stage runs")
}

func scanStageRun(row pgx.Row, op string) (domain.StageRun, error) {
	var (
		r      domain.StageRun
		output []byte
	)
	err := row.Scan(&r.ID, &r.PipelineRunID, &r.WorkspaceID, &r.SourceEventID, &r.StageName,
		&r.StageVersion, &r.Status, &r.Attempts, &output, &r.LastError, &r.ErrorClass,
		&r.StartedAt, &r.FinishedAt, &r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		if isNoRows(err) {
			return domain.StageRun{}, domain.Errorf(domain.CodeNotFound, op, "stage run not found")
		}
		return domain.StageRun{}, mapError(err, op, "cannot scan stage run")
	}
	r.OutputRef = json.RawMessage(output)
	return r, nil
}

// describeError renders an error for durable storage: a bounded message plus its
// retry class. Messages are truncated because a provider can return an entire
// response body, and a failure record is not a place to archive one.
func describeError(err error) (string, domain.ErrorClass) {
	if err == nil {
		return "", ""
	}
	const maxLen = 2000
	msg := err.Error()
	if len(msg) > maxLen {
		msg = msg[:maxLen] + "...(truncated)"
	}
	return msg, domain.ClassifyError(err)
}

// nowUTC exists so time handling stays consistent when tests inject clocks.
func nowUTC() time.Time { return time.Now().UTC() }
