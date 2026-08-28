package index

import (
	"slices"
	"testing"
	"time"

	"github.com/gimantha/strata/internal/domain"
)

// RunGraphConformance is the graph port's contract, which did not exist until now.
//
// The other two ports search a set of records; this one walks a structure, so most of what
// it must promise has no analogue in the other suites. Depth is a hard ceiling and the value
// reported is the shallowest at which an entity was reached. The walk is undirected. Cycles
// terminate. Filters cut paths rather than results, so a filtered-out edge does not merely
// hide one entity — it can make everything behind it unreachable, or change how deep
// something else is reported to be.
//
// Two properties are load-bearing and easy to lose. Roots are never returned, which is what
// makes a walk seeded with another tenant's identifier come back empty instead of confirming
// that the identifier exists. And every hit carries the edge that produced it, which is what
// makes a traversal explainable rather than merely reported.
func RunGraphConformance(t *testing.T, name string, idx Graph, f Fixture) {
	t.Helper()
	ctx := t.Context()

	if f.NewEntities == nil || f.NewAssertions == nil {
		t.Fatal("the graph suite needs a fixture that can mint entities and assertions")
	}

	var (
		past    = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		instant = time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
		future  = time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	)

	// A hub with one neighbour per property under test, plus a chain deep enough to reach
	// the ceiling, a cycle, and an inbound edge.
	const neighbours = 14
	// Room for the hub and its spokes, an inbound edge, a seven-hop chain, and a cycle.
	const identifiers = neighbours + 18
	entities := f.NewEntities(t, identifiers)
	assertions := f.NewAssertions(t, identifiers)

	root := entities[0]
	next := 1
	take := func() (domain.EntityID, domain.AssertionID) {
		t.Helper()
		id, assertion := entities[next], assertions[next]
		next++
		return id, assertion
	}

	edges := []domain.GraphEdge{}
	labels := map[domain.EntityID]string{}

	spoke := func(label string, mutate func(*domain.GraphEdge)) domain.EntityID {
		t.Helper()

		target, assertion := take()
		edge := domain.GraphEdge{
			WorkspaceID: f.Primary.WorkspaceID, GraphSpaceID: f.Primary.GraphSpaceID,
			SubjectID: root, Predicate: "SUPPLIES", ObjectEntityID: target,
			AssertionID: assertion, Status: domain.AssertionActive,
			Classification: domain.ClassificationInternal, MemoryKind: domain.MemorySemantic,
			Confidence: 1,
		}
		mutate(&edge)
		edges = append(edges, edge)
		labels[target] = label
		return target
	}

	spoke("plain", func(*domain.GraphEdge) {})
	spoke("employs", func(e *domain.GraphEdge) { e.Predicate = "EMPLOYS" })
	spoke("disputed", func(e *domain.GraphEdge) { e.Status = domain.AssertionDisputed })
	supersededTarget := spoke("superseded", func(e *domain.GraphEdge) {
		e.Status = domain.AssertionSuperseded
	})
	spoke("restricted", func(e *domain.GraphEdge) {
		e.Classification = domain.ClassificationRestricted
	})
	spoke("sourced", func(e *domain.GraphEdge) { e.SourceID = f.SourceA })
	spoke("collected", func(e *domain.GraphEdge) { e.CollectionID = f.CollectionA })
	spoke("other-collected", func(e *domain.GraphEdge) { e.CollectionID = f.CollectionB })
	spoke("episodic", func(e *domain.GraphEdge) { e.MemoryKind = domain.MemoryEpisodic })
	spoke("valid-window", func(e *domain.GraphEdge) { e.ValidFrom, e.ValidTo = &past, &future })
	spoke("valid-ended", func(e *domain.GraphEdge) { e.ValidFrom, e.ValidTo = &past, &instant })
	spoke("valid-later", func(e *domain.GraphEdge) { e.ValidFrom = &future })
	spoke("inactive", func(e *domain.GraphEdge) { e.ActiveUntil = &past })
	spoke("expired", func(e *domain.GraphEdge) { e.ExpiresAt = &past })

	// An edge pointing at the root rather than away from it: the walk is undirected, so
	// this neighbour is reached exactly like the others.
	inbound, inboundAssertion := take()
	edges = append(edges, domain.GraphEdge{
		WorkspaceID: f.Primary.WorkspaceID, GraphSpaceID: f.Primary.GraphSpaceID,
		SubjectID: inbound, Predicate: "SUPPLIES", ObjectEntityID: root,
		AssertionID: inboundAssertion, Status: domain.AssertionActive,
		Classification: domain.ClassificationInternal, MemoryKind: domain.MemorySemantic,
		Confidence: 1,
	})
	labels[inbound] = "inbound"

	// A chain from its own root, deep enough to run past the ceiling.
	chain := []domain.EntityID{}
	chainRoot, _ := take()
	previous := chainRoot
	for range 7 {
		target, assertion := take()
		edges = append(edges, domain.GraphEdge{
			WorkspaceID: f.Primary.WorkspaceID, GraphSpaceID: f.Primary.GraphSpaceID,
			SubjectID: previous, Predicate: "SUPPLIES", ObjectEntityID: target,
			AssertionID: assertion, Status: domain.AssertionActive,
			Classification: domain.ClassificationInternal, MemoryKind: domain.MemorySemantic,
			Confidence: 1,
		})
		chain = append(chain, target)
		previous = target
	}

	// A cycle, so termination is observable.
	cycleRoot, _ := take()
	cycleA, cycleAssertionA := take()
	cycleB, cycleAssertionB := take()
	closing := assertions[next]
	next++
	for _, edge := range []domain.GraphEdge{
		{SubjectID: cycleRoot, ObjectEntityID: cycleA, AssertionID: cycleAssertionA},
		{SubjectID: cycleA, ObjectEntityID: cycleB, AssertionID: cycleAssertionB},
		{SubjectID: cycleB, ObjectEntityID: cycleRoot, AssertionID: closing},
	} {
		edge.WorkspaceID = f.Primary.WorkspaceID
		edge.GraphSpaceID = f.Primary.GraphSpaceID
		edge.Predicate = "SUPPLIES"
		edge.Status = domain.AssertionActive
		edge.Classification = domain.ClassificationInternal
		edge.MemoryKind = domain.MemorySemantic
		edge.Confidence = 1
		edges = append(edges, edge)
	}

	if err := idx.UpsertEdges(ctx, edges); err != nil {
		t.Fatalf("seed the graph fixture: %v", err)
	}

	// Every hub neighbour, which is what an unfiltered walk of depth one returns —
	// superseded excluded, because the default status set is active and disputed.
	baseline := []string{
		"plain", "employs", "disputed", "restricted", "sourced", "collected",
		"other-collected", "episodic", "valid-window", "valid-ended", "valid-later",
		"inactive", "expired", "inbound",
	}
	without := func(excluded ...string) []string {
		out := make([]string, 0, len(baseline))
		for _, label := range baseline {
			if !slices.Contains(excluded, label) {
				out = append(out, label)
			}
		}
		return out
	}

	base := func() domain.GraphExpandQuery {
		return domain.GraphExpandQuery{
			Scope: f.Primary, Roots: []domain.EntityID{root}, Depth: 1, Limit: 100,
		}
	}

	reached := func(t *testing.T, q domain.GraphExpandQuery) []string {
		t.Helper()

		hits, err := idx.Expand(t.Context(), q)
		if err != nil {
			t.Fatalf("expand: %v", err)
		}
		out := make([]string, 0, len(hits))
		for _, hit := range hits {
			label, known := labels[hit.EntityID]
			if !known {
				label = "unlabelled:" + string(hit.EntityID)
			}
			out = append(out, label)
		}
		slices.Sort(out)
		return out
	}

	cases := []struct {
		name  string
		query func() domain.GraphExpandQuery
		want  []string
	}{
		{"an unfiltered walk", base, without()},
		{
			name: "predicates cut the walk",
			query: func() domain.GraphExpandQuery {
				q := base()
				q.Predicates = []string{"EMPLOYS"}
				return q
			},
			want: []string{"employs"},
		},
		{
			name: "valid at an instant, half-open",
			query: func() domain.GraphExpandQuery {
				q := base()
				q.ValidAt = &instant
				return q
			},
			want: without("valid-ended", "valid-later"),
		},
		{
			name: "active at an instant, two bounds and no third",
			query: func() domain.GraphExpandQuery {
				// Two, not three: an edge has no active_from, so there is no such thing as
				// a not-yet-active relationship. Copying the record-level helper, which
				// tests three, would exclude edges that should be walked.
				q := base()
				q.ActiveAt = &instant
				return q
			},
			want: without("inactive", "expired"),
		},
		{
			name: "superseded is excluded by default and walkable on request",
			query: func() domain.GraphExpandQuery {
				q := base()
				q.IncludeSuperseded = true
				return q
			},
			want: append(without(), "superseded"),
		},
		{
			name: "a classification ceiling",
			query: func() domain.GraphExpandQuery {
				q := base()
				q.Policy = domain.PolicyFilters{MaxClassification: domain.ClassificationInternal}
				return q
			},
			want: without("restricted"),
		},
		{
			name: "allowed sources drop source-less edges",
			query: func() domain.GraphExpandQuery {
				q := base()
				q.Policy = domain.PolicyFilters{AllowedSources: []domain.SourceID{f.SourceA}}
				return q
			},
			want: []string{"sourced"},
		},
		{
			name: "denied sources keep source-less edges",
			query: func() domain.GraphExpandQuery {
				q := base()
				q.Policy = domain.PolicyFilters{DeniedSources: []domain.SourceID{f.SourceA}}
				return q
			},
			want: without("sourced"),
		},
		{
			name: "allowed collections keep collection-less edges",
			query: func() domain.GraphExpandQuery {
				q := base()
				q.Policy = domain.PolicyFilters{
					AllowedCollections: []domain.CollectionID{f.CollectionA},
				}
				return q
			},
			want: without("other-collected"),
		},
		{
			name: "denied collections",
			query: func() domain.GraphExpandQuery {
				q := base()
				q.Policy = domain.PolicyFilters{
					DeniedCollections: []domain.CollectionID{f.CollectionA},
				}
				return q
			},
			want: without("collected"),
		},
		{
			name: "denied memory kinds reach the walk",
			query: func() domain.GraphExpandQuery {
				// They did not until recently: the edge had no memory kind, so a rule
				// about one narrowed every other retrieval path and not this one.
				q := base()
				q.Policy = domain.PolicyFilters{
					DeniedMemoryKinds: []domain.MemoryKind{domain.MemoryEpisodic},
				}
				return q
			},
			want: without("episodic"),
		},
		{
			name: "denied predicates have no blank escape hatch",
			query: func() domain.GraphExpandQuery {
				q := base()
				q.Policy = domain.PolicyFilters{DeniedPredicates: []string{"EMPLOYS"}}
				return q
			},
			want: without("employs"),
		},
		{
			name: "a root from another workspace reaches nothing",
			query: func() domain.GraphExpandQuery {
				// And is not echoed back either. Returning even the bare root would tell
				// one tenant that an entity with that identifier exists in another.
				q := base()
				q.Scope = f.Other
				return q
			},
			want: nil,
		},
		{
			name: "a root that does not exist is not an error",
			query: func() domain.GraphExpandQuery {
				q := base()
				q.Roots = []domain.EntityID{"01a00000-0000-7000-8000-0000000000ff"}
				return q
			},
			want: nil,
		},
		{
			name: "no roots is an empty walk, not a scan",
			query: func() domain.GraphExpandQuery {
				q := base()
				q.Roots = nil
				return q
			},
			want: nil,
		},
	}

	for _, tc := range cases {
		t.Run(name+"/graph: "+tc.name, func(t *testing.T) {
			got := reached(t, tc.query())
			want := slices.Clone(tc.want)
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Errorf("walk reached the wrong set\n  want: %v\n  got:  %v", want, got)
			}
		})
	}

	t.Run(name+"/roots are never returned", func(t *testing.T) {
		hits, err := idx.Expand(t.Context(), base())
		if err != nil {
			t.Fatalf("expand: %v", err)
		}
		for _, hit := range hits {
			if hit.EntityID == root {
				t.Error("the root was returned as a hit")
			}
			if hit.Depth < 1 {
				t.Errorf("a hit at depth %d; only entities reached by an edge are hits", hit.Depth)
			}
		}
	})

	t.Run(name+"/every hit is explainable", func(t *testing.T) {
		hits, err := idx.Expand(t.Context(), base())
		if err != nil {
			t.Fatalf("expand: %v", err)
		}
		if len(hits) == 0 {
			t.Fatal("no hits to check")
		}
		for _, hit := range hits {
			// The three together are what turn a reported entity into a path somebody can
			// argue with, which is the whole reason traversal is in a provenance system.
			if domain.IsZero(hit.ViaAssertion) {
				t.Errorf("%s was reached without naming the claim that connects it", hit.EntityID)
			}
			if hit.ViaPredicate == "" {
				t.Errorf("%s was reached without naming the relationship", hit.EntityID)
			}
			if domain.IsZero(hit.FromEntityID) {
				t.Errorf("%s was reached from nowhere", hit.EntityID)
			}
		}
	})

	t.Run(name+"/depth is a hard ceiling and defaults", func(t *testing.T) {
		deepest := func(q domain.GraphExpandQuery) int {
			t.Helper()
			hits, err := idx.Expand(t.Context(), q)
			if err != nil {
				t.Fatalf("expand: %v", err)
			}
			worst := 0
			for _, hit := range hits {
				worst = max(worst, hit.Depth)
			}
			return worst
		}

		q := domain.GraphExpandQuery{
			Scope: f.Primary, Roots: []domain.EntityID{chainRoot}, Limit: 100,
		}

		// Unset means the default, not unlimited.
		q.Depth = 0
		if got := deepest(q); got != domain.DefaultGraphDepth {
			t.Errorf("an unset depth walked to %d, not the default %d",
				got, domain.DefaultGraphDepth)
		}
		// Negative is the default too, not an error and not zero.
		q.Depth = -5
		if got := deepest(q); got != domain.DefaultGraphDepth {
			t.Errorf("a negative depth walked to %d", got)
		}
		// Beyond the ceiling is the ceiling: a caller cannot widen the walk by asking.
		q.Depth = domain.MaxGraphDepth + 50
		if got := deepest(q); got != domain.MaxGraphDepth {
			t.Errorf("a depth of %d walked to %d, past the ceiling of %d",
				q.Depth, got, domain.MaxGraphDepth)
		}
		// And an ordinary depth stops exactly there.
		q.Depth = 3
		if got := deepest(q); got != 3 {
			t.Errorf("a depth of 3 walked to %d", got)
		}
	})

	t.Run(name+"/a cycle terminates and reports each entity once", func(t *testing.T) {
		hits, err := idx.Expand(t.Context(), domain.GraphExpandQuery{
			Scope: f.Primary, Roots: []domain.EntityID{cycleRoot},
			Depth: domain.MaxGraphDepth, Limit: 100,
		})
		if err != nil {
			t.Fatalf("expand: %v", err)
		}

		seen := map[domain.EntityID]bool{}
		for _, hit := range hits {
			if seen[hit.EntityID] {
				t.Errorf("%s was returned twice; the walk is not deduplicating", hit.EntityID)
			}
			seen[hit.EntityID] = true
			if hit.EntityID == cycleRoot {
				t.Error("the walk came back to its own root and reported it")
			}
		}
		// Both other members, each once, at the shallowest depth they are reachable.
		if len(seen) != 2 {
			t.Errorf("a three-entity cycle from one root reached %d entities, want 2", len(seen))
		}
	})

	t.Run(name+"/upsert is idempotent and refreshes standing", func(t *testing.T) {
		ctx := t.Context()
		before, err := idx.Count(ctx, f.Primary.WorkspaceID)
		if err != nil {
			t.Fatalf("count: %v", err)
		}

		// Keyed by the assertion alone, so a replay converges rather than accumulating.
		replay := edges[0]
		for range 3 {
			if err := idx.UpsertEdges(ctx, []domain.GraphEdge{replay}); err != nil {
				t.Fatalf("replay: %v", err)
			}
		}
		after, err := idx.Count(ctx, f.Primary.WorkspaceID)
		if err != nil {
			t.Fatalf("count after replay: %v", err)
		}
		if after != before {
			t.Errorf("replaying one edge three times changed the count from %d to %d",
				before, after)
		}

		// And a re-upsert carries new standing, which is how a superseded claim stops
		// being walkable without anything being deleted.
		restood := edges[0]
		restood.Status = domain.AssertionSuperseded
		if err := idx.UpsertEdges(ctx, []domain.GraphEdge{restood}); err != nil {
			t.Fatalf("restand: %v", err)
		}
		if got := reached(t, base()); slices.Contains(got, "plain") {
			t.Error("an edge marked superseded is still walked by default")
		}
		// Put it back, so later subtests see the fixture they expect.
		if err := idx.UpsertEdges(ctx, []domain.GraphEdge{edges[0]}); err != nil {
			t.Fatalf("restore: %v", err)
		}
		_ = supersededTarget
	})

	t.Run(name+"/an empty batch is not an error", func(t *testing.T) {
		if err := idx.UpsertEdges(t.Context(), nil); err != nil {
			t.Errorf("an empty batch errored: %v", err)
		}
	})

	t.Run(name+"/purge and count", func(t *testing.T) {
		ctx := t.Context()
		count, err := idx.Count(ctx, f.Primary.WorkspaceID)
		if err != nil {
			t.Fatalf("count: %v", err)
		}
		if count == 0 {
			t.Fatal("count reports nothing with a fixture written")
		}

		if err := idx.Purge(ctx, f.Primary.WorkspaceID); err != nil {
			t.Fatalf("purge: %v", err)
		}
		if count, err = idx.Count(ctx, f.Primary.WorkspaceID); err != nil {
			t.Fatalf("count after purge: %v", err)
		}
		if count != 0 {
			t.Errorf("purge left %d edges", count)
		}
		// Visible to the next read, because a rebuild purges and immediately writes.
		if got := reached(t, base()); len(got) != 0 {
			t.Errorf("a purged graph still walks: %v", got)
		}
	})

	t.Run(name+"/names itself", func(t *testing.T) {
		if idx.Name() == "" {
			t.Fatal("the backend does not identify itself")
		}
	})
}
