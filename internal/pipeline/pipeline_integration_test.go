package pipeline_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gimantha/strata/internal/domain"
	"github.com/gimantha/strata/internal/ingest"
	"github.com/gimantha/strata/internal/normalize"
	"github.com/gimantha/strata/internal/pipeline"
	"github.com/gimantha/strata/internal/store/blob"
	"github.com/gimantha/strata/internal/testsupport/pgtest"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

type harness struct {
	fixture *pgtest.Fixture
	gateway *ingest.Gateway
	runner  *pipeline.Runner
	blobs   *blob.FS
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	f := pgtest.NewFixture(t)
	blobs, err := blob.NewFS(t.TempDir())
	if err != nil {
		t.Fatalf("create blob store: %v", err)
	}

	stages := pipeline.DefaultStages(f.Store, blobs, pipeline.StageConfig{
		ChunkMaxTokens:     64,
		ChunkOverlapTokens: 8,
		Tokenizer:          normalize.DefaultTokenizer,
	})

	return &harness{
		fixture: f,
		gateway: ingest.New(f.Store, blobs, ingest.Options{PipelineVersion: 1}, nil, nil, nil),
		runner:  pipeline.NewRunner(f.Store, 1, stages, nil, nil, nil),
		blobs:   blobs,
	}
}

// ingestFile submits a repository fixture and returns its event id.
func (h *harness) ingestFile(t *testing.T, relPath, mediaType, key string) domain.SourceEventID {
	t.Helper()

	payload, err := os.ReadFile(filepath.Join("..", "..", "testdata", relPath))
	if err != nil {
		t.Fatalf("read fixture %s: %v", relPath, err)
	}
	receipt, err := h.gateway.Accept(context.Background(), ingest.Request{
		Scope:          h.fixture.Primary.Scope(),
		Principal:      h.fixture.Primary.Principal.Ref(),
		SourceID:       h.fixture.Primary.Source.ID,
		MediaType:      mediaType,
		Payload:        payload,
		IdempotencyKey: key,
	})
	if err != nil {
		t.Fatalf("ingest %s: %v", relPath, err)
	}
	return receipt.SourceEventID
}

func TestIntegrationPipelineProducesEpisodesAndChunks(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	ws := h.fixture.Primary.Workspace.ID

	eventID := h.ingestFile(t, "chat/session-01.json", normalize.MediaTypeJSON, "chat-1")

	result, err := h.runner.Process(ctx, ws, eventID, false)
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if result.StagesRun != 3 || result.StagesSkipped != 0 {
		t.Fatalf("expected all 3 stages to run, got %d run and %d skipped",
			result.StagesRun, result.StagesSkipped)
	}

	episodes, err := h.fixture.Store.ListEpisodes(ctx, ws, eventID)
	if err != nil {
		t.Fatalf("list episodes: %v", err)
	}
	if len(episodes) != 4 {
		t.Fatalf("expected one episode per conversation turn, got %d", len(episodes))
	}
	// Provenance and world time survive into the ledger.
	if episodes[0].Locator.Role != "user" || episodes[0].Locator.MessageIndex == nil {
		t.Fatalf("episode lost its conversational provenance: %+v", episodes[0].Locator)
	}
	if episodes[0].EventTime == nil {
		t.Fatal("the timestamp supplied by the source must reach the episode")
	}
	if episodes[0].Classification != domain.ClassificationInternal {
		t.Fatalf("classification must propagate from the source event, got %q", episodes[0].Classification)
	}

	chunks, err := h.fixture.Store.ListChunks(ctx, ws, eventID)
	if err != nil {
		t.Fatalf("list chunks: %v", err)
	}
	if len(chunks) == 0 {
		t.Fatal("expected chunks to be produced")
	}
	for _, chunk := range chunks {
		if domain.IsZero(chunk.EpisodeID) {
			t.Fatal("every chunk must point at its episode")
		}
		if chunk.TokenCount <= 0 {
			t.Fatal("every chunk needs a token estimate for later budgeting")
		}
	}

	status, err := h.fixture.Store.SourceEventStatus(ctx, ws, eventID)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if status.Event.Status != domain.SourceEventProcessed {
		t.Fatalf("expected the event to be marked processed, got %s", status.Event.Status)
	}
	if status.Run == nil || status.Run.Status != domain.RunSucceeded {
		t.Fatalf("expected a succeeded pipeline run, got %+v", status.Run)
	}
	if len(status.Stages) != 3 {
		t.Fatalf("expected 3 recorded stage runs, got %d", len(status.Stages))
	}
	for _, stage := range status.Stages {
		if stage.Status != domain.RunSucceeded {
			t.Fatalf("stage %s did not succeed: %s", stage.StageName, stage.LastError)
		}
		if len(stage.OutputRef) == 0 || string(stage.OutputRef) == "{}" {
			t.Fatalf("stage %s recorded no output summary", stage.StageName)
		}
	}
}

