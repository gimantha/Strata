// Package projection maintains the derived retrieval stores.
//
// Everything here is rebuildable. The vector, lexical, and graph projections hold no history
// and no provenance of their own: they point back at canonical records and copy only what a
// query needs to filter on. Deleting them entirely and replaying loses nothing, which is the
// property that keeps the ledger authoritative rather than merely first
// (AGENTS.md sections 2.3, 15.2).
package projection

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/gimantha/strata/internal/domain"
	"github.com/gimantha/strata/internal/embedding"
)

// Names of the projections, as recorded on checkpoints.
const (
	ProjectionVector  = "vector"
	ProjectionLexical = "lexical"
	ProjectionGraph   = "graph"
)

// Store is the persistence the projector needs, declared by its consumer.
type Store interface {
	GetSourceEvent(ctx context.Context, ws domain.WorkspaceID, id domain.SourceEventID) (domain.SourceEvent, error)
	ListSourceEventIDsAfter(ctx context.Context, ws domain.WorkspaceID, after *time.Time, afterID domain.SourceEventID, limit int) ([]domain.SourceEvent, error)
	ListChunks(ctx context.Context, ws domain.WorkspaceID, eventID domain.SourceEventID) ([]domain.Chunk, error)
	ListEntities(ctx context.Context, scope domain.Scope, entityType string, limit int) ([]domain.Entity, error)
	GetEntity(ctx context.Context, ws domain.WorkspaceID, id domain.EntityID) (domain.Entity, error)
	QueryAssertions(ctx context.Context, q domain.AssertionQuery) ([]domain.Assertion, error)

	UpsertVectors(ctx context.Context, records []domain.VectorRecord) error
	UpsertLexical(ctx context.Context, records []domain.ProjectedRecord) error
	UpsertGraphEdges(ctx context.Context, edges []domain.GraphEdge) error
	ExistingVectorHashes(ctx context.Context, ws domain.WorkspaceID, model string, version int, surface domain.Surface, recordIDs []string) (map[string]string, error)

	DeleteProjections(ctx context.Context, ws domain.WorkspaceID) error
	SaveCheckpoint(ctx context.Context, checkpoint domain.ProjectionCheckpoint) error
	GetCheckpoint(ctx context.Context, ws domain.WorkspaceID, projection string) (domain.ProjectionCheckpoint, error)
}

// Projector writes canonical records into the retrieval projections.
type Projector struct {
	store    Store
	embedder embedding.Embedder
	now      func() time.Time
	logger   *slog.Logger
	tracer   trace.Tracer
}

// Options configures the projector.
type Options struct {
	Now func() time.Time
}

// New builds a projector. The embedder may be nil, in which case the lexical and graph
// projections are still maintained: text search and traversal do not need a model, and a
// deployment without one should not lose them.
func New(store Store, embedder embedding.Embedder, opts Options, logger *slog.Logger, tracer trace.Tracer) *Projector {
	now := opts.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	if tracer == nil {
		tracer = tracenoop.NewTracerProvider().Tracer("projection")
	}
	return &Projector{store: store, embedder: embedder, now: now, logger: logger, tracer: tracer}
}

// Stats reports what a projection run wrote.
type Stats struct {
	Vectors  int
	Lexical  int
	Edges    int
	Embedded int
	// Reused counts records whose text was unchanged, so no embedding was requested.
	Reused int
	Events int
}

// Add accumulates one run's stats into another.
func (s *Stats) Add(other Stats) {
	s.Vectors += other.Vectors
	s.Lexical += other.Lexical
	s.Edges += other.Edges
	s.Embedded += other.Embedded
	s.Reused += other.Reused
	s.Events += other.Events
}

// ProjectEvent writes everything derived from one source event into the projections.
func (p *Projector) ProjectEvent(ctx context.Context, scope domain.Scope, eventID domain.SourceEventID) (Stats, error) {
	ctx, span := p.tracer.Start(ctx, "projection.ProjectEvent", trace.WithAttributes(
		attribute.String("strata.source_event_id", string(eventID)),
	))
	defer span.End()

	var stats Stats

	chunkStats, err := p.projectChunks(ctx, scope, eventID)
	if err != nil {
		return Stats{}, err
	}
	stats.Add(chunkStats)

	assertionStats, err := p.projectAssertions(ctx, scope, eventID)
	if err != nil {
		return Stats{}, err
	}
	stats.Add(assertionStats)

	stats.Events = 1
	return stats, nil
}

