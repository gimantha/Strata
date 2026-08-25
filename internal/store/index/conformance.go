package index

import (
	"testing"
	"time"

	"github.com/gimantha/strata/internal/domain"
)

// Conformance suites, one per port, following ADR 0020.
//
// Two implementations compiling against one interface prove nothing about behaviour: the
// compiler checks signatures and says nothing about what the methods do. These suites say
// what they must do, and every backend runs them — including PostgreSQL, so the bar is set
// by the contract rather than by whatever the first substitute's author assumed.
//
// Exported from a non-test file, again per ADR 0020, so a backend living outside this
// repository can run them.
//
// What each suite covers is chosen from where implementations genuinely diverge: whether a
// replayed write converges or duplicates, whether filters narrow before ranking or after,
// whether an empty result is an error, and whether workspace scope is enforced by the port
// or assumed of the caller. Those are the things a second backend gets wrong.

// Fixture supplies a conformance run with the scopes and records it needs.
//
// A backend's own test provides this, because only it knows how to reach a clean store.
type Fixture struct {
	// Primary and Other are two workspaces. Isolation is checked between them, so they
	// must be genuinely distinct rather than two names for one.
	Primary domain.Scope
	Other   domain.Scope
	// Embed turns text into a vector of the width the backend expects. Vector suites need
	// real vectors; a backend with a different dimensionality supplies its own.
	Embed func(tb testing.TB, text string) []float32
	// Model and Version identify the embedding these vectors came from.
	Model   string
	Version int
}

