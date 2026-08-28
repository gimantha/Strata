package index

import (
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gimantha/strata/internal/domain"
)

// RunLexicalFilterConformance pins what every filter on domain.LexicalQuery means.
//
// The companion to RunVectorFilterConformance, and it exists for the same reason: the shape
// suite would pass for a backend that ignored every policy filter, and filters are where two
// search engines actually diverge. Several of the cases below are asymmetries that read as
// inconsistencies until the reason is stated, which is why each says what breaks without it.
//
// Two things have no vector analogue and are pinned here. A lexical hit carries Content where
// a vector hit does not, and the two search modes order differently — exact by score, then
// content length, then record id; full text by score then record id. A backend that used one
// ordering for both would look correct until two documents tied.
func RunLexicalFilterConformance(t *testing.T, name string, idx Lexical, f Fixture) {
	t.Helper()
	ctx := t.Context()

	var (
		past    = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		instant = time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
		future  = time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	)

	// One phrase every record contains, so a query matches the whole corpus and only a
	// filter separates one record from another.
	const shared = "quarterly logistics dossier"

	corpus := map[string]domain.ProjectedRecord{}
	record := func(label string, mutate func(*domain.ProjectedRecord)) {
		t.Helper()

		r := domain.ProjectedRecord{
			Scope:          f.Primary,
			Surface:        domain.SurfaceAssertion,
			RecordID:       domain.NewUUIDString(),
			Content:        shared + " " + label,
			Status:         string(domain.AssertionActive),
			Classification: domain.ClassificationInternal,
			MemoryKind:     domain.MemorySemantic,
		}
		mutate(&r)
		corpus[label] = r
	}

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
	record("collected", func(r *domain.ProjectedRecord) { r.Scope.CollectionID = f.CollectionA })
	record("other-collected", func(r *domain.ProjectedRecord) {
		r.Scope.CollectionID = f.CollectionB
	})
	record("valid-window", func(r *domain.ProjectedRecord) {
		r.ValidFrom, r.ValidTo = &past, &future
	})
	record("valid-ended", func(r *domain.ProjectedRecord) {
		r.ValidFrom, r.ValidTo = &past, &instant
	})
	record("valid-later", func(r *domain.ProjectedRecord) { r.ValidFrom = &future })
	record("inactive", func(r *domain.ProjectedRecord) { r.Lifecycle.ActiveUntil = &past })
	record("not-yet-active", func(r *domain.ProjectedRecord) { r.Lifecycle.ActiveFrom = &future })
	record("expired", func(r *domain.ProjectedRecord) { r.Lifecycle.ExpiresAt = &past })

	records := slices.Collect(maps.Values(corpus))
	if err := idx.Upsert(ctx, records); err != nil {
		t.Fatalf("seed the filter fixture: %v", err)
	}

	all := slices.Collect(maps.Keys(corpus))
	without := func(excluded ...string) []string {
		out := make([]string, 0, len(all))
		for _, label := range all {
			if !slices.Contains(excluded, label) {
				out = append(out, label)
			}
		}
		return out
	}

	base := func() domain.LexicalQuery {
		return domain.LexicalQuery{Scope: f.Primary, Text: shared, Limit: 100}
	}

	byRecordID := map[string]string{}
	for label, r := range corpus {
		byRecordID[r.RecordID] = label
	}

	cases := []struct {
		name  string
		query func() domain.LexicalQuery
		want  []string
	}{
		{"no filters", base, without()},
		{
			name: "exact mode finds the same set",
			query: func() domain.LexicalQuery {
				// Substring rather than stemmed matching, but over the same phrase, so
				// the two modes must agree about membership even though they differ
				// about order.
				q := base()
				q.Exact = true
				return q
			},
			want: without(),
		},
		{
			name: "surfaces",
			query: func() domain.LexicalQuery {
				q := base()
				q.Surfaces = []domain.Surface{domain.SurfaceChunk}
				return q
			},
			want: []string{"chunk"},
		},
		{
			name: "a graph space narrows, and an unset one does not",
			query: func() domain.LexicalQuery {
				q := base()
				q.Scope = f.Primary
				return q
			},
			want: without(),
		},
		{
			name: "statuses",
			query: func() domain.LexicalQuery {
				// No implicit "active only": an unset status filter returns every status,
				// superseded included.
				q := base()
				q.Statuses = []string{string(domain.AssertionSuperseded)}
				return q
			},
			want: []string{"superseded"},
		},
		{
			name: "classification is a membership test, not a ceiling",
			query: func() domain.LexicalQuery {
				// Unlike Policy.MaxClassification below, which is a ceiling expanded into
				// a set. Asking for internal here does not also admit public.
				q := base()
				q.Classification = []domain.Classification{domain.ClassificationRestricted}
				return q
			},
			want: []string{"restricted"},
		},
		{
			name: "memory kinds",
			query: func() domain.LexicalQuery {
				q := base()
				q.MemoryKinds = []domain.MemoryKind{domain.MemoryEpisodic}
				return q
			},
			want: []string{"episodic"},
		},
		{
			name: "entity types at query level exclude records with none",
			query: func() domain.LexicalQuery {
				q := base()
				q.EntityTypes = []string{"organization"}
				return q
			},
			want: []string{"entity-typed"},
		},
		{
			name: "valid at an instant, half-open",
			query: func() domain.LexicalQuery {
				q := base()
				q.ValidAt = &instant
				return q
			},
			want: without("valid-ended", "valid-later"),
		},
		{
			name: "active at an instant, three bounds",
			query: func() domain.LexicalQuery {
				// Three, unlike graph edges, which have no active_from.
				q := base()
				q.ActiveAt = &instant
				return q
			},
			want: without("inactive", "not-yet-active", "expired"),
		},
		{
			name: "a classification ceiling",
			query: func() domain.LexicalQuery {
				q := base()
				q.Policy = domain.PolicyFilters{MaxClassification: domain.ClassificationInternal}
				return q
			},
			want: without("restricted"),
		},
		{
			name: "allowed sources drop records with no source",
			query: func() domain.LexicalQuery {
				q := base()
				q.Policy = domain.PolicyFilters{AllowedSources: []domain.SourceID{f.SourceA}}
				return q
			},
			want: []string{"sourced"},
		},
		{
			name: "denied sources keep records with no source",
			query: func() domain.LexicalQuery {
				q := base()
				q.Policy = domain.PolicyFilters{DeniedSources: []domain.SourceID{f.SourceA}}
				return q
			},
			want: without("sourced"),
		},
		{
			name: "allowed collections keep records in no collection",
			query: func() domain.LexicalQuery {
				// The asymmetry with sources above, and the one most likely to be got
				// wrong by a backend sharing a helper between them.
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
			query: func() domain.LexicalQuery {
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
			query: func() domain.LexicalQuery {
				q := base()
				q.Policy = domain.PolicyFilters{AllowedEntityTypes: []string{"organization"}}
				return q
			},
			want: without(),
		},
		{
			name: "allowed predicates admit records with none",
			query: func() domain.LexicalQuery {
				q := base()
				q.Policy = domain.PolicyFilters{AllowedPredicates: []string{"SUPPLIES"}}
				return q
			},
			want: without(),
		},
		{
			name: "denied predicates",
			query: func() domain.LexicalQuery {
				q := base()
				q.Policy = domain.PolicyFilters{DeniedPredicates: []string{"SUPPLIES"}}
				return q
			},
			want: without("predicated"),
		},
		{
			name: "allowed memory kinds have no escape hatch",
			query: func() domain.LexicalQuery {
				q := base()
				q.Policy = domain.PolicyFilters{
					AllowedMemoryKinds: []domain.MemoryKind{domain.MemoryEpisodic},
				}
				return q
			},
			want: []string{"episodic"},
		},
		{
			name: "any restrictive policy installs a classification ceiling",
			query: func() domain.LexicalQuery {
				q := base()
				q.Policy = domain.PolicyFilters{
					DeniedCollections: []domain.CollectionID{f.CollectionA},
				}
				return q
			},
			want: without("collected"),
		},
		{
			name: "another workspace sees nothing",
			query: func() domain.LexicalQuery {
				q := base()
				q.Scope = f.Other
				return q
			},
			want: nil,
		},
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

	t.Run(name+"/exact matching is literal", func(t *testing.T) {
		ctx := t.Context()
		// The characters exact mode exists for. Both are LIKE wildcards, so a backend
		// building a pattern from the caller's text finds the lookalike as well.
		literal := domain.ProjectedRecord{
			Scope: f.Primary, Surface: domain.SurfaceChunk,
			RecordID: domain.NewUUIDString(), Content: "the build failed with ERR_7731X",
			Status: string(domain.AssertionActive), Classification: domain.ClassificationInternal,
			MemoryKind: domain.MemorySemantic,
		}
		lookalike := literal
		lookalike.RecordID = domain.NewUUIDString()
		lookalike.Content = "the build failed with ERRX7731X"

		if err := idx.Upsert(ctx, []domain.ProjectedRecord{literal, lookalike}); err != nil {
			t.Fatalf("upsert: %v", err)
		}

		hits, err := idx.Search(ctx, domain.LexicalQuery{
			Scope: f.Primary, Text: "ERR_7731X", Exact: true, Limit: 50,
		})
		if err != nil {
			t.Fatalf("search: %v", err)
		}
		for _, hit := range hits {
			if hit.RecordID == lookalike.RecordID {
				t.Error("an underscore matched any character; exact mode is not literal")
			}
		}
		found := false
		for _, hit := range hits {
			if hit.RecordID == literal.RecordID {
				found = true
			}
		}
		if !found {
			t.Error("the identifier itself was not found by an exact search for it")
		}

		// A bare wildcard is a search for a wildcard, not for everything.
		hits, err = idx.Search(ctx, domain.LexicalQuery{
			Scope: f.Primary, Text: "%", Exact: true, Limit: 50,
		})
		if err != nil {
			t.Fatalf("wildcard search: %v", err)
		}
		if len(hits) != 0 {
			t.Errorf("a search for %%%% returned %d records; it matched everything", len(hits))
		}
	})

	t.Run(name+"/hit shape", func(t *testing.T) {
		hits, err := idx.Search(t.Context(), base())
		if err != nil {
			t.Fatalf("search: %v", err)
		}
		if len(hits) == 0 {
			t.Fatal("no hits, so the shape cannot be checked")
		}

		for _, hit := range hits {
			// Unlike a vector hit, a lexical hit carries its text: the index holds the
			// content it matched, so making the caller fetch it again would be a second
			// read for something already in hand.
			if hit.Content == "" {
				t.Fatalf("a lexical hit carries no content: %+v", hit)
			}
			if !strings.Contains(hit.Content, shared) {
				t.Fatalf("a hit's content is not what was written: %q", hit.Content)
			}
			if hit.Detail["retriever"] != "lexical" {
				t.Fatalf("hit detail does not name the retriever: %v", hit.Detail)
			}
		}

		exact := base()
		exact.Exact = true
		hits, err = idx.Search(t.Context(), exact)
		if err != nil {
			t.Fatalf("exact search: %v", err)
		}
		if len(hits) == 0 {
			t.Fatal("exact mode found nothing")
		}
		// The two modes are distinguishable in the result, because a caller explaining a
		// ranking needs to know which one produced it.
		if hits[0].Detail["retriever"] != "lexical_exact" {
			t.Fatalf("exact mode reports itself as %v", hits[0].Detail["retriever"])
		}
	})

	t.Run(name+"/empty text is an error, not an empty result", func(t *testing.T) {
		for _, text := range []string{"", "   "} {
			q := base()
			q.Text = text
			if _, err := idx.Search(t.Context(), q); err == nil {
				t.Errorf("a search for %q was accepted", text)
			}
		}
	})

	t.Run(name+"/a term nothing contains returns nothing", func(t *testing.T) {
		for _, exact := range []bool{false, true} {
			q := base()
			q.Text = "zqxjklmnprstv"
			q.Exact = exact
			hits, err := idx.Search(t.Context(), q)
			if err != nil {
				t.Errorf("exact=%v: a query matching nothing errored: %v", exact, err)
			}
			if len(hits) != 0 {
				t.Errorf("exact=%v: expected no hits, got %d", exact, len(hits))
			}
		}
	})
}