// projectChunks indexes retrieval units.
//
// Chunks are indexed rather than whole episodes: an episode's text is covered by its chunks,
// and indexing both would return the same passage twice under different surfaces, which
// fusion in phase 7 would then have to undo.
func (p *Projector) projectChunks(ctx context.Context, scope domain.Scope, eventID domain.SourceEventID) (Stats, error) {
	chunks, err := p.store.ListChunks(ctx, scope.WorkspaceID, eventID)
	if err != nil {
		return Stats{}, err
	}
	if len(chunks) == 0 {
		return Stats{}, nil
	}

	records := make([]domain.ProjectedRecord, 0, len(chunks))
	for _, chunk := range chunks {
		if strings.TrimSpace(chunk.Content) == "" {
			continue
		}
		records = append(records, domain.ProjectedRecord{
			Scope:          domain.Scope{WorkspaceID: chunk.WorkspaceID, GraphSpaceID: chunk.GraphSpaceID},
			Surface:        domain.SurfaceChunk,
			RecordID:       string(chunk.ID),
			Content:        chunk.Content,
			Classification: chunk.Classification,
			SourceEventID:  chunk.SourceEventID,
		})
	}
	return p.write(ctx, scope, records)
}

// projectAssertions indexes committed knowledge.
//
// A claim is rendered as a sentence so it can be found by meaning as well as by structure:
// "Acme SUPPLIES industrial fasteners" is searchable in a way that a row of identifiers is
// not.
func (p *Projector) projectAssertions(ctx context.Context, scope domain.Scope, eventID domain.SourceEventID) (Stats, error) {
	assertions, err := p.store.QueryAssertions(ctx, domain.AssertionQuery{
		Scope:         scope,
		SourceEventID: eventID,
		// Superseded and retracted claims are projected too: retrieval filters by status
		// and knowledge time, and a projection that only held current belief could not
		// answer a question about what was believed last month.
		IncludeSuperseded: true,
		Limit:             domain.MaxAssertionLimit,
	})
	if err != nil {
		return Stats{}, err
	}
	// A quarantined claim is not knowledge yet: it failed schema validation or carried an
	// instruction-like quote, and it is held for review. Projecting it would make it
	// retrievable, and retrievable is indistinguishable from believed once a claim is in
	// a context block.
	kept := assertions[:0]
	for _, assertion := range assertions {
		if assertion.Status != domain.AssertionQuarantined {
			kept = append(kept, assertion)
		}
	}
	assertions = kept

	if len(assertions) == 0 {
		return Stats{}, nil
	}

	names := map[domain.EntityID]string{}
	types := map[domain.EntityID]string{}
	nameOf := func(id domain.EntityID) string {
		if name, ok := names[id]; ok {
			return name
		}
		entity, err := p.store.GetEntity(ctx, scope.WorkspaceID, id)
		if err != nil {
			names[id] = string(id)
			return names[id]
		}
		names[id] = entity.CanonicalName
		types[id] = entity.EntityType
		return entity.CanonicalName
	}

	records := make([]domain.ProjectedRecord, 0, len(assertions))
	edges := make([]domain.GraphEdge, 0, len(assertions))

	for _, assertion := range assertions {
		object := assertion.Object.Display()
		if assertion.Object.Kind == domain.ObjectEntity {
			object = nameOf(assertion.Object.EntityID)
		}

		// Resolved before the literal below rather than inside it: nameOf populates the
		// type cache as a side effect, and field evaluation order in a composite literal
		// is not something to depend on.
		subjectName := nameOf(assertion.SubjectID)
		subjectType := domain.NormalizeEntityType(types[assertion.SubjectID])

		records = append(records, domain.ProjectedRecord{
			Scope: domain.Scope{
				WorkspaceID: assertion.WorkspaceID, GraphSpaceID: assertion.GraphSpaceID,
			},
			Surface:        domain.SurfaceAssertion,
			RecordID:       string(assertion.ID),
			Content:        renderClaim(subjectName, assertion.Predicate.Name, object),
			ValidFrom:      assertion.Temporal.ValidFrom,
			ValidTo:        assertion.Temporal.ValidTo,
			Status:         string(assertion.Status),
			Classification: assertion.Classification,
			MemoryKind:     assertion.MemoryKind,
			SourceEventID:  assertion.SourceEventID,
			// The subject's type, so a query can ask for claims about organizations
			// without joining back to the ledger for every candidate.
			EntityType: subjectType,
		})

		// Only entity-to-entity claims become edges.
		if assertion.Object.Kind == domain.ObjectEntity {
			edges = append(edges, domain.GraphEdge{
				WorkspaceID:    assertion.WorkspaceID,
				GraphSpaceID:   assertion.GraphSpaceID,
				SubjectID:      assertion.SubjectID,
				Predicate:      assertion.Predicate.Name,
				ObjectEntityID: assertion.Object.EntityID,
				AssertionID:    assertion.ID,
				ValidFrom:      assertion.Temporal.ValidFrom,
				ValidTo:        assertion.Temporal.ValidTo,
				Status:         assertion.Status,
				Confidence:     assertion.Confidence,
				Classification: assertion.Classification,
			})
		}
	}

	stats, err := p.write(ctx, scope, records)
	if err != nil {
		return Stats{}, err
	}

	if len(edges) > 0 {
		if err := p.store.UpsertGraphEdges(ctx, edges); err != nil {
			return Stats{}, err
		}
		stats.Edges = len(edges)
	}
	return stats, nil
}

