package ledger_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/gimantha/strata/internal/domain"
	"github.com/gimantha/strata/internal/testsupport/pgtest"
)

func chatRecord(scope domain.Scope, text string) domain.ProjectedRecord {
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

// A word that is on half the records says little about which one answers a question. In a
// chat memory the speaker's name is such a word. Ranking that weighs every word the same
// put the many records that only shared the name ahead of the one record that shared the
// rare word. BM25 weighs a word by how rare it is.
func TestIntegrationRelaxedSearchRanksRareWordsAboveCommonOnes(t *testing.T) {
	f := pgtest.NewFixture(t)
	ctx := context.Background()
	scope := f.Primary.Scope()

	var records []domain.ProjectedRecord
	for i := range 30 {
		records = append(records, chatRecord(scope, fmt.Sprintf("Caroline: thanks, see you on day %d!", i)))
	}
	answer := chatRecord(scope, "Melanie: the adoption agency called back about the interview.")
	records = append(records, answer)
	if err := f.Store.UpsertLexical(ctx, records); err != nil {
		t.Fatal(err)
	}

	hits, err := f.Store.SearchLexical(ctx, domain.LexicalQuery{
		Scope: scope, Text: "What did Caroline hear about the adoption?", MatchAny: true, Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 || hits[0].RecordID != answer.RecordID {
		t.Fatalf("the record with the rare word should rank first, got %+v", hits)
	}
	if hits[0].Score <= hits[len(hits)-1].Score {
		t.Fatalf("scores should fall down the list: first %v, last %v", hits[0].Score, hits[len(hits)-1].Score)
	}
}

// Strict search still decides which records match. BM25 only orders them.
func TestIntegrationStrictSearchStillRequiresEveryWord(t *testing.T) {
	f := pgtest.NewFixture(t)
	ctx := context.Background()
	scope := f.Primary.Scope()

	both := chatRecord(scope, "Melanie: the adoption interview went well.")
	one := chatRecord(scope, "Melanie: the adoption papers arrived.")
	if err := f.Store.UpsertLexical(ctx, []domain.ProjectedRecord{both, one}); err != nil {
		t.Fatal(err)
	}
	hits, err := f.Store.SearchLexical(ctx, domain.LexicalQuery{
		Scope: scope, Text: "adoption interview", Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].RecordID != both.RecordID {
		t.Fatalf("strict search must return only the record with every word, got %+v", hits)
	}
}
