package contextblock_test

import (
	"context"
	"encoding/json"
	"sort"
	"testing"

	"github.com/gimantha/strata/internal/domain"
	"github.com/gimantha/strata/internal/ingest"
	"github.com/gimantha/strata/internal/normalize"
)

// ingestChat puts one conversation through the whole pipeline and returns its chunks in
// message order.
func (h *harness) ingestChat(t *testing.T, key string, messages ...string) []domain.Chunk {
	t.Helper()
	ctx := context.Background()

	type message struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	payload := struct {
		Messages []message `json:"messages"`
	}{}
	for i, content := range messages {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		payload.Messages = append(payload.Messages, message{Role: role, Content: content})
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}

	receipt, err := h.gateway.Accept(ctx, ingest.Request{
		Scope:          h.scope(),
		Principal:      h.fixture.Primary.Principal.Ref(),
		SourceID:       h.fixture.Primary.Source.ID,
		MediaType:      normalize.MediaTypeJSON,
		Payload:        body,
		IdempotencyKey: key,
	})
	if err != nil {
		t.Fatalf("ingest %s: %v", key, err)
	}
	if _, err := h.runner.Process(ctx, h.scope().WorkspaceID, receipt.SourceEventID, false); err != nil {
		t.Fatalf("process %s: %v", key, err)
	}
	chunks, err := h.fixture.Store.ListChunks(ctx, h.scope().WorkspaceID, receipt.SourceEventID)
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != len(messages) {
		t.Fatalf("expected one chunk per message, got %d for %d messages", len(chunks), len(messages))
	}
	for _, c := range chunks {
		if c.Locator.MessageIndex == nil {
			t.Fatalf("a chat chunk should carry its message index: %+v", c.Locator)
		}
	}
	sort.Slice(chunks, func(i, j int) bool {
		return *chunks[i].Locator.MessageIndex < *chunks[j].Locator.MessageIndex
	})
	return chunks
}

func TestIntegrationChunkNeighborsFollowReadingOrderAndClassification(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	ws := h.scope().WorkspaceID

	chunks := h.ingestChat(t, "chat-1",
		"Did you ever finish that painting of the lake?",
		"Yes, last year! It hangs in my kitchen now.",
		"Lovely. How are the kids?",
	)
	first, middle, last := chunks[0].ID, chunks[1].ID, chunks[2].ID

	all := []domain.Classification{domain.ClassificationPublic, domain.ClassificationInternal}
	got, err := h.fixture.Store.ChunkNeighbors(ctx, ws, []domain.ChunkID{first, middle, last}, all)
	if err != nil {
		t.Fatal(err)
	}
	if got[middle].Previous != first || got[middle].Next != last {
		t.Fatalf("the middle message should sit between the other two, got %+v", got[middle])
	}
	if got[first].Previous != "" || got[last].Next != "" {
		t.Fatalf("the ends of a conversation have no neighbour beyond them: %+v %+v", got[first], got[last])
	}

	// A neighbour the caller may not see is left out by the query itself, so its text is
	// never read into the assembler.
	if _, err := h.fixture.Store.Pool().Exec(ctx,
		`UPDATE chunks SET classification = 'restricted' WHERE workspace_id = $1 AND id = $2`, ws, last); err != nil {
		t.Fatal(err)
	}
	got, err = h.fixture.Store.ChunkNeighbors(ctx, ws, []domain.ChunkID{middle}, all)
	if err != nil {
		t.Fatal(err)
	}
	if got[middle].Previous != first || got[middle].Next != "" {
		t.Fatalf("a restricted neighbour must not be returned, got %+v", got[middle])
	}
}
