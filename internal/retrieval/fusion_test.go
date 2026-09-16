package retrieval

import (
	"testing"

	"github.com/gimantha/strata/internal/domain"
)

// hit builds a candidate for one retriever.
func hit(mode domain.RetrievalMode, rank int, score float64, surface domain.Surface, id string) candidate {
	return candidate{
		mode: mode, rank: rank, score: score,
		hit: domain.Hit{Surface: surface, RecordID: id, Content: id},
	}
}

func find(items []domain.RetrievedItem, id string) (domain.RetrievedItem, bool) {
	for _, item := range items {
		if item.RecordID == id {
			return item, true
		}
	}
	return domain.RetrievedItem{}, false
}

func TestFuseRewardsAgreementBetweenRetrievers(t *testing.T) {
	// "agreed" is ranked second by two retrievers; "solo" is ranked first by one. Fusing
	// should prefer the record both retrievers liked - that is the entire reason to fuse
	// rather than concatenate.
	items := fuse([]candidate{
		hit(domain.ModeLexical, 1, 1.0, domain.SurfaceChunk, "solo"),
		hit(domain.ModeLexical, 2, 0.9, domain.SurfaceChunk, "agreed"),
		hit(domain.ModeVector, 1, 1.0, domain.SurfaceChunk, "agreed"),
	}, DefaultWeights(), 10)

	if len(items) != 2 {
		t.Fatalf("expected two results, got %d", len(items))
	}
	if items[0].RecordID != "agreed" {
		t.Fatalf("the record both retrievers found should rank first, got %q", items[0].RecordID)
	}
	if len(items[0].FoundBy) != 2 {
		t.Fatalf("the fused result must record both retrievers, got %v", items[0].FoundBy)
	}
	if items[0].Signals["lexical_rrf"] == 0 || items[0].Signals["vector_rrf"] == 0 {
		t.Fatalf("each retriever's contribution must be inspectable, got %v", items[0].Signals)
	}
}

func TestFuseAggregatesRepeatedFindsFromOneRetriever(t *testing.T) {
	first := hit(domain.ModeLexical, 2, 0.7, domain.SurfaceChunk, "same")
	first.stream = 1
	second := hit(domain.ModeLexical, 1, 0.5, domain.SurfaceChunk, "same")
	second.stream = 2

	items := fuse([]candidate{first, second}, DefaultWeights(), 10)
	if len(items) != 1 || len(items[0].FoundBy) != 1 {
		t.Fatalf("FoundBy must name retrievers once, got %+v", items)
	}
	if items[0].Ranks[domain.ModeLexical] != 1 {
		t.Fatalf("expected best rank across query streams, got %d", items[0].Ranks[domain.ModeLexical])
	}
	if items[0].Signals["lexical_rrf"] != items[0].Score {
		t.Fatalf("reported RRF contribution %v does not equal score %v",
			items[0].Signals["lexical_rrf"], items[0].Score)
	}
}

func TestFuseWeightsModesDifferently(t *testing.T) {
	// An exact identifier match outranks a lexical match at the same position, because an
	// identifier query has one right answer and prose does not.
	items := fuse([]candidate{
		hit(domain.ModeLexical, 1, 1.0, domain.SurfaceChunk, "prose"),
		hit(domain.ModeExact, 1, 1.0, domain.SurfaceChunk, "identifier"),
	}, DefaultWeights(), 10)

	if items[0].RecordID != "identifier" {
		t.Fatalf("exact matching should outrank lexical at equal rank, got %q", items[0].RecordID)
	}
}

func TestFuseFillsDefaultWeights(t *testing.T) {
	// A zero Weights means "use the defaults". Before this was handled, every weight was
	// zero, every score collapsed to zero, and ranking silently fell through to the
	// tie-breakers - which looks like working retrieval right up until it is measured.
	items := fuse([]candidate{
		hit(domain.ModeLexical, 1, 1.0, domain.SurfaceChunk, "a"),
	}, Weights{}, 10)

	if len(items) != 1 {
		t.Fatalf("expected one result, got %d", len(items))
	}
	if items[0].Score <= 0 {
		t.Fatalf("a zero Weights must fall back to defaults, got score %v", items[0].Score)
	}
}

