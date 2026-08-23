package pipeline

import (
	"context"

	"github.com/gimantha/strata/internal/domain"
	"github.com/gimantha/strata/internal/projection"
)

// Projector writes canonical records into the retrieval projections.
type Projector interface {
	ProjectEvent(ctx context.Context, scope domain.Scope, eventID domain.SourceEventID) (projection.Stats, error)
	ProjectEntities(ctx context.Context, scope domain.Scope) (projection.Stats, error)
	Advance(ctx context.Context, ws domain.WorkspaceID, event domain.SourceEvent, stats projection.Stats) error
}

// ProjectStage indexes an event's derived records for retrieval.
//
// It runs last, after knowledge has been committed, because it projects what the ledger
// holds rather than what the pipeline happens to have in memory. That is also what makes it
// re-runnable: the stage reads canonical state, so running it again converges rather than
// duplicating (AGENTS.md section 15.2).
type ProjectStage struct {
	projector Projector
	cfg       StageConfig
}

// NewProjectStage builds the stage.
func NewProjectStage(projector Projector, cfg StageConfig) ProjectStage {
	return ProjectStage{projector: projector, cfg: cfg}
}

func (ProjectStage) Name() string { return "project" }

// Version is part of the durable stage key. Bump it when what gets projected changes in a
// way that should re-index events already processed.
func (ProjectStage) Version() int { return 1 }

func (s ProjectStage) Execute(ctx context.Context, in Input) (Output, error) {
	stats, err := s.projector.ProjectEvent(ctx, in.Scope, in.Event.ID)
	if err != nil {
		return Output{}, err
	}

	// Entities are not tied to one event, so they are refreshed alongside it. Upserts make
	// this idempotent, and the cost is bounded by how many identities the graph space holds.
	entityStats, err := s.projector.ProjectEntities(ctx, in.Scope)
	if err != nil {
		return Output{}, err
	}
	stats.Add(entityStats)

	if err := s.projector.Advance(ctx, in.Event.WorkspaceID, in.Event, stats); err != nil {
		return Output{}, err
	}

	return Output{Summary: map[string]any{
		"vectors":  stats.Vectors,
		"lexical":  stats.Lexical,
		"edges":    stats.Edges,
		"embedded": stats.Embedded,
		"reused":   stats.Reused,
	}}, nil
}
