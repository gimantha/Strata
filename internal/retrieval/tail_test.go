package retrieval

import (
	"slices"
	"testing"

	"github.com/gimantha/strata/internal/domain"
)

func TestWeakResultsAreDroppedFromAPlainSearch(t *testing.T) {
	items := fuse([]candidate{
		hit(domain.ModeLexical, 1, 1.0, domain.SurfaceChunk, "strong"),
		hit(domain.ModeLexical, 2, 0.1, domain.SurfaceChunk, "weak"),
	}, DefaultWeights(), 10)
	if _, ok := find(items, "weak"); ok {
		t.Fatalf("a plain search should drop results under the floor (ADR 0012): %+v", items)
	}
}

func TestKeptWeakResultsRankBelowEveryStrongOne(t *testing.T) {
	candidates := []candidate{
		hit(domain.ModeLexical, 1, 1.0, domain.SurfaceChunk, "strong-1"),
		hit(domain.ModeLexical, 2, 0.9, domain.SurfaceChunk, "strong-2"),
		hit(domain.ModeLexical, 3, 0.2, domain.SurfaceChunk, "weak-1"),
		hit(domain.ModeLexical, 4, 0.1, domain.SurfaceChunk, "weak-2"),
		// Found by a second retriever too, at full strength: it is strong.
		hit(domain.ModeVector, 1, 0.8, domain.SurfaceChunk, "strong-3"),
	}
	items := fuseWithTail(candidates, DefaultWeights(), 10, true)

	var order []string
	for _, item := range items {
		order = append(order, item.RecordID)
	}
	if len(items) != 5 {
		t.Fatalf("every result should be kept, got %v", order)
	}
	weakest := 1.0
	for _, item := range items {
		if item.Signals["weak_tail"] == 0 && item.Score < weakest {
			weakest = item.Score
		}
	}
	for i, item := range items {
		if item.Signals["weak_tail"] == 0 {
			continue
		}
		if item.Score >= weakest {
			t.Fatalf("weak result %s scores %v, not below the weakest strong one (%v)", item.RecordID, item.Score, weakest)
		}
		for _, later := range items[:i] {
			if later.Signals["weak_tail"] == 1 && later.Score < item.Score {
				t.Fatalf("weak results lost their own order: %v", order)
			}
		}
	}
	if order[len(order)-1] != "weak-2" || order[len(order)-2] != "weak-1" {
		t.Fatalf("weak results should come last, in their own order: %v", order)
	}

	// The limit still holds, and strong results fill it first.
	if items := fuseWithTail(candidates, DefaultWeights(), 3, true); len(items) != 3 || items[2].Signals["weak_tail"] != 0 {
		t.Fatalf("with room for three, the three strong results should be returned: %+v", items)
	}
}

func TestLooserHitsFollowStricterOnesWithoutRepeats(t *testing.T) {
	strict := []domain.Hit{{Surface: domain.SurfaceChunk, RecordID: "a"}}
	loose := []domain.Hit{
		{Surface: domain.SurfaceChunk, RecordID: "a"},
		{Surface: domain.SurfaceChunk, RecordID: "b"},
		{Surface: domain.SurfaceAssertion, RecordID: "a"},
		{Surface: domain.SurfaceChunk, RecordID: "c"},
	}
	got := appendNewHits(strict, loose, 3)
	var keys []string
	for _, h := range got {
		keys = append(keys, string(h.Surface)+"/"+h.RecordID)
	}
	want := []string{"chunk/a", "chunk/b", "assertion/a"}
	if !slices.Equal(keys, want) {
		t.Fatalf("got %v, want %v", keys, want)
	}
}

func TestANameOnItsOwnIsLookedUpAsAnEntityOnly(t *testing.T) {
	intent := Interpret("What did Caroline say about ticket CASE-42?")
	names := nameAnchors(intent)
	if !names["caroline"] {
		t.Fatalf("Caroline should be a name anchor, got %v", names)
	}
	if names["case-42"] {
		t.Fatal("an identifier is not a name anchor: searching for it alone finds the few records that carry it")
	}

	all := []domain.RetrievalMode{domain.ModeLexical, domain.ModeVector, domain.ModeEntity, domain.ModeExact}
	if got := nameModes(all); !slices.Equal(got, []domain.RetrievalMode{domain.ModeEntity}) {
		t.Fatalf("a name sub-query should run entity lookup only, got %v", got)
	}
	if got := nameModes([]domain.RetrievalMode{domain.ModeLexical}); got != nil {
		t.Fatalf("with no entity lookup allowed, a name sub-query runs nothing, got %v", got)
	}
}