// RunVectorConformance exercises the behaviour every vector backend must share.
func RunVectorConformance(t *testing.T, name string, idx Vectors, f Fixture) {
	t.Helper()

	t.Run(name+"/upsert then search", func(t *testing.T) {
		ctx := t.Context()
		record := vectorRecord(t, f, f.Primary,
			"industrial fasteners supplied under a long term agreement")

		if err := idx.Upsert(ctx, []domain.VectorRecord{record}); err != nil {
			t.Fatalf("upsert: %v", err)
		}
		hits, err := idx.Search(ctx, domain.VectorQuery{
			Scope: f.Primary, Embedding: record.Embedding, Limit: 10,
			Model: f.Model, Version: f.Version,
		})
		if err != nil {
			t.Fatalf("search: %v", err)
		}
		if !containsRecord(hits, record.RecordID) {
			t.Fatalf("a record was written and not found: %v", hits)
		}
	})

	t.Run(name+"/upsert is idempotent", func(t *testing.T) {
		ctx := t.Context()
		record := vectorRecord(t, f, f.Primary,
			"the same text written twice")

		for range 3 {
			if err := idx.Upsert(ctx, []domain.VectorRecord{record}); err != nil {
				t.Fatalf("upsert: %v", err)
			}
		}

		// Every rebuild replays the whole ledger, so a backend that appended instead of
		// replacing would grow without bound and return the same record several times.
		hits, err := idx.Search(ctx, domain.VectorQuery{
			Scope: f.Primary, Embedding: record.Embedding, Limit: 10,
			Model: f.Model, Version: f.Version,
		})
		if err != nil {
			t.Fatalf("search: %v", err)
		}
		if n := countRecord(hits, record.RecordID); n != 1 {
			t.Fatalf("a record written three times appears %d times; replay does not converge", n)
		}
	})

	t.Run(name+"/metadata refresh without re-embedding", func(t *testing.T) {
		ctx := t.Context()
		record := vectorRecord(t, f, f.Primary,
			"a claim that will be deactivated")

		if err := idx.Upsert(ctx, []domain.VectorRecord{record}); err != nil {
			t.Fatalf("upsert: %v", err)
		}

		// Deactivate it: same text, different standing. This is the operation whose
		// absence let a forgotten claim stay reachable through the vector leg.
		past := time.Now().UTC().Add(-time.Hour)
		deactivated := record.ProjectedRecord
		deactivated.Lifecycle.ActiveUntil = &past

		if err := idx.RefreshMetadata(ctx, f.Model, f.Version,
			[]domain.ProjectedRecord{deactivated}); err != nil {
			t.Fatalf("refresh metadata: %v", err)
		}

		now := time.Now().UTC()
		hits, err := idx.Search(ctx, domain.VectorQuery{
			Scope: f.Primary, Embedding: record.Embedding, Limit: 10,
			Model: f.Model, Version: f.Version, ActiveAt: &now,
		})
		if err != nil {
			t.Fatalf("search: %v", err)
		}
		if containsRecord(hits, record.RecordID) {
			t.Fatal("a record whose active window has closed is still returned for an " +
				"active-at query; the metadata refresh did not reach the filter")
		}

		// And it is still there without the lifecycle filter: a refresh changes standing,
		// it does not delete.
		hits, err = idx.Search(ctx, domain.VectorQuery{
			Scope: f.Primary, Embedding: record.Embedding, Limit: 10,
			Model: f.Model, Version: f.Version,
		})
		if err != nil {
			t.Fatalf("search without a lifecycle filter: %v", err)
		}
		if !containsRecord(hits, record.RecordID) {
			t.Fatal("the metadata refresh removed the record instead of restanding it")
		}
	})

	t.Run(name+"/existing hashes are stored with the vector", func(t *testing.T) {
		ctx := t.Context()
		record := vectorRecord(t, f, f.Primary,
			"text with a known hash")

		if err := idx.Upsert(ctx, []domain.VectorRecord{record}); err != nil {
			t.Fatalf("upsert: %v", err)
		}

		hashes, err := idx.ExistingHashes(ctx, f.Primary.WorkspaceID, f.Model, f.Version,
			record.Surface, []string{record.RecordID, "01a00000-0000-7000-8000-0000000000aa"})
		if err != nil {
			t.Fatalf("existing hashes: %v", err)
		}
		if hashes[record.RecordID] != record.ContentHash {
			t.Fatalf("stored hash %q, got %q", record.ContentHash, hashes[record.RecordID])
		}
		// An id that was never written must be absent, not empty-valued: the caller uses
		// presence to decide whether to embed.
		if _, present := hashes["01a00000-0000-7000-8000-0000000000aa"]; present {
			t.Fatal("a record that was never written has a hash")
		}
	})

	t.Run(name+"/workspace isolation", func(t *testing.T) {
		ctx := t.Context()
		mine := vectorRecord(t, f, f.Primary,
			"a record belonging to one tenant")

		if err := idx.Upsert(ctx, []domain.VectorRecord{mine}); err != nil {
			t.Fatalf("upsert: %v", err)
		}

		// Scope is the port's obligation, not the caller's. A backend that ignored it
		// would leak across tenants for every query in the system at once.
		hits, err := idx.Search(ctx, domain.VectorQuery{
			Scope: f.Other, Embedding: mine.Embedding, Limit: 10,
			Model: f.Model, Version: f.Version,
		})
		if err != nil {
			t.Fatalf("search from the other workspace: %v", err)
		}
		if containsRecord(hits, mine.RecordID) {
			t.Fatal("a record was returned to a different workspace")
		}

		hashes, err := idx.ExistingHashes(ctx, f.Other.WorkspaceID, f.Model, f.Version,
			mine.Surface, []string{mine.RecordID})
		if err != nil {
			t.Fatalf("existing hashes from the other workspace: %v", err)
		}
		if len(hashes) != 0 {
			t.Fatal("another workspace's hashes are visible")
		}
	})

	t.Run(name+"/purge and count", func(t *testing.T) {
		ctx := t.Context()
		record := vectorRecord(t, f, f.Other,
			"a record to be purged")

		if err := idx.Upsert(ctx, []domain.VectorRecord{record}); err != nil {
			t.Fatalf("upsert: %v", err)
		}
		count, err := idx.Count(ctx, f.Other.WorkspaceID)
		if err != nil {
			t.Fatalf("count: %v", err)
		}
		if count == 0 {
			t.Fatal("count reports nothing after a write")
		}

		if err := idx.Purge(ctx, f.Other.WorkspaceID); err != nil {
			t.Fatalf("purge: %v", err)
		}
		count, err = idx.Count(ctx, f.Other.WorkspaceID)
		if err != nil {
			t.Fatalf("count after purge: %v", err)
		}
		if count != 0 {
			t.Fatalf("purge left %d records", count)
		}

		// A purge must be visible to the next read. A backend indexing asynchronously has
		// to make it so, because a rebuild purges and immediately starts writing.
		hits, err := idx.Search(ctx, domain.VectorQuery{
			Scope: f.Other, Embedding: record.Embedding, Limit: 10,
			Model: f.Model, Version: f.Version,
		})
		if err != nil {
			t.Fatalf("search after purge: %v", err)
		}
		if containsRecord(hits, record.RecordID) {
			t.Fatal("a purged record is still searchable")
		}
	})

	t.Run(name+"/empty result is not an error", func(t *testing.T) {
		ctx := t.Context()
		hits, err := idx.Search(ctx, domain.VectorQuery{
			Scope:     f.Primary,
			Embedding: f.Embed(t, "a phrase nothing in this corpus resembles at all"),
			Model:     f.Model, Version: f.Version, Limit: 10,
			// A floor no distant neighbour can clear, so the result is genuinely empty
			// rather than merely irrelevant.
			MinScore: 0.999999,
		})
		if err != nil {
			t.Fatalf("a query matching nothing returned an error: %v", err)
		}
		if len(hits) != 0 {
			t.Fatalf("expected no hits above the floor, got %d", len(hits))
		}
	})

	t.Run(name+"/names itself", func(t *testing.T) {
		if idx.Name() == "" {
			t.Fatal("the backend does not identify itself")
		}
	})
}

