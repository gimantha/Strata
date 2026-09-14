package purge_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gimantha/strata/internal/domain"
	"github.com/gimantha/strata/internal/projection"
	"github.com/gimantha/strata/internal/purge"
)

type fakeStore struct {
	planCalls, executeCalls int
	finish                  domain.KnowledgePurgeStatus
}

func (f *fakeStore) PlanKnowledgePurge(_ context.Context, req domain.KnowledgePurgeRequest) (domain.KnowledgePurgeRun, error) {
	f.planCalls++
	if err := req.Validate(); err != nil {
		return domain.KnowledgePurgeRun{}, err
	}
	status := domain.PurgePlanned
	if req.DryRun {
		status = domain.PurgeCompleted
	}
	return domain.KnowledgePurgeRun{ID: "purge", Scope: req.Scope, DryRun: req.DryRun,
		Status: status, Eligible: domain.KnowledgePurgeCounts{Events: 2}}, nil
}

func (f *fakeStore) ExecuteKnowledgePurge(_ context.Context, ws domain.WorkspaceID, id domain.KnowledgePurgeID) (domain.KnowledgePurgeRun, []string, error) {
	f.executeCalls++
	return domain.KnowledgePurgeRun{ID: id,
			Scope:  domain.Scope{WorkspaceID: ws, GraphSpaceID: "graph"},
			Status: domain.PurgeCanonicalDeleted, Deleted: domain.KnowledgePurgeCounts{Events: 2}},
		[]string{"blob-a"}, nil
}

func (f *fakeStore) FinishKnowledgePurge(_ context.Context, _ domain.WorkspaceID, _ domain.KnowledgePurgeID, status domain.KnowledgePurgeStatus, _ string, _ int64, _ time.Time) error {
	f.finish = status
	return nil
}

func (f *fakeStore) CreateKnowledgeHold(_ context.Context, hold domain.KnowledgeHold) (domain.KnowledgeHold, error) {
	return hold, nil
}

func (f *fakeStore) ReleaseKnowledgeHold(context.Context, domain.Scope, domain.KnowledgeHoldID, domain.PrincipalRef) error {
	return nil
}

type fakeBlobs struct {
	deleted []string
	err     error
}

func (f *fakeBlobs) Delete(_ context.Context, key string) error {
	f.deleted = append(f.deleted, key)
	return f.err
}

type fakeProjector struct {
	calls int
	err   error
}

func (f *fakeProjector) Rebuild(context.Context, domain.WorkspaceID) (projection.Stats, error) {
	f.calls++
	return projection.Stats{}, f.err
}

func purgeRequest(before time.Time, dry bool) domain.KnowledgePurgeRequest {
	return domain.KnowledgePurgeRequest{
		Scope:  domain.Scope{WorkspaceID: "workspace", GraphSpaceID: "graph"},
		Before: before, Principal: domain.PrincipalRef{ID: "admin", Kind: domain.PrincipalUser},
		Reason: "retention policy", DryRun: dry,
	}
}

func TestDryRunRecordsPlanWithoutDeletingAnything(t *testing.T) {
	now := time.Date(2026, 9, 14, 0, 0, 0, 0, time.UTC)
	store, blobs, projector := &fakeStore{}, &fakeBlobs{}, &fakeProjector{}
	service := purge.New(store, blobs, projector, purge.Options{Clock: func() time.Time { return now }}, nil)
	run, err := service.Run(t.Context(), purgeRequest(now.Add(-90*24*time.Hour), true))
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if run.Status != domain.PurgeCompleted || store.planCalls != 1 || store.executeCalls != 0 || projector.calls != 0 || len(blobs.deleted) != 0 {
		t.Fatalf("dry run crossed deletion boundary: run=%+v store=%+v projector=%+v blobs=%+v", run, store, projector, blobs)
	}
}

func TestRunDeletesBlobsAndRebuildsProjections(t *testing.T) {
	now := time.Date(2026, 9, 14, 0, 0, 0, 0, time.UTC)
	store, blobs, projector := &fakeStore{}, &fakeBlobs{}, &fakeProjector{}
	service := purge.New(store, blobs, projector, purge.Options{Clock: func() time.Time { return now }}, nil)
	run, err := service.Run(t.Context(), purgeRequest(now.Add(-90*24*time.Hour), false))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if run.Status != domain.PurgeCompleted || store.finish != domain.PurgeCompleted || projector.calls != 1 || len(blobs.deleted) != 1 {
		t.Fatalf("cleanup did not complete: run=%+v store=%+v projector=%+v blobs=%+v", run, store, projector, blobs)
	}
}

func TestCleanupFailureIsDurableAndRetryable(t *testing.T) {
	now := time.Date(2026, 9, 14, 0, 0, 0, 0, time.UTC)
	store := &fakeStore{}
	blobs := &fakeBlobs{err: errors.New("object store unavailable")}
	projector := &fakeProjector{}
	service := purge.New(store, blobs, projector, purge.Options{Clock: func() time.Time { return now }}, nil)
	run, err := service.Run(t.Context(), purgeRequest(now.Add(-90*24*time.Hour), false))
	if err == nil || run.Status != domain.PurgeFailed || store.finish != domain.PurgeFailed || projector.calls != 1 {
		t.Fatalf("partial cleanup was not recorded: run=%+v err=%v", run, err)
	}
	blobs.err = nil
	run, err = service.Retry(t.Context(), purgeRequest(now, false).Scope, "purge")
	if err != nil || run.Status != domain.PurgeCompleted || store.finish != domain.PurgeCompleted || store.executeCalls != 2 {
		t.Fatalf("retry did not resume cleanup: run=%+v err=%v store=%+v", run, err, store)
	}
}

func TestRecentCutoffIsRejectedBeforePlanning(t *testing.T) {
	now := time.Date(2026, 9, 14, 0, 0, 0, 0, time.UTC)
	store := &fakeStore{}
	service := purge.New(store, &fakeBlobs{}, &fakeProjector{},
		purge.Options{Clock: func() time.Time { return now }}, nil)
	if _, err := service.Run(t.Context(), purgeRequest(now.Add(-24*time.Hour), true)); err == nil {
		t.Fatal("the default 30-day safety window accepted a one-day-old cutoff")
	}
	if store.planCalls != 0 {
		t.Fatal("an unsafe cutoff reached the ledger")
	}
}