// ProjectEntities indexes identities so they can be found by name.
//
// This is what makes an exact-identifier lookup work: a question naming an entity should
// reach that entity directly rather than through whichever passage happens to mention it.
func (p *Projector) ProjectEntities(ctx context.Context, scope domain.Scope) (Stats, error) {
	entities, err := p.store.ListEntities(ctx, scope, "", domain.MaxAssertionLimit)
	if err != nil {
		return Stats{}, err
	}
	if len(entities) == 0 {
		return Stats{}, nil
	}

	records := make([]domain.ProjectedRecord, 0, len(entities))
	for _, entity := range entities {
		// A merged identity is reachable through the one it merged into, so indexing it
		// separately would return a redirect as if it were a result.
		if entity.RetiredAt != nil {
			continue
		}
		records = append(records, domain.ProjectedRecord{
			Scope:          domain.Scope{WorkspaceID: entity.WorkspaceID, GraphSpaceID: entity.GraphSpaceID},
			Surface:        domain.SurfaceEntity,
			RecordID:       string(entity.ID),
			Content:        entity.CanonicalName + " (" + entity.EntityType + ")",
			Classification: domain.ClassificationInternal,
			EntityType:     domain.NormalizeEntityType(entity.EntityType),
		})
	}
	return p.write(ctx, scope, records)
}

// write sends records to the lexical projection and, when an embedder is configured, the
// vector projection.
func (p *Projector) write(ctx context.Context, scope domain.Scope, records []domain.ProjectedRecord) (Stats, error) {
	if len(records) == 0 {
		return Stats{}, nil
	}

	if err := p.store.UpsertLexical(ctx, records); err != nil {
		return Stats{}, err
	}
	stats := Stats{Lexical: len(records)}

	if p.embedder == nil {
		return stats, nil
	}

	vectors, embedded, reused, err := p.embed(ctx, scope, records)
	if err != nil {
		return Stats{}, err
	}
	if len(vectors) > 0 {
		if err := p.store.UpsertVectors(ctx, vectors); err != nil {
			return Stats{}, err
		}
	}
	stats.Vectors = len(vectors)
	stats.Embedded = embedded
	stats.Reused = reused
	return stats, nil
}

// embed turns records into vectors, skipping text that is already embedded unchanged.
func (p *Projector) embed(ctx context.Context, scope domain.Scope, records []domain.ProjectedRecord) ([]domain.VectorRecord, int, int, error) {
	const op = "projection.embed"

	surface := records[0].Surface
	ids := make([]string, 0, len(records))
	for _, record := range records {
		ids = append(ids, record.RecordID)
	}

	existing, err := p.store.ExistingVectorHashes(ctx, scope.WorkspaceID,
		p.embedder.Model(), p.embedder.Version(), surface, ids)
	if err != nil {
		return nil, 0, 0, err
	}

	var (
		pending []domain.ProjectedRecord
		hashes  []string
		reused  int
	)
	for _, record := range records {
		hash := domain.ContentHash([]byte(record.Content))
		if existing[record.RecordID] == hash {
			reused++
			continue
		}
		pending = append(pending, record)
		hashes = append(hashes, hash)
	}
	if len(pending) == 0 {
		return nil, 0, reused, nil
	}

	out := make([]domain.VectorRecord, 0, len(pending))
	for start := 0; start < len(pending); start += embedding.MaxBatch {
		end := min(start+embedding.MaxBatch, len(pending))
		batch := pending[start:end]

		texts := make([]string, 0, len(batch))
		for _, record := range batch {
			texts = append(texts, record.Content)
		}

		vectors, err := p.embedder.Embed(ctx, texts)
		if err != nil {
			return nil, 0, 0, err
		}
		if len(vectors) != len(batch) {
			return nil, 0, 0, domain.Errorf(domain.CodeProviderUnavailable, op,
				"embedder returned %d vectors for %d inputs", len(vectors), len(batch))
		}

		for i, record := range batch {
			if err := vectors[i].Validate(p.embedder.Dimensions()); err != nil {
				return nil, 0, 0, err
			}
			out = append(out, domain.VectorRecord{
				ProjectedRecord: record,
				Model:           p.embedder.Model(),
				Version:         p.embedder.Version(),
				Embedding:       vectors[i],
				ContentHash:     hashes[start+i],
			})
		}
	}
	return out, len(pending), reused, nil
}