func TestFuseDropsEachRetrieversWeakTail(t *testing.T) {
	// A retriever that found nothing good should not cast a full-strength vote for its
	// least-bad result.
	weights := DefaultWeights()
	items := fuse([]candidate{
		hit(domain.ModeVector, 1, 1.0, domain.SurfaceChunk, "strong"),
		hit(domain.ModeVector, 2, 0.05, domain.SurfaceChunk, "weak"),
	}, weights, 10)

	if _, ok := find(items, "weak"); ok {
		t.Fatal("a result far below its retriever's best should be dropped before fusion")
	}
	if _, ok := find(items, "strong"); !ok {
		t.Fatal("the strong result must survive")
	}

	// Comparison is within a retriever, never across: a retriever whose scores are all
	// small is not thereby worthless.
	items = fuse([]candidate{
		hit(domain.ModeVector, 1, 0.04, domain.SurfaceChunk, "small-but-best"),
		hit(domain.ModeLexical, 1, 90.0, domain.SurfaceChunk, "large-scale"),
	}, weights, 10)
	if _, ok := find(items, "small-but-best"); !ok {
		t.Fatal("scores must be compared within a retriever, not across retrievers")
	}

	// Rewritten queries from the same retriever are separate result streams too. Text
	// rank for a one-word requirement is not calibrated against text rank for the full
	// question, so the best hit from either search must survive its own weak-tail check.
	original := hit(domain.ModeLexical, 1, 1.0, domain.SurfaceChunk, "original-query")
	original.stream = 1
	decomposed := hit(domain.ModeLexical, 1, 0.05, domain.SurfaceChunk, "decomposed-query")
	decomposed.stream = 2
	items = fuse([]candidate{original, decomposed}, weights, 10)
	if _, ok := find(items, "decomposed-query"); !ok {
		t.Fatal("the best result from an independent rewritten query was dropped")
	}
}

func TestFuseScalesByScoreQuality(t *testing.T) {
	// Two retrievers each rank their own record first, but one is far less confident about
	// it relative to what else it found.
	weights := DefaultWeights()
	items := fuse([]candidate{
		hit(domain.ModeLexical, 1, 1.0, domain.SurfaceChunk, "confident"),
		hit(domain.ModeLexical, 2, 0.95, domain.SurfaceChunk, "runner-up"),
		hit(domain.ModeVector, 1, 1.0, domain.SurfaceChunk, "other-confident"),
	}, weights, 10)

	confident, _ := find(items, "confident")
	runnerUp, _ := find(items, "runner-up")
	if confident.Score <= runnerUp.Score {
		t.Fatal("a retriever's best result should outrank its own runner-up")
	}

	// Turning the quality weight off makes fusion rank-only, which is plain RRF.
	weights.ScoreQualityWeight = 0
	plain := fuse([]candidate{
		hit(domain.ModeLexical, 1, 1.0, domain.SurfaceChunk, "a"),
		hit(domain.ModeVector, 1, 0.2, domain.SurfaceChunk, "b"),
	}, weights, 10)
	first, _ := find(plain, "a")
	second, _ := find(plain, "b")
	if first.Score != second.Score {
		t.Fatalf("without score weighting, equal ranks and weights should tie: %v vs %v",
			first.Score, second.Score)
	}
}

func TestFuseDiscountsDeeperGraphResults(t *testing.T) {
	near := hit(domain.ModeGraph, 1, 1.0, domain.SurfaceEntity, "one-hop")
	near.path = &domain.GraphPath{Depth: 1}
	far := hit(domain.ModeGraph, 2, 1.0, domain.SurfaceEntity, "four-hops")
	far.path = &domain.GraphPath{Depth: 4}

	items := fuse([]candidate{near, far}, DefaultWeights(), 10)
	if items[0].RecordID != "one-hop" {
		t.Fatal("a closer graph result should outrank a more distant one")
	}
	if items[0].Path == nil || items[0].Path.Depth != 1 {
		t.Fatal("a graph result must carry the path that reached it")
	}
}

