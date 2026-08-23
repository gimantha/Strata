package pipeline

import (
	"context"
	"time"

	"github.com/gimantha/strata/internal/domain"
	"github.com/gimantha/strata/internal/extraction"
	"github.com/gimantha/strata/internal/normalize"
)

// LedgerStore is the canonical persistence the built-in stages need.
type LedgerStore interface {
	GetArtifact(ctx context.Context, ws domain.WorkspaceID, id domain.ArtifactID) (domain.Artifact, error)
	InsertEpisodes(ctx context.Context, episodes []domain.Episode) ([]domain.Episode, error)
	ListEpisodes(ctx context.Context, ws domain.WorkspaceID, eventID domain.SourceEventID) ([]domain.Episode, error)
	InsertChunks(ctx context.Context, chunks []domain.Chunk) ([]domain.Chunk, error)
	ListChunks(ctx context.Context, ws domain.WorkspaceID, eventID domain.SourceEventID) ([]domain.Chunk, error)
	GraphSpaceBinding(ctx context.Context, ws domain.WorkspaceID, id domain.GraphSpaceID) (domain.OntologyBinding, error)
}

// BlobReader reads archived source bytes.
type BlobReader interface {
	Get(ctx context.Context, key string) ([]byte, error)
}

// StageConfig configures the built-in stages.
type StageConfig struct {
	ChunkMaxTokens     int
	ChunkOverlapTokens int
	Tokenizer          normalize.Tokenizer
	// Now supplies knowledge time, injectable so tests are deterministic.
	Now func() time.Time

	// Extractor and Committer enable the extraction stage. Both must be present for it
	// to run: a deployment with no model provider configured processes source material
	// into episodes and chunks and stops there, rather than failing.
	Extractor *extraction.Extractor
	Committer Committer

	// Projector enables the projection stage. Without it the ledger is still complete;
	// only retrieval is unavailable, and a later rebuild fills the projections in.
	Projector Projector
}

func (c StageConfig) now() time.Time {
	if c.Now == nil {
		return time.Now().UTC()
	}
	return c.Now().UTC()
}

func (c StageConfig) chunkOptions() normalize.ChunkOptions {
	return normalize.ChunkOptions{
		MaxTokens:     c.ChunkMaxTokens,
		OverlapTokens: c.ChunkOverlapTokens,
		Tokenizer:     c.Tokenizer,
	}
}

// DefaultStages is the pipeline: decode, split into episodes, chunk, and - when a model
// provider is configured - extract candidate knowledge.
//
// Deterministic work always precedes the model (AGENTS.md section 10.5). Structure the
// source already carries is read directly, and the model is asked only for what cannot be
// read that way.
//
// Extraction is added only when both an extractor and a committer are supplied, so a
// deployment with no model provider still ingests, segments, and chunks rather than
// failing.
//
// Entity resolution, ontology validation, temporal reconciliation, and projection stages
// join this list in later phases. They are absent rather than stubbed, because an empty
// stage that records success would make replay state lie.
func DefaultStages(store LedgerStore, blobs BlobReader, cfg StageConfig) []Stage {
	stages := []Stage{
		NormalizeStage{store: store, blobs: blobs, cfg: cfg},
		SegmentStage{store: store, blobs: blobs, cfg: cfg},
		ChunkStage{store: store, cfg: cfg},
	}
	if cfg.Extractor != nil && cfg.Committer != nil {
		stages = append(stages, NewExtractStage(store, cfg.Extractor, cfg.Committer, cfg))
	}
	if cfg.Projector != nil {
		stages = append(stages, NewProjectStage(cfg.Projector, cfg))
	}
	return stages
}

// loadDocument reads the archived artifact and decodes it.
//
// Every stage re-reads from the ledger and blob store rather than receiving the
// previous stage's in-memory output. That is what lets a single stage be re-run in
// isolation years later, and it costs one cheap read for text payloads.
func loadDocument(ctx context.Context, store LedgerStore, blobs BlobReader, in Input) (normalize.Document, error) {
	const op = "pipeline.loadDocument"

	artifact, err := store.GetArtifact(ctx, in.Event.WorkspaceID, in.Event.RawArtifactID)
	if err != nil {
		return normalize.Document{}, err
	}
	raw, err := blobs.Get(ctx, artifact.BlobKey)
	if err != nil {
		// Missing archived bytes is a hard failure: knowledge without retrievable
		// evidence is exactly what this architecture refuses to produce.
		return normalize.Document{}, domain.Wrap(err, domain.CodeInternal, op,
			"archived artifact is unreadable")
	}

	mediaType := in.Event.MediaType
	if mediaType == "" {
		mediaType = artifact.MediaType
	}

	direct, _ := in.Event.Metadata[normalize.MetadataDirectEpisode].(bool)
	doc, err := normalize.Decode(mediaType, raw, normalize.Options{DirectEpisode: direct})
	if err != nil {
		return normalize.Document{}, err
	}
	return doc, nil
}