func TestIntegrationReplaySkipsCompletedStages(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	ws := h.fixture.Primary.Workspace.ID

	eventID := h.ingestFile(t, "doc/handbook.md", normalize.MediaTypeMarkdown, "doc-1")

	if _, err := h.runner.Process(ctx, ws, eventID, false); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	episodesBefore, _ := h.fixture.Store.ListEpisodes(ctx, ws, eventID)
	chunksBefore, _ := h.fixture.Store.ListChunks(ctx, ws, eventID)

	// Redelivery of the same work item must cost almost nothing and create nothing.
	result, err := h.runner.Process(ctx, ws, eventID, false)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if result.StagesRun != 0 || result.StagesSkipped != 3 {
		t.Fatalf("a replay must skip completed stages, got %d run and %d skipped",
			result.StagesRun, result.StagesSkipped)
	}

	episodesAfter, _ := h.fixture.Store.ListEpisodes(ctx, ws, eventID)
	chunksAfter, _ := h.fixture.Store.ListChunks(ctx, ws, eventID)
	if len(episodesAfter) != len(episodesBefore) || len(chunksAfter) != len(chunksBefore) {
		t.Fatalf("replay changed derived state: episodes %d->%d, chunks %d->%d",
			len(episodesBefore), len(episodesAfter), len(chunksBefore), len(chunksAfter))
	}
}

func TestIntegrationForcedReprocessDoesNotDuplicateKnowledge(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	ws := h.fixture.Primary.Workspace.ID

	eventID := h.ingestFile(t, "json/customers.json", normalize.MediaTypeJSON, "json-1")
	if _, err := h.runner.Process(ctx, ws, eventID, false); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	episodesBefore, _ := h.fixture.Store.ListEpisodes(ctx, ws, eventID)
	if len(episodesBefore) != 3 {
		t.Fatalf("expected one episode per record, got %d", len(episodesBefore))
	}

	result, err := h.runner.Process(ctx, ws, eventID, true)
	if err != nil {
		t.Fatalf("forced reprocess: %v", err)
	}
	if result.StagesRun != 3 {
		t.Fatalf("a forced run must re-execute every stage, got %d", result.StagesRun)
	}

	episodesAfter, _ := h.fixture.Store.ListEpisodes(ctx, ws, eventID)
	if len(episodesAfter) != len(episodesBefore) {
		t.Fatalf("forced reprocessing duplicated episodes: %d -> %d",
			len(episodesBefore), len(episodesAfter))
	}
	// Identifiers must be preserved too, so anything referencing an episode stays valid.
	for i := range episodesBefore {
		if episodesBefore[i].ID != episodesAfter[i].ID {
			t.Fatal("reprocessing must reuse existing episode identities, not mint new ones")
		}
	}
}

