package memory_test

import (
	"testing"

	"github.com/gimantha/strata/internal/domain"
)

// TestIntegrationRebuildKeepsTheConsolidationCursor covers a coupling that was invisible
// from either side.
//
// projection_checkpoints is shared. The three retrieval projections keep their positions
// there, and so does consolidation — reasonably, since a consolidation cursor is the same
// kind of thing, a derived component's position in the ledger. But DeleteProjections
// cleared the table by workspace, so rebuilding the retrieval indexes silently reset a
// cursor belonging to something that has nothing to do with retrieval.
//
// The damage was bounded rather than absent: consolidation is idempotent, so a reset cursor
// means rescanning the ledger and re-deriving facts that already exist. Bounded silent work
// with no visible cause is still the kind of thing that costs somebody an afternoon.
func TestIntegrationRebuildKeepsTheConsolidationCursor(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	ws := h.scope().WorkspaceID

	const cursor = "01a00000-0000-7000-8000-0000000000ff"
	if err := h.fixture.Store.SaveCheckpoint(ctx, domain.ProjectionCheckpoint{
		WorkspaceID:  ws,
		Projection:   "consolidation",
		LastRecordID: cursor,
	}); err != nil {
		t.Fatalf("save the consolidation cursor: %v", err)
	}

	// A retrieval rebuild. Nothing about it concerns consolidation.
	if _, err := h.projector.Rebuild(ctx, ws); err != nil {
		t.Fatalf("rebuild: %v", err)
	}

	after, err := h.fixture.Store.GetCheckpoint(ctx, ws, "consolidation")
	if err != nil {
		t.Fatalf("read the consolidation cursor: %v", err)
	}
	if after.LastRecordID != cursor {
		t.Fatalf("rebuilding the retrieval projections reset an unrelated component's "+
			"cursor: %q became %q", cursor, after.LastRecordID)
	}

	// And the rebuild did clear its own checkpoints, so the narrowing did not turn into
	// a rebuild that leaves stale positions behind.
	for _, name := range domain.RetrievalProjections() {
		checkpoint, err := h.fixture.Store.GetCheckpoint(ctx, ws, name)
		if err != nil {
			t.Fatalf("read the %s checkpoint: %v", name, err)
		}
		if checkpoint.RebuiltAt == nil {
			t.Errorf("the %s checkpoint was not rewritten by the rebuild", name)
		}
	}
}