// RunLexicalConformance exercises the behaviour every lexical backend must share.
func RunLexicalConformance(t *testing.T, name string, idx Lexical, f Fixture) {
	t.Helper()

	t.Run(name+"/upsert then search", func(t *testing.T) {
		ctx := t.Context()
		record := lexicalRecord(f.Primary,
			"Acme Corporation supplies industrial fasteners")

		if err := idx.Upsert(ctx, []domain.ProjectedRecord{record}); err != nil {
			t.Fatalf("upsert: %v", err)
		}
		hits, err := idx.Search(ctx, domain.LexicalQuery{
			Scope: f.Primary, Text: "industrial fasteners", Limit: 10,
		})
		if err != nil {
			t.Fatalf("search: %v", err)
		}
		if !containsRecord(hits, record.RecordID) {
			t.Fatalf("a record was written and not found: %v", hits)
		}
	})

	t.Run(name+"/exact matching finds what stemming mangles", func(t *testing.T) {
		ctx := t.Context()
		record := lexicalRecord(f.Primary,
			"the build failed with error code ERR_7731X during linking")

		if err := idx.Upsert(ctx, []domain.ProjectedRecord{record}); err != nil {
			t.Fatalf("upsert: %v", err)
		}
		// The reason the port has one method with a flag rather than two indexes: an
		// identifier has to be findable, and full-text stemming destroys it.
		hits, err := idx.Search(ctx, domain.LexicalQuery{
			Scope: f.Primary, Text: "ERR_7731X", Exact: true, Limit: 10,
		})
		if err != nil {
			t.Fatalf("exact search: %v", err)
		}
		if !containsRecord(hits, record.RecordID) {
			t.Fatal("an identifier was not found by exact search")
		}
	})

	t.Run(name+"/upsert is idempotent", func(t *testing.T) {
		ctx := t.Context()
		record := lexicalRecord(f.Primary,
			"written repeatedly by every rebuild")

		for range 3 {
			if err := idx.Upsert(ctx, []domain.ProjectedRecord{record}); err != nil {
				t.Fatalf("upsert: %v", err)
			}
		}
		hits, err := idx.Search(ctx, domain.LexicalQuery{
			Scope: f.Primary, Text: "written repeatedly", Limit: 10,
		})
		if err != nil {
			t.Fatalf("search: %v", err)
		}
		if n := countRecord(hits, record.RecordID); n != 1 {
			t.Fatalf("a record written three times appears %d times", n)
		}
	})

	t.Run(name+"/lifecycle narrows before ranking", func(t *testing.T) {
		ctx := t.Context()
		record := lexicalRecord(f.Primary,
			"a note that stops being current")
		past := time.Now().UTC().Add(-time.Hour)
		record.Lifecycle.ActiveUntil = &past

		if err := idx.Upsert(ctx, []domain.ProjectedRecord{record}); err != nil {
			t.Fatalf("upsert: %v", err)
		}
		now := time.Now().UTC()
		hits, err := idx.Search(ctx, domain.LexicalQuery{
			Scope: f.Primary, Text: "stops being current", Limit: 10, ActiveAt: &now,
		})
		if err != nil {
			t.Fatalf("search: %v", err)
		}
		if containsRecord(hits, record.RecordID) {
			t.Fatal("a record whose active window has closed was returned")
		}
	})

	t.Run(name+"/workspace isolation", func(t *testing.T) {
		ctx := t.Context()
		mine := lexicalRecord(f.Primary,
			"a distinctive phrase belonging to one tenant")

		if err := idx.Upsert(ctx, []domain.ProjectedRecord{mine}); err != nil {
			t.Fatalf("upsert: %v", err)
		}
		hits, err := idx.Search(ctx, domain.LexicalQuery{
			Scope: f.Other, Text: "distinctive phrase", Limit: 10,
		})
		if err != nil {
			t.Fatalf("search from the other workspace: %v", err)
		}
		if containsRecord(hits, mine.RecordID) {
			t.Fatal("a record was returned to a different workspace")
		}
	})

	t.Run(name+"/purge and count", func(t *testing.T) {
		ctx := t.Context()
		record := lexicalRecord(f.Other,
			"a record to be purged")

		if err := idx.Upsert(ctx, []domain.ProjectedRecord{record}); err != nil {
			t.Fatalf("upsert: %v", err)
		}
		if count, err := idx.Count(ctx, f.Other.WorkspaceID); err != nil || count == 0 {
			t.Fatalf("count after a write: %d, %v", count, err)
		}
		if err := idx.Purge(ctx, f.Other.WorkspaceID); err != nil {
			t.Fatalf("purge: %v", err)
		}
		count, err := idx.Count(ctx, f.Other.WorkspaceID)
		if err != nil {
			t.Fatalf("count after purge: %v", err)
		}
		if count != 0 {
			t.Fatalf("purge left %d records", count)
		}
	})

	t.Run(name+"/empty result is not an error", func(t *testing.T) {
		hits, err := idx.Search(t.Context(), domain.LexicalQuery{
			Scope: f.Primary, Text: "zqxjklmnprstv nonexistent phrase", Limit: 10,
		})
		if err != nil {
			t.Fatalf("a query matching nothing returned an error: %v", err)
		}
		if len(hits) != 0 {
			t.Fatalf("expected no hits, got %d", len(hits))
		}
	})

	t.Run(name+"/names itself", func(t *testing.T) {
		if idx.Name() == "" {
			t.Fatal("the backend does not identify itself")
		}
	})
}