// NormalizeStage decodes the payload and records what was found.
//
// It writes no knowledge. Its job is to fail fast and visibly on material that cannot
// be interpreted, before anything downstream derives from it.
type NormalizeStage struct {
	store LedgerStore
	blobs BlobReader
	cfg   StageConfig
}

func (NormalizeStage) Name() string { return "normalize" }
func (NormalizeStage) Version() int { return 1 }

func (s NormalizeStage) Execute(ctx context.Context, in Input) (Output, error) {
	doc, err := loadDocument(ctx, s.store, s.blobs, in)
	if err != nil {
		return Output{}, err
	}
	return Output{Summary: map[string]any{
		"media_type":    doc.MediaType,
		"text_chars":    len([]rune(doc.Text)),
		"segment_count": len(doc.Segments),
		"shape":         doc.Metadata["shape"],
	}}, nil
}

// SegmentStage turns the decoded document into episodes.
type SegmentStage struct {
	store LedgerStore
	blobs BlobReader
	cfg   StageConfig
}

func (SegmentStage) Name() string { return "segment" }
func (SegmentStage) Version() int { return 1 }

func (s SegmentStage) Execute(ctx context.Context, in Input) (Output, error) {
	doc, err := loadDocument(ctx, s.store, s.blobs, in)
	if err != nil {
		return Output{}, err
	}

	now := s.cfg.now()
	episodes := make([]domain.Episode, 0, len(doc.Segments))
	for _, seg := range doc.Segments {
		eventTime := seg.EventTime
		if eventTime == nil {
			// Fall back to the event's world time, never to the current clock: an
			// invented event time would corrupt every temporal query built on it.
			eventTime = in.Event.EventTime
		}
		episodes = append(episodes, domain.Episode{
			WorkspaceID:   in.Event.WorkspaceID,
			GraphSpaceID:  in.Event.GraphSpaceID,
			SourceEventID: in.Event.ID,
			ArtifactID:    in.Event.RawArtifactID,
			Sequence:      seg.Sequence,
			Content:       seg.Content,
			ContentType:   seg.ContentType,
			EventTime:     eventTime,
			ObservedAt:    in.Event.ObservedAt,
			RecordedAt:    now,
			Locator:       seg.Locator,
			// Sensitivity propagates from the source event downstream unless policy
			// explicitly downgrades it (AGENTS.md section 22.3).
			Classification: in.Event.Classification,
			Metadata:       seg.Metadata,
		})
	}

	stored, err := s.store.InsertEpisodes(ctx, episodes)
	if err != nil {
		return Output{}, err
	}
	return Output{Summary: map[string]any{
		"episode_count": len(stored),
		"media_type":    doc.MediaType,
	}}, nil
}

// ChunkStage splits episodes into retrieval units.
type ChunkStage struct {
	store LedgerStore
	cfg   StageConfig
}

func (ChunkStage) Name() string { return "chunk" }
func (ChunkStage) Version() int { return 1 }

func (s ChunkStage) Execute(ctx context.Context, in Input) (Output, error) {
	episodes, err := s.store.ListEpisodes(ctx, in.Event.WorkspaceID, in.Event.ID)
	if err != nil {
		return Output{}, err
	}

	opts := s.cfg.chunkOptions()
	var (
		chunks     []domain.Chunk
		tokenTotal int
	)
	for _, episode := range episodes {
		for _, spec := range normalize.Chunk(episode.Content, opts) {
			tokenTotal += spec.TokenCount
			chunks = append(chunks, domain.Chunk{
				WorkspaceID:    in.Event.WorkspaceID,
				GraphSpaceID:   in.Event.GraphSpaceID,
				SourceEventID:  in.Event.ID,
				EpisodeID:      episode.ID,
				ArtifactID:     episode.ArtifactID,
				Sequence:       spec.Sequence,
				Content:        spec.Content,
				ContentType:    episode.ContentType,
				TokenCount:     spec.TokenCount,
				CharStart:      spec.CharStart,
				CharEnd:        spec.CharEnd,
				ByteStart:      spec.ByteStart,
				ByteEnd:        spec.ByteEnd,
				Locator:        normalize.LocatorForChunk(episode.Locator, spec),
				Classification: episode.Classification,
			})
		}
	}

	stored, err := s.store.InsertChunks(ctx, chunks)
	if err != nil {
		return Output{}, err
	}
	return Output{Summary: map[string]any{
		"chunk_count":   len(stored),
		"episode_count": len(episodes),
		"token_total":   tokenTotal,
	}}, nil
}
