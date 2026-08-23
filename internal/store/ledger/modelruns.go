package ledger

import (
	"context"
	"time"

	"github.com/gimantha/strata/internal/domain"
)

const modelRunColumns = `id, workspace_id, graph_space_id, provider, model, prompt_template,
	prompt_version, request_hash, response_hash, prompt_tokens, completion_tokens, total_tokens,
	cost_micros, latency_ms, status, validation_error, response_excerpt, source_event_id, created_at`

// RecordModelRun stores one model interaction.
//
// Runs are recorded whatever their outcome, including failures and rejected output: a run
// that produced nothing usable is the one worth reviewing (AGENTS.md section 13.2).
func (s *Store) RecordModelRun(ctx context.Context, run domain.ModelRun) (domain.ModelRun, error) {
	const op = "ledger.RecordModelRun"

	if domain.IsZero(run.ID) {
		run.ID = domain.ModelRunID(domain.NewUUIDString())
	}
	if err := run.Validate(); err != nil {
		return domain.ModelRun{}, err
	}

	row := s.pool.QueryRow(ctx, `
		INSERT INTO model_runs (id, workspace_id, graph_space_id, provider, model, prompt_template,
		                        prompt_version, request_hash, response_hash, prompt_tokens,
		                        completion_tokens, total_tokens, cost_micros, latency_ms, status,
		                        validation_error, response_excerpt, source_event_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)
		RETURNING `+modelRunColumns,
		run.ID, run.WorkspaceID, nullableString(run.GraphSpaceID), run.Provider, run.Model,
		run.PromptTemplate, run.PromptVersion, run.RequestHash, run.ResponseHash,
		run.PromptTokens, run.CompletionTokens, run.TotalTokens, run.CostMicros,
		run.Latency.Milliseconds(), run.Status, run.ValidationError, run.ResponseExcerpt,
		nullableString(run.SourceEventID))

	return scanModelRun(row, op)
}

// GetModelRun loads one recorded interaction.
func (s *Store) GetModelRun(ctx context.Context, ws domain.WorkspaceID, id domain.ModelRunID) (domain.ModelRun, error) {
	const op = "ledger.GetModelRun"

	row := s.pool.QueryRow(ctx, `SELECT `+modelRunColumns+` FROM model_runs
		WHERE workspace_id = $1 AND id = $2`, ws, id)
	return scanModelRun(row, op)
}

// ListModelRuns returns recent interactions, newest first, optionally only the problematic
// ones.
func (s *Store) ListModelRuns(ctx context.Context, ws domain.WorkspaceID, problemsOnly bool, limit int) ([]domain.ModelRun, error) {
	const op = "ledger.ListModelRuns"

	if limit <= 0 || limit > domain.MaxAssertionLimit {
		limit = domain.DefaultAssertionLimit
	}
	rows, err := s.pool.Query(ctx, `SELECT `+modelRunColumns+` FROM model_runs
		WHERE workspace_id = $1 AND (NOT $2 OR status <> 'succeeded')
		ORDER BY created_at DESC LIMIT $3`, ws, problemsOnly, limit)
	if err != nil {
		return nil, mapError(err, op, "cannot list model runs")
	}
	defer rows.Close()

	var out []domain.ModelRun
	for rows.Next() {
		run, err := scanModelRun(rowAdapter{rows}, op)
		if err != nil {
			return nil, err
		}
		out = append(out, run)
	}
	return out, mapError(rows.Err(), op, "cannot list model runs")
}

func scanModelRun(row interface{ Scan(...any) error }, op string) (domain.ModelRun, error) {
	var (
		run           domain.ModelRun
		graphSpaceID  *string
		sourceEventID *string
		latencyMillis int
	)
	err := row.Scan(&run.ID, &run.WorkspaceID, &graphSpaceID, &run.Provider, &run.Model,
		&run.PromptTemplate, &run.PromptVersion, &run.RequestHash, &run.ResponseHash,
		&run.PromptTokens, &run.CompletionTokens, &run.TotalTokens, &run.CostMicros,
		&latencyMillis, &run.Status, &run.ValidationError, &run.ResponseExcerpt,
		&sourceEventID, &run.CreatedAt)
	if err != nil {
		if isNoRows(err) {
			return domain.ModelRun{}, domain.Errorf(domain.CodeNotFound, op, "model run not found")
		}
		return domain.ModelRun{}, mapError(err, op, "cannot scan model run")
	}

	run.Latency = millisToDuration(latencyMillis)
	if graphSpaceID != nil {
		run.GraphSpaceID = domain.GraphSpaceID(*graphSpaceID)
	}
	if sourceEventID != nil {
		run.SourceEventID = domain.SourceEventID(*sourceEventID)
	}
	return run, nil
}

// millisToDuration restores a latency stored in milliseconds.
func millisToDuration(ms int) time.Duration { return time.Duration(ms) * time.Millisecond }