// Rebuild drops every projection for a workspace and replays it from the ledger.
//
// This is scenario I from AGENTS.md section 37, and it is the same code path incremental
// projection uses - a rebuild is not a separate implementation that might drift, it is the
// normal path run over everything.
func (p *Projector) Rebuild(ctx context.Context, ws domain.WorkspaceID) (Stats, error) {
	ctx, span := p.tracer.Start(ctx, "projection.Rebuild", trace.WithAttributes(
		attribute.String("strata.workspace_id", string(ws)),
	))
	defer span.End()

	started := p.now()
	if err := p.store.DeleteProjections(ctx, ws); err != nil {
		return Stats{}, err
	}

	var (
		stats     Stats
		cursor    *time.Time
		cursorID  domain.SourceEventID
		seenScope = map[domain.GraphSpaceID]bool{}
	)

	for {
		events, err := p.store.ListSourceEventIDsAfter(ctx, ws, cursor, cursorID, 200)
		if err != nil {
			return Stats{}, err
		}
		if len(events) == 0 {
			break
		}

		for _, event := range events {
			scope := domain.Scope{WorkspaceID: event.WorkspaceID, GraphSpaceID: event.GraphSpaceID}
			seenScope[event.GraphSpaceID] = true

			eventStats, err := p.ProjectEvent(ctx, scope, event.ID)
			if err != nil {
				return Stats{}, err
			}
			stats.Add(eventStats)

			recorded := event.RecordedAt
			cursor, cursorID = &recorded, event.ID
		}

		// Checkpoint after each page rather than only at the end, so an interrupted
		// rebuild resumes rather than starting over.
		if err := p.saveCheckpoints(ctx, ws, cursor, cursorID, stats, nil); err != nil {
			return Stats{}, err
		}
	}

	// Entities are not tied to a single event, so they are projected once per graph space
	// after the events that created them.
	for graphSpace := range seenScope {
		entityStats, err := p.ProjectEntities(ctx,
			domain.Scope{WorkspaceID: ws, GraphSpaceID: graphSpace})
		if err != nil {
			return Stats{}, err
		}
		stats.Add(entityStats)
	}

	if err := p.saveCheckpoints(ctx, ws, cursor, cursorID, stats, &started); err != nil {
		return Stats{}, err
	}

	if p.logger != nil {
		p.logger.InfoContext(ctx, "projections rebuilt",
			slog.Int("events", stats.Events),
			slog.Int("vectors", stats.Vectors),
			slog.Int("lexical", stats.Lexical),
			slog.Int("edges", stats.Edges),
			slog.Int("embedded", stats.Embedded),
			slog.Int("reused", stats.Reused))
	}
	return stats, nil
}

// Advance records that incremental projection has consumed up to an event.
func (p *Projector) Advance(ctx context.Context, ws domain.WorkspaceID, event domain.SourceEvent, stats Stats) error {
	recorded := event.RecordedAt
	return p.saveCheckpoints(ctx, ws, &recorded, event.ID, stats, nil)
}

func (p *Projector) saveCheckpoints(ctx context.Context, ws domain.WorkspaceID, cursor *time.Time, cursorID domain.SourceEventID, stats Stats, rebuiltAt *time.Time) error {
	counts := map[string]int{
		ProjectionVector:  stats.Vectors,
		ProjectionLexical: stats.Lexical,
		ProjectionGraph:   stats.Edges,
	}
	for name, count := range counts {
		if err := p.store.SaveCheckpoint(ctx, domain.ProjectionCheckpoint{
			WorkspaceID:      ws,
			Projection:       name,
			LastRecordedAt:   cursor,
			LastRecordID:     string(cursorID),
			RecordsProjected: int64(count),
			RebuiltAt:        rebuiltAt,
		}); err != nil {
			return err
		}
	}
	return nil
}

// renderClaim turns a claim into a searchable sentence.
func renderClaim(subject, predicate, object string) string {
	readable := strings.ToLower(strings.ReplaceAll(predicate, "_", " "))
	return strings.TrimSpace(subject + " " + readable + " " + object)
}
