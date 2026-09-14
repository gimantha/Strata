package retrieval

import (
	"context"
	"testing"
	"time"

	"github.com/gimantha/strata/internal/domain"
)

type capturedTrace struct{ trace domain.RetrievalTrace }

func (c *capturedTrace) RecordTrace(_ context.Context, trace domain.RetrievalTrace) (domain.RetrievalTrace, error) {
	trace.ID = domain.TraceID("trace-1")
	c.trace = trace
	return trace, nil
}

func TestTracePreservesPlanAndRankingEvidence(t *testing.T) {
	sink := &capturedTrace{}
	r := &Retriever{traces: sink, now: func() time.Time { return time.Unix(100, 0).UTC() }}
	req := domain.QueryRequest{Scope: domain.Scope{
		WorkspaceID: domain.WorkspaceID("workspace-1"), GraphSpaceID: domain.GraphSpaceID("graph-1"),
	}, Query: "why did CASE-42 change?"}
	plan := domain.RetrievalPlan{
		Planner: "heuristic", FocusTerms: []string{"CASE-42"},
		Requirements: []string{"decision_trace"},
	}
	path := &domain.GraphPath{FromEntityID: domain.EntityID("entity-1"), Depth: 1}
	candidates := []candidate{{
		mode: domain.ModeExact, rank: 1, score: .93,
		hit: domain.Hit{Surface: domain.SurfaceAssertion, RecordID: "assertion-1"}, path: path,
	}}
	items := []domain.RetrievedItem{{
		Surface: domain.SurfaceAssertion, RecordID: "assertion-1", Score: .1,
		FoundBy: []domain.RetrievalMode{domain.ModeExact},
		Ranks:   map[domain.RetrievalMode]int{domain.ModeExact: 1},
		Signals: map[string]float64{"exact_rrf": .1}, Path: path,
	}}

	if got := r.record(context.Background(), req, plan, candidates, items, 12*time.Millisecond); got != "trace-1" {
		t.Fatalf("trace id = %q", got)
	}
	if sink.trace.Plan.Planner != "heuristic" || len(sink.trace.Plan.FocusTerms) != 1 {
		t.Fatalf("plan was not retained: %+v", sink.trace.Plan)
	}
	if got := sink.trace.CandidateRefs[0]; got.Ranks[domain.ModeExact] != 1 || got.Signals["raw_score"] != .93 || got.Path == nil {
		t.Fatalf("candidate evidence was not retained: %+v", got)
	}
	if got := sink.trace.SelectedRefs[0]; got.Signals["exact_rrf"] != .1 || got.Path == nil {
		t.Fatalf("selection evidence was not retained: %+v", got)
	}
}
