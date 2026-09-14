// Package purge physically removes old knowledge after its useful and historical lifetime
// has ended. It is separate from memory deactivation and retraction because those preserve
// the immutable ledger and remain temporally queryable.
package purge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/gimantha/strata/internal/domain"
	"github.com/gimantha/strata/internal/projection"
)

// Store is the durable purge workflow owned by the canonical ledger.
type Store interface {
	PlanKnowledgePurge(context.Context, domain.KnowledgePurgeRequest) (domain.KnowledgePurgeRun, error)
	ExecuteKnowledgePurge(context.Context, domain.WorkspaceID, domain.KnowledgePurgeID) (domain.KnowledgePurgeRun, []string, error)
	FinishKnowledgePurge(context.Context, domain.WorkspaceID, domain.KnowledgePurgeID, domain.KnowledgePurgeStatus, string, int64, time.Time) error
	CreateKnowledgeHold(context.Context, domain.KnowledgeHold) (domain.KnowledgeHold, error)
	ReleaseKnowledgeHold(context.Context, domain.Scope, domain.KnowledgeHoldID, domain.PrincipalRef) error
}

type BlobStore interface {
	Delete(context.Context, string) error
}

type Projector interface {
	Rebuild(context.Context, domain.WorkspaceID) (projection.Stats, error)
}

type Options struct {
	// MinAge is the safety window between now and the cutoff. It defaults to 30 days so a
	// misplaced date cannot turn routine retention into deletion of recent knowledge.
	MinAge time.Duration
	Clock  func() time.Time
}

type Service struct {
	store     Store
	blobs     BlobStore
	projector Projector
	minAge    time.Duration
	now       func() time.Time
	logger    *slog.Logger
}

func New(store Store, blobs BlobStore, projector Projector, opts Options, logger *slog.Logger) *Service {
	if opts.MinAge <= 0 {
		opts.MinAge = 30 * 24 * time.Hour
	}
	if opts.Clock == nil {
		opts.Clock = func() time.Time { return time.Now().UTC() }
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Service{store: store, blobs: blobs, projector: projector, minAge: opts.MinAge,
		now: opts.Clock, logger: logger}
}

// Run always records a plan. Dry-run is the default at API/CLI boundaries; setting DryRun
// false crosses the canonical deletion boundary and then cleans every derived store.
func (s *Service) Run(ctx context.Context, req domain.KnowledgePurgeRequest) (domain.KnowledgePurgeRun, error) {
	now := s.now().UTC()
	req.RequestedAt = now
	if req.Before.After(now.Add(-s.minAge)) {
		return domain.KnowledgePurgeRun{}, domain.Errorf(domain.CodeInvalidArgument, "purge.Run",
			"before must be at least %s older than requested_at", s.minAge)
	}
	plan, err := s.store.PlanKnowledgePurge(ctx, req)
	if err != nil || req.DryRun {
		return plan, err
	}

	run, keys, err := s.store.ExecuteKnowledgePurge(ctx, req.Scope.WorkspaceID, plan.ID)
	if err != nil {
		return run, err
	}
	return s.cleanup(ctx, run, keys, now)
}

// Retry resumes external cleanup for a run whose canonical deletion already committed.
// ExecuteKnowledgePurge returns the persisted blob keys and counts in that state.
func (s *Service) Retry(ctx context.Context, scope domain.Scope, id domain.KnowledgePurgeID) (domain.KnowledgePurgeRun, error) {
	run, keys, err := s.store.ExecuteKnowledgePurge(ctx, scope.WorkspaceID, id)
	if err != nil {
		return run, err
	}
	if run.Scope.GraphSpaceID != scope.GraphSpaceID {
		return domain.KnowledgePurgeRun{}, domain.Errorf(domain.CodeNotFound, "purge.Retry",
			"knowledge purge not found")
	}
	return s.cleanup(ctx, run, keys, s.now().UTC())
}

func (s *Service) cleanup(ctx context.Context, run domain.KnowledgePurgeRun, keys []string, now time.Time) (domain.KnowledgePurgeRun, error) {
	var cleanup []error
	var blobsDeleted int64
	for _, key := range keys {
		if err := s.blobs.Delete(ctx, key); err != nil {
			cleanup = append(cleanup, fmt.Errorf("delete blob %q: %w", key, err))
		} else {
			blobsDeleted++
		}
	}
	// Rebuilding the whole workspace also cleans external indexes. It is deliberately done
	// even for an empty retry, because a prior attempt may have committed canonical deletion
	// and failed between external backends.
	if _, err := s.projector.Rebuild(ctx, run.Scope.WorkspaceID); err != nil {
		cleanup = append(cleanup, fmt.Errorf("rebuild projections: %w", err))
	}
	if joined := errors.Join(cleanup...); joined != nil {
		message := joined.Error()
		if finishErr := s.store.FinishKnowledgePurge(ctx, run.Scope.WorkspaceID, run.ID,
			domain.PurgeFailed, message, blobsDeleted, now); finishErr != nil {
			return run, errors.Join(joined, finishErr)
		}
		run.Status, run.LastError = domain.PurgeFailed, message
		run.Deleted.Blobs = blobsDeleted
		run.CompletedAt = &now
		s.logger.ErrorContext(ctx, "knowledge purge cleanup incomplete",
			slog.String("purge_id", string(run.ID)), slog.String("error", message))
		return run, domain.Wrap(joined, domain.CodeProviderUnavailable, "purge.cleanup",
			fmt.Sprintf("purge %s cleanup incomplete; retry this run", run.ID))
	}
	if err := s.store.FinishKnowledgePurge(ctx, run.Scope.WorkspaceID, run.ID,
		domain.PurgeCompleted, "", blobsDeleted, now); err != nil {
		return run, err
	}
	run.Status, run.CompletedAt = domain.PurgeCompleted, &now
	run.Deleted.Blobs = blobsDeleted
	return run, nil
}

func (s *Service) Hold(ctx context.Context, hold domain.KnowledgeHold) (domain.KnowledgeHold, error) {
	if hold.ExpiresAt != nil && !hold.ExpiresAt.After(s.now()) {
		return domain.KnowledgeHold{}, domain.Errorf(domain.CodeInvalidArgument, "purge.Hold",
			"expires_at must be in the future")
	}
	return s.store.CreateKnowledgeHold(ctx, hold)
}

func (s *Service) Release(ctx context.Context, scope domain.Scope, id domain.KnowledgeHoldID, actor domain.PrincipalRef) error {
	return s.store.ReleaseKnowledgeHold(ctx, scope, id, actor)
}
