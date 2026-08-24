package app

import (
	"context"

	"github.com/gimantha/strata/internal/domain"
	"github.com/gimantha/strata/internal/ingest"
	"github.com/gimantha/strata/internal/normalize"
	"github.com/gimantha/strata/internal/pipeline"
)

// importRecorder attributes an imported package to a real ingestion event.
//
// It goes through the gateway rather than writing an event directly, so an import gets the
// same archival, idempotency, and outbox commit as any other source material. A package
// imported twice returns the same event, which is what makes re-import safe.
type importRecorder struct {
	gateway *ingest.Gateway
	runner  *pipeline.Runner
	store   episodeLister
}

// episodeLister reads back the episode the pipeline created for the package.
type episodeLister interface {
	ListEpisodes(ctx context.Context, ws domain.WorkspaceID, eventID domain.SourceEventID) ([]domain.Episode, error)
}

func (r importRecorder) RecordImport(ctx context.Context, scope domain.Scope, principal domain.PrincipalRef, sourceID domain.SourceID, payload []byte, idempotencyKey string) (domain.SourceEventID, domain.EpisodeID, error) {
	receipt, err := r.gateway.Accept(ctx, ingest.Request{
		Scope:          scope,
		Principal:      principal,
		SourceID:       sourceID,
		EventType:      "package.import",
		Operation:      domain.SourceOpSnapshot,
		MediaType:      normalize.MediaTypeJSON,
		Payload:        payload,
		IdempotencyKey: idempotencyKey,
		// The package header is the archived material, not the knowledge itself: the
		// claims are committed individually so each one keeps its own evidence. Marking
		// it a direct episode stops the pipeline from trying to extract facts from a
		// manifest.
		DirectEpisode: true,
	})
	if err != nil {
		return "", "", err
	}

	// Processed synchronously, because the claims committed next must cite an episode that
	// already exists. Waiting for the worker would leave an import either blocked on a
	// queue or committing evidence that points at nothing.
	if _, err := r.runner.Process(ctx, scope.WorkspaceID, receipt.SourceEventID, false); err != nil {
		return "", "", err
	}

	episodes, err := r.store.ListEpisodes(ctx, scope.WorkspaceID, receipt.SourceEventID)
	if err != nil {
		return "", "", err
	}
	if len(episodes) == 0 {
		return "", "", domain.Errorf(domain.CodeInternal, "app.RecordImport",
			"the archived package produced no episode to cite")
	}
	return receipt.SourceEventID, episodes[0].ID, nil
}