// vectorRecord builds one projected vector for a conformance run.
func vectorRecord(tb testing.TB, f Fixture, scope domain.Scope, text string) domain.VectorRecord {
	tb.Helper()

	return domain.VectorRecord{
		ProjectedRecord: lexicalRecord(scope, text),
		Model:           f.Model,
		Version:         f.Version,
		Embedding:       f.Embed(tb, text),
		ContentHash:     domain.ContentHash([]byte(text)),
	}
}

// lexicalRecord builds one projected record. Record ids are UUIDs because the PostgreSQL
// implementation casts them; a conformance fixture that used arbitrary strings would fail
// against the reference backend for a reason unrelated to the contract.
func lexicalRecord(scope domain.Scope, text string) domain.ProjectedRecord {
	return domain.ProjectedRecord{
		Scope:          scope,
		Surface:        domain.SurfaceChunk,
		RecordID:       domain.NewUUIDString(),
		Content:        text,
		Status:         string(domain.AssertionActive),
		Classification: domain.ClassificationInternal,
		MemoryKind:     domain.MemorySemantic,
	}
}

func containsRecord(hits []domain.Hit, recordID string) bool {
	return countRecord(hits, recordID) > 0
}

func countRecord(hits []domain.Hit, recordID string) int {
	n := 0
	for _, hit := range hits {
		if hit.RecordID == recordID {
			n++
		}
	}
	return n
}