func TestFuseReservesRoomForOtherSurfaces(t *testing.T) {
	// Passages outnumber identities, so pure score ordering fills the result set with
	// chunks and the entity the question was about never appears.
	candidates := []candidate{
		hit(domain.ModeLexical, 1, 1.0, domain.SurfaceChunk, "chunk-1"),
		hit(domain.ModeLexical, 2, 0.9, domain.SurfaceChunk, "chunk-2"),
		hit(domain.ModeLexical, 3, 0.8, domain.SurfaceChunk, "chunk-3"),
		hit(domain.ModeVector, 1, 1.0, domain.SurfaceChunk, "chunk-1"),
		hit(domain.ModeVector, 2, 0.9, domain.SurfaceChunk, "chunk-2"),
		hit(domain.ModeGraph, 9, 0.1, domain.SurfaceEntity, "distant-entity"),
	}
	graphResult := &candidates[5]
	graphResult.path = &domain.GraphPath{Depth: 3}

	items := fuse(candidates, DefaultWeights(), 3)
	if len(items) != 3 {
		t.Fatalf("expected three results, got %d", len(items))
	}

	var surfaces int
	for _, item := range items {
		if item.Surface == domain.SurfaceEntity {
			surfaces++
		}
	}
	if surfaces == 0 {
		t.Fatal("a surface that produced candidates should not be shut out of the results")
	}

	// Diversity must not overwhelm relevance: the top result is still the best-scoring one.
	if items[0].Surface != domain.SurfaceChunk {
		t.Fatal("the highest-scoring result should still rank first")
	}
}

func TestFuseIsStableAndOrdered(t *testing.T) {
	candidates := []candidate{
		hit(domain.ModeLexical, 1, 1.0, domain.SurfaceChunk, "b"),
		hit(domain.ModeLexical, 1, 1.0, domain.SurfaceChunk, "a"),
	}

	// Identical scores must break ties deterministically, or results reorder between runs
	// and every regression becomes invisible.
	var previous string
	for range 5 {
		items := fuse(candidates, DefaultWeights(), 10)
		order := items[0].RecordID + items[1].RecordID
		if previous != "" && order != previous {
			t.Fatalf("fusion is not stable: %q then %q", previous, order)
		}
		previous = order
	}
}

func TestFuseRespectsLimit(t *testing.T) {
	var candidates []candidate
	for i := range 20 {
		candidates = append(candidates,
			hit(domain.ModeLexical, i+1, 1.0-float64(i)*0.01, domain.SurfaceChunk,
				string(rune('a'+i))))
	}
	if items := fuse(candidates, DefaultWeights(), 5); len(items) != 5 {
		t.Fatalf("expected the limit to be honoured, got %d", len(items))
	}
	if items := fuse(nil, DefaultWeights(), 5); len(items) != 0 {
		t.Fatalf("no candidates should yield no results, got %d", len(items))
	}
}

func TestPlannerRecognizesIdentifiers(t *testing.T) {
	identifiers := []string{"ERR_7731X", "AF-2291-B", "sku 12345", "v2.1.4", "user-42"}
	for _, query := range identifiers {
		if !looksLikeIdentifier(query) {
			t.Fatalf("%q should be recognized as an identifier", query)
		}
	}

	prose := []string{
		"what is the refund policy",
		"who supplies industrial fasteners",
		"Acme Corporation",
		"tell me about the Berlin office and its history in some detail",
	}
	for _, query := range prose {
		if looksLikeIdentifier(query) {
			t.Fatalf("%q should not be treated as an identifier", query)
		}
	}
}

func TestPlannerSkipsVectorWithoutAnEmbedder(t *testing.T) {
	plan := heuristicPlanner{hasEmbedder: false}.Plan(t.Context(), domain.QueryRequest{Query: "anything at all"})
	for _, mode := range plan.Modes {
		if mode == domain.ModeVector {
			t.Fatal("vector retrieval must not be planned without an embedder")
		}
	}
	if plan.Skipped[domain.ModeVector] == "" {
		t.Fatal("the plan must say why vector retrieval was skipped")
	}

	// An explicitly requested mode list is honoured rather than second-guessed, except for
	// a mode that cannot run.
	plan = heuristicPlanner{hasEmbedder: false}.Plan(t.Context(), domain.QueryRequest{
		Query: "anything", Modes: []domain.RetrievalMode{domain.ModeLexical, domain.ModeVector},
	})
	if len(plan.Modes) != 1 || plan.Modes[0] != domain.ModeLexical {
		t.Fatalf("expected only the runnable requested mode, got %v", plan.Modes)
	}
}