func TestIntegrationDerivedUnitsRegenerateIdenticallyFromTheLedger(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	ws := h.fixture.Primary.Workspace.ID

	eventID := h.ingestFile(t, "doc/handbook.md", normalize.MediaTypeMarkdown, "doc-1")
	if _, err := h.runner.Process(ctx, ws, eventID, false); err != nil {
		t.Fatalf("first pass: %v", err)
	}

	before := describeUnits(t, h, ws, eventID)

	// Episodes and chunks are derived state. Dropping them and reprocessing must
	// reproduce the same content and provenance from the archived source alone: this is
	// the projection-rebuild property the whole architecture rests on
	// (AGENTS.md sections 2.3, 15.2).
	if err := h.fixture.Store.DeleteDerivedUnits(ctx, ws, eventID); err != nil {
		t.Fatalf("delete derived units: %v", err)
	}
	if episodes, _ := h.fixture.Store.ListEpisodes(ctx, ws, eventID); len(episodes) != 0 {
		t.Fatal("derived units were not deleted")
	}

	if _, err := h.runner.Process(ctx, ws, eventID, true); err != nil {
		t.Fatalf("rebuild: %v", err)
	}

	after := describeUnits(t, h, ws, eventID)
	if before != after {
		t.Fatalf("rebuilt derived state differs from the original:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func TestIntegrationMissingArchivedBytesFailsVisibly(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	ws := h.fixture.Primary.Workspace.ID

	eventID := h.ingestFile(t, "chat/session-01.json", normalize.MediaTypeJSON, "chat-1")

	// Delete the archived payload behind the event's back.
	event, err := h.fixture.Store.GetSourceEvent(ctx, ws, eventID)
	if err != nil {
		t.Fatalf("load event: %v", err)
	}
	artifact, err := h.fixture.Store.GetArtifact(ctx, ws, event.RawArtifactID)
	if err != nil {
		t.Fatalf("load artifact: %v", err)
	}
	if err := h.blobs.Delete(ctx, artifact.BlobKey); err != nil {
		t.Fatalf("delete blob: %v", err)
	}

	// Deriving knowledge without retrievable evidence is exactly what must not happen,
	// so this fails loudly and is recorded rather than silently producing nothing.
	if _, err := h.runner.Process(ctx, ws, eventID, true); err == nil {
		t.Fatal("processing must fail when the archived payload is gone")
	}

	status, err := h.fixture.Store.SourceEventStatus(ctx, ws, eventID)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if status.Run == nil || status.Run.Status != domain.RunFailed {
		t.Fatalf("expected a failed pipeline run, got %+v", status.Run)
	}
	var normalizeStage *domain.StageRun
	for i := range status.Stages {
		if status.Stages[i].StageName == "normalize" {
			normalizeStage = &status.Stages[i]
		}
	}
	if normalizeStage == nil {
		t.Fatal("the failing stage must be recorded")
	}
	if normalizeStage.LastError == "" || normalizeStage.ErrorClass == "" {
		t.Fatalf("a failed stage must record its cause and class: %+v", normalizeStage)
	}
	if episodes, _ := h.fixture.Store.ListEpisodes(ctx, ws, eventID); len(episodes) != 0 {
		t.Fatal("no knowledge may be derived from unreadable source material")
	}
}

func TestIntegrationMalformedPayloadIsTerminalNotRetriedForever(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	ws := h.fixture.Primary.Workspace.ID

	// Declared as JSON but not JSON: this can never succeed on retry.
	receipt, err := h.gateway.Accept(ctx, ingest.Request{
		Scope:          h.fixture.Primary.Scope(),
		Principal:      h.fixture.Primary.Principal.Ref(),
		SourceID:       h.fixture.Primary.Source.ID,
		MediaType:      normalize.MediaTypeJSON,
		Payload:        []byte(`{"messages": [{"role": "user"`),
		IdempotencyKey: "broken-1",
	})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}

	_, err = h.runner.Process(ctx, ws, receipt.SourceEventID, false)
	if err == nil {
		t.Fatal("malformed JSON must fail the pipeline")
	}
	if domain.ClassifyError(err).Retryable() {
		t.Fatalf("malformed input must not be retryable, classified as %s", domain.ClassifyError(err))
	}

	status, err := h.fixture.Store.SourceEventStatus(ctx, ws, receipt.SourceEventID)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if status.Event.Status != domain.SourceEventFailed {
		t.Fatalf("expected the event to be marked failed, got %s", status.Event.Status)
	}
	for _, stage := range status.Stages {
		if stage.StageName == "normalize" && stage.Status != domain.RunDead {
			t.Fatalf("a permanently invalid payload must leave its stage dead, got %s", stage.Status)
		}
	}
	// The source event and its archived bytes remain: the raw record is preserved even
	// when interpretation fails.
	if status.Event.ContentHash == "" {
		t.Fatal("the source event must survive a processing failure")
	}
}

func TestIntegrationWorkspaceIsolationOnProcessing(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	eventID := h.ingestFile(t, "chat/session-01.json", normalize.MediaTypeJSON, "chat-1")
	other := h.fixture.NewTenant(t, "globex")

	// An event id from another tenant must not be processable.
	if _, err := h.runner.Process(ctx, other.Workspace.ID, eventID, false); err == nil {
		t.Fatal("processing must be scoped to the owning workspace")
	} else if !domain.IsCode(err, domain.CodeNotFound) {
		t.Fatalf("expected not_found, got %s", domain.CodeOf(err))
	}
}

// describeUnits renders derived state in a comparable form: content and provenance,
// excluding identifiers and timestamps that legitimately differ between runs.
func describeUnits(t *testing.T, h *harness, ws domain.WorkspaceID, eventID domain.SourceEventID) string {
	t.Helper()
	ctx := context.Background()

	episodes, err := h.fixture.Store.ListEpisodes(ctx, ws, eventID)
	if err != nil {
		t.Fatalf("list episodes: %v", err)
	}
	chunks, err := h.fixture.Store.ListChunks(ctx, ws, eventID)
	if err != nil {
		t.Fatalf("list chunks: %v", err)
	}

	var sb strings.Builder
	for _, e := range episodes {
		sb.WriteString("episode\t")
		sb.WriteString(e.ContentType)
		sb.WriteString("\t")
		sb.WriteString(e.Locator.Section)
		sb.WriteString("\t")
		sb.WriteString(strings.Join(e.Locator.HeadingPath, ">"))
		sb.WriteString("\t")
		sb.WriteString(e.Content)
		sb.WriteString("\n")
	}
	for _, c := range chunks {
		sb.WriteString("chunk\t")
		sb.WriteString(c.Content)
		sb.WriteString("\n")
	}
	return sb.String()
}
