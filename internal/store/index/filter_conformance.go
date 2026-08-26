package index

import (
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/gimantha/strata/internal/domain"
)

// RunVectorFilterConformance pins what every filter on domain.VectorQuery means.
//
// RunVectorConformance covers the shape of the port — write, read, converge, purge. It
// exercises seven of VectorQuery's fourteen fields and none of PolicyFilters' thirteen, so a
// backend that leaked restricted classifications, ignored denied sources, or hid every
// passage behind an entity-type rule would pass it green. For a port whose whole purpose is
// substitution that is the gap that matters, because the filters are where two engines with
// different query languages actually diverge.
//
// This is also not hypothetical: writing it turned up that the reference implementation was
// silently ignoring collection restrictions, which is a policy hole rather than a portability
// one. A suite that only a newcomer runs cannot find that.
//
// Every record here carries the same embedding, so distance is constant and the only thing
// separating one result from another is a filter. Cases are declared as a query plus the
// exact set of records that must come back — not "at least these", because a filter that
// admits too much fails in the direction that matters.
func RunVectorFilterConformance(t *testing.T, name string, idx Vectors, f Fixture) {
	t.Helper()
	ctx := t.Context()

	var (
		past    = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		instant = time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
		future  = time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	)

	embedding := f.Embed(t, "one embedding shared by every record in this fixture")
	corpus := map[string]domain.ProjectedRecord{}
	record := func(label string, mutate func(*domain.ProjectedRecord)) {
		t.Helper()

		r := domain.ProjectedRecord{
			Scope:          f.Primary,
			Surface:        domain.SurfaceAssertion,
			RecordID:       domain.NewUUIDString(),
			Content:        label,
			Status:         string(domain.AssertionActive),
			Classification: domain.ClassificationInternal,
			MemoryKind:     domain.MemorySemantic,
		}
		mutate(&r)
		corpus[label] = r
	}

	// One record per property under test, each differing from the baseline in one way.
	record("baseline", func(*domain.ProjectedRecord) {})
	record("chunk", func(r *domain.ProjectedRecord) { r.Surface = domain.SurfaceChunk })
	record("entity-typed", func(r *domain.ProjectedRecord) { r.EntityType = "organization" })
	record("predicated", func(r *domain.ProjectedRecord) { r.Predicate = "SUPPLIES" })
	record("episodic", func(r *domain.ProjectedRecord) { r.MemoryKind = domain.MemoryEpisodic })
	record("restricted", func(r *domain.ProjectedRecord) {
		r.Classification = domain.ClassificationRestricted
	})
	record("superseded", func(r *domain.ProjectedRecord) {
		r.Status = string(domain.AssertionSuperseded)
	})
	record("sourced", func(r *domain.ProjectedRecord) { r.SourceID = f.SourceA })
	record("other-sourced", func(r *domain.ProjectedRecord) { r.SourceID = f.SourceB })
	record("collected", func(r *domain.ProjectedRecord) { r.Scope.CollectionID = f.CollectionA })
	record("other-collected", func(r *domain.ProjectedRecord) { r.Scope.CollectionID = f.CollectionB })
	// Validity is half-open: valid_from is inclusive and valid_to is exclusive, so a
	// record valid until the instant is not valid at it.
	record("valid-window", func(r *domain.ProjectedRecord) {
		r.ValidFrom, r.ValidTo = &past, &future
	})
	record("valid-ended", func(r *domain.ProjectedRecord) {
		r.ValidFrom, r.ValidTo = &past, &instant
	})
	record("valid-later", func(r *domain.ProjectedRecord) { r.ValidFrom = &future })
	// Lifecycle has three bounds, all independently able to remove a record from an
	// active-at query, and expiry is the hard one.
	record("inactive", func(r *domain.ProjectedRecord) { r.Lifecycle.ActiveUntil = &past })
	record("not-yet-active", func(r *domain.ProjectedRecord) { r.Lifecycle.ActiveFrom = &future })
	record("expired", func(r *domain.ProjectedRecord) { r.Lifecycle.ExpiresAt = &past })

	// Written under a distinct model, to pin that Model and Version are coupled.
	otherModel := domain.ProjectedRecord{
		Scope: f.Primary, Surface: domain.SurfaceAssertion,
		RecordID: domain.NewUUIDString(), Content: "other-model",
		Status: string(domain.AssertionActive), Classification: domain.ClassificationInternal,
		MemoryKind: domain.MemorySemantic,
	}
	corpus["other-model"] = otherModel

	vectors := make([]domain.VectorRecord, 0, len(corpus))
	for label, r := range corpus {
		v := domain.VectorRecord{
			ProjectedRecord: r,
			Model:           f.Model,
			Version:         f.Version,
			Embedding:       embedding,
			ContentHash:     domain.ContentHash([]byte(label)),
		}
		if label == "other-model" {
			v.Model, v.Version = f.Model+"-alternate", f.Version+1
		}
		vectors = append(vectors, v)
	}
	if err := idx.Upsert(ctx, vectors); err != nil {
		t.Fatalf("seed the filter fixture: %v", err)
	}

	// Everything except the record written under another model, which no default query
	// reaches because the default query names a model.
	all := slices.Collect(maps.Keys(corpus))
	without := func(excluded ...string) []string {
		out := make([]string, 0, len(all))
		for _, label := range all {
			if !slices.Contains(excluded, label) && label != "other-model" {
				out = append(out, label)
			}
		}
		return out
	}

	base := func() domain.VectorQuery {
		return domain.VectorQuery{
			Scope: f.Primary, Embedding: embedding, Limit: 100,
			Model: f.Model, Version: f.Version,
		}
	}

	cases := []struct {
		name  string
		query func() domain.VectorQuery
		want  []string
	}{
		{
			name:  "no filters beyond the model",
			query: base,
			want:  without(),
		},
		{
			name: "surfaces",
			query: func() domain.VectorQuery {
				q := base()
				q.Surfaces = []domain.Surface{domain.SurfaceChunk}
				return q
			},
			want: []string{"chunk"},
		},
		{
			name: "an empty model searches every model",
			query: func() domain.VectorQuery {
				q := base()
				q.Model, q.Version = "", 0
				return q
			},
			want: all,
		},
		{
			name: "a version without a model is not applied on its own",
			query: func() domain.VectorQuery {
				// The reference couples them: both clauses live inside `if Model != ""`.
				// A backend filtering on version alone returns strictly less.
				q := base()
				q.Model, q.Version = "", 99
				return q
			},
			want: all,
		},
		{
			name: "statuses",
			query: func() domain.VectorQuery {
				q := base()
				q.Statuses = []string{string(domain.AssertionSuperseded)}
				return q
			},
			want: []string{"superseded"},
		},
		{
			name: "classification, at query level",
			query: func() domain.VectorQuery {
				q := base()
				q.Classification = []domain.Classification{domain.ClassificationRestricted}
				return q
			},
			want: []string{"restricted"},
		},
		{
			name: "memory kinds, at query level",
			query: func() domain.VectorQuery {
				q := base()
				q.MemoryKinds = []domain.MemoryKind{domain.MemoryEpisodic}
				return q
			},
			want: []string{"episodic"},
		},
		{
			name: "entity types at query level exclude records with none",
			query: func() domain.VectorQuery {
				// Deliberately unlike the policy filter below: the query-level filter has
				// no empty-string escape hatch, so it excludes passages rather than
				// admitting them. A backend using one helper for both breaks one of them.
				q := base()
				q.EntityTypes = []string{"organization"}
				return q
			},
			want: []string{"entity-typed"},
		},
		{
			name: "valid at an instant, half-open",
			query: func() domain.VectorQuery {
				q := base()
				q.ValidAt = &instant
				return q
			},
			// valid-ended stops exactly at the instant and is excluded; valid-later has
			// not started. Everything with no validity at all is unbounded and matches.
			want: without("valid-ended", "valid-later"),
		},
		{
			name: "active at an instant, three bounds",
			query: func() domain.VectorQuery {
				q := base()
				q.ActiveAt = &instant
				return q
			},
			want: without("inactive", "not-yet-active", "expired"),
		},
		{
			name: "a classification ceiling",
			query: func() domain.VectorQuery {
				q := base()
				q.Policy = domain.PolicyFilters{MaxClassification: domain.ClassificationInternal}
				return q
			},
			want: without("restricted"),
		},
		{
			name: "a denied classification",
			query: func() domain.VectorQuery {
				q := base()
				q.Policy = domain.PolicyFilters{
					MaxClassification:     domain.ClassificationSecret,
					DeniedClassifications: []domain.Classification{domain.ClassificationRestricted},
				}
				return q
			},
			want: without("restricted"),
		},
		{
			name: "allowed sources exclude records with no source",
			query: func() domain.VectorQuery {
				// The asymmetry worth pinning: an allow-list on sources drops a
				// source-less record, while a deny-list keeps it. They are not mirrors.
				q := base()
				q.Policy = domain.PolicyFilters{AllowedSources: []domain.SourceID{f.SourceA}}
				return q
			},
			want: []string{"sourced"},
		},
		{
			name: "denied sources keep records with no source",
			query: func() domain.VectorQuery {
				q := base()
				q.Policy = domain.PolicyFilters{DeniedSources: []domain.SourceID{f.SourceA}}
				return q
			},
			want: without("sourced"),
		},
		{
			name: "allowed collections keep records in no collection",
			query: func() domain.VectorQuery {
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
			query: func() domain.VectorQuery {
				q := base()
				q.Policy = domain.PolicyFilters{
					DeniedCollections: []domain.CollectionID{f.CollectionA},
				}
				return q
			},
			want: without("collected"),
		},
		{
			name: "allowed entity types admit records with none",
			query: func() domain.VectorQuery {
				// The mirror image of the query-level case above, and the reason they
				// cannot share an implementation.
				q := base()
				q.Policy = domain.PolicyFilters{AllowedEntityTypes: []string{"organization"}}
				return q
			},
			want: without(),
		},
		{
			name: "denied entity types",
			query: func() domain.VectorQuery {
				q := base()
				q.Policy = domain.PolicyFilters{DeniedEntityTypes: []string{"organization"}}
				return q
			},
			want: without("entity-typed"),
		},
		{
			name: "allowed predicates admit records with none",
			query: func() domain.VectorQuery {
				q := base()
				q.Policy = domain.PolicyFilters{AllowedPredicates: []string{"SUPPLIES"}}
				return q
			},
			want: without(),
		},
		{
			name: "denied predicates",
			query: func() domain.VectorQuery {
				q := base()
				q.Policy = domain.PolicyFilters{DeniedPredicates: []string{"SUPPLIES"}}
				return q
			},
			want: without("predicated"),
		},
		{
			name: "allowed memory kinds have no escape hatch",
			query: func() domain.VectorQuery {
				// Unlike entity types and predicates: memory_kind is always set, so an
				// allow-list means exactly what it says.
				q := base()
				q.Policy = domain.PolicyFilters{
					AllowedMemoryKinds: []domain.MemoryKind{domain.MemoryEpisodic},
				}
				return q
			},
			want: []string{"episodic"},
		},
		{
			name: "denied memory kinds",
			query: func() domain.VectorQuery {
				q := base()
				q.Policy = domain.PolicyFilters{
					DeniedMemoryKinds: []domain.MemoryKind{domain.MemoryEpisodic},
				}
				return q
			},
			want: without("episodic"),
		},
		{
			name: "any restrictive policy installs a classification ceiling",
			query: func() domain.VectorQuery {
				// A policy naming only collections still counts as restrictive, and the
				// reference then always applies the permitted-classification set with its
				// default ceiling. Reproducing that matters: it is why a record whose
				// classification is unrecognised is invisible under any policy at all.
				q := base()
				q.Policy = domain.PolicyFilters{
					DeniedCollections: []domain.CollectionID{f.CollectionA},
				}
				return q
			},
			want: without("collected"),
		},
		{
			name: "filters combine as intersection, not union",
			query: func() domain.VectorQuery {
				q := base()
				q.Surfaces = []domain.Surface{domain.SurfaceAssertion}
				q.MemoryKinds = []domain.MemoryKind{domain.MemoryEpisodic}
				return q
			},
			want: []string{"episodic"},
		},
	}

	byRecordID := map[string]string{}
	for label, r := range corpus {
		byRecordID[r.RecordID] = label
	}

	for _, tc := range cases {
		t.Run(name+"/filter: "+tc.name, func(t *testing.T) {
			hits, err := idx.Search(t.Context(), tc.query())
			if err != nil {
				t.Fatalf("search: %v", err)
			}

			got := make([]string, 0, len(hits))
			for _, hit := range hits {
				label, known := byRecordID[hit.RecordID]
				if !known {
					t.Fatalf("a record this fixture never wrote came back: %s", hit.RecordID)
				}
				got = append(got, label)
			}
			slices.Sort(got)

			want := slices.Clone(tc.want)
			slices.Sort(want)

			if !slices.Equal(got, want) {
				t.Errorf("filter returned the wrong set\n  want: %v\n  got:  %v", want, got)
			}
		})
	}

	// The shape of a hit, which callers depend on and no other subtest pins.
	t.Run(name+"/hit shape", func(t *testing.T) {
		hits, err := idx.Search(t.Context(), base())
		if err != nil {
			t.Fatalf("search: %v", err)
		}
		if len(hits) == 0 {
			t.Fatal("no hits, so the shape cannot be checked")
		}

		hit := hits[0]
		if hit.Surface == "" {
			t.Error("a hit carries no surface")
		}
		// Content is deliberately empty: the vector index returns identifiers and scores,
		// and hydrating text is the caller's job. A backend filling it in would make the
		// two implementations disagree about payload size for every query.
		if hit.Content != "" {
			t.Errorf("a vector hit carries content: %q", hit.Content)
		}
		if hit.Score <= 0 {
			t.Errorf("an identical embedding scored %v; the metric is not cosine similarity",
				hit.Score)
		}
		if hit.Detail["retriever"] != "vector" {
			t.Errorf("hit detail does not name the retriever: %v", hit.Detail)
		}
		if _, ok := hit.Detail["cosine_similarity"]; !ok {
			t.Errorf("hit detail does not report the similarity: %v", hit.Detail)
		}
	})

	// MinScore is applied inclusively, and its zero value is not "no threshold".
	t.Run(name+"/min score", func(t *testing.T) {
		q := base()
		q.MinScore = 0.999
		hits, err := idx.Search(t.Context(), q)
		if err != nil {
			t.Fatalf("search: %v", err)
		}
		if len(hits) == 0 {
			t.Fatal("an identical embedding was dropped by a floor below its score")
		}

		q.MinScore = 1.5
		hits, err = idx.Search(t.Context(), q)
		if err != nil {
			t.Fatalf("search above the maximum: %v", err)
		}
		if len(hits) != 0 {
			t.Errorf("a floor above any possible score returned %d hits", len(hits))
		}
	})
}