func TestExactStreamIsRankOnly(t *testing.T) {
	// Exact mode's score is a trigram similarity that mostly measures record length. A
	// passage that contains the identifier once scores far below a two-word label that is
	// the identifier, and the relative floor used to drop it before fusion.
	items := fuse([]candidate{
		hit(domain.ModeExact, 1, 0.9, domain.SurfaceEntity, "label"),
		hit(domain.ModeExact, 2, 0.05, domain.SurfaceChunk, "passage"),
	}, DefaultWeights(), 10)
	if _, ok := find(items, "passage"); !ok {
		t.Fatal("an exact match was dropped for having a low similarity score")
	}
	// The same shape in a retriever whose scores are magnitudes is still trimmed.
	items = fuse([]candidate{
		hit(domain.ModeLexical, 1, 0.9, domain.SurfaceChunk, "strong"),
		hit(domain.ModeLexical, 2, 0.05, domain.SurfaceChunk, "weak"),
	}, DefaultWeights(), 10)
	if _, ok := find(items, "weak"); ok {
		t.Fatal("the relative floor must still apply to scored retrievers")
	}
}

func TestPrioritizeAnchoredPromotesRecordsNamingTheIdentifier(t *testing.T) {
	items := []domain.RetrievedItem{
		{RecordID: "lookalike", Content: "replacement decision for another case", Score: 0.9},
		{RecordID: "about-it", Content: "replacement decision for CASE-42", Score: 0.5},
		{RecordID: "cited", Content: "audit report", Score: 0.1},
		{RecordID: "noise", Content: "", Score: 0.05},
	}
	got := prioritizeAnchored(items, []string{"CASE-42"}, map[string]bool{"cited": true}, 3)
	if len(got) != 3 || got[0].RecordID != "about-it" || got[1].RecordID != "cited" || got[2].RecordID != "lookalike" {
		t.Fatalf("anchored and cited records must come first, in fused order: %+v", got)
	}
	if got[0].Signals["anchored"] != 1 || got[1].Signals["anchored"] != 1 {
		t.Fatal("promotion must be visible in the item signals")
	}
	unanchored := prioritizeAnchored(items, nil, nil, 2)
	if len(unanchored) != 2 || unanchored[0].RecordID != "lookalike" {
		t.Fatalf("without anchors the fused order stands: %+v", unanchored)
	}
	absent := prioritizeAnchored(items, []string{"CASE-99"}, nil, 2)
	if absent[0].RecordID != "lookalike" {
		t.Fatalf("an anchor nothing mentions must not reorder anything: %+v", absent)
	}
}

func TestAnchorsInSkipsAbsentTermsAndTheAnchorItself(t *testing.T) {
	got := anchorsIn([]string{"CASE-42", "ERR_9"}, "why did CASE-42 fail?")
	if len(got) != 1 || got[0] != "CASE-42" {
		t.Fatalf("anchors = %v", got)
	}
	if anchorsIn([]string{"CASE-42"}, "CASE-42") != nil {
		t.Fatal("a sub-query that is the identifier has nothing to anchor")
	}
}

func TestReachedRecordsAreDiscountedWhateverRetrieverReachedThem(t *testing.T) {
	direct := hit(domain.ModeExact, 1, 1, domain.SurfaceChunk, "direct")
	cited := hit(domain.ModeExact, 1, 1, domain.SurfaceChunk, "cited")
	cited.stream = 2
	cited.path = &domain.GraphPath{ViaPredicate: "cites", Depth: 1}
	items := fuse([]candidate{direct, cited}, DefaultWeights(), 10)
	d, _ := find(items, "direct")
	c, _ := find(items, "cited")
	if !(c.Score < d.Score) {
		t.Fatalf("a record reached through a citation must not outrank one matched directly: %v vs %v", c.Score, d.Score)
	}
	if c.Path == nil || c.Path.ViaPredicate != "cites" {
		t.Fatal("the path a record was reached by must be reported")
	}
}
