package pipeline

import (
	"context"
	"encoding/json"
	"time"

	"github.com/gimantha/strata/internal/domain"
	"github.com/gimantha/strata/internal/knowledge"
)

// ChangeStore is what the change stage reads and writes beyond the shared ledger surface.
type ChangeStore interface {
	GetCDCStream(ctx context.Context, ws domain.WorkspaceID, source domain.SourceID, stream string) (domain.CDCStream, error)
	CurrentClaimsForRecord(ctx context.Context, ws domain.WorkspaceID, source domain.SourceID, externalID string) ([]domain.Assertion, error)
	WithRecordLock(ctx context.Context, ws domain.WorkspaceID, source domain.SourceID, externalID string, fn func(context.Context) error) error
	RetractSourceRecord(ctx context.Context, ws domain.WorkspaceID, source domain.SourceID, externalID string, at time.Time, reason string, actor domain.PrincipalID) ([]domain.AssertionID, error)
}

// ChangeStage turns one upstream row change into knowledge (AGENTS.md section 11.2).
//
// Deterministic, with no model involved. A database row is already structured: the columns
// say what the predicates are and the primary key says what the subject is. Asking a model
// to rediscover that would be slower, more expensive, and less reliable than reading the
// mapping — the same argument the phase 1 stages make about conversation turns and markdown
// headings.
//
// The stage does not delete and rebuild the subject's subgraph. It states what the row now
// says; claims that are unchanged collide on their fingerprint and are left exactly as they
// were, and claims whose value moved are superseded by the reconciler. That is what makes an
// update cheap and an assertion id stable across a hundred unrelated column edits.
type ChangeStage struct {
	store     LedgerStore
	changes   ChangeStore
	committer Committer
	blobs     BlobReader
	cfg       StageConfig
}

// NewChangeStage builds the stage.
func NewChangeStage(store LedgerStore, changes ChangeStore, committer Committer, blobs BlobReader, cfg StageConfig) ChangeStage {
	return ChangeStage{store: store, changes: changes, committer: committer, blobs: blobs, cfg: cfg}
}

func (ChangeStage) Name() string { return "changes" }

// Version is part of the durable stage key. Bump it when the mapping semantics change in a
// way that should re-derive knowledge from events already processed.
func (ChangeStage) Version() int { return 1 }

func (s ChangeStage) Execute(ctx context.Context, in Input) (Output, error) {
	const op = "pipeline.ChangeStage"

	if in.Event.EventType != domain.EventTypeChangeRow {
		// Not a row change. Every other kind of source material passes through
		// untouched, which is what lets one pipeline serve documents and databases.
		return Output{Summary: map[string]any{"skipped": "not a change event"}}, nil
	}

	artifact, err := s.store.GetArtifact(ctx, in.Event.WorkspaceID, in.Event.RawArtifactID)
	if err != nil {
		return Output{}, err
	}
	raw, err := s.blobs.Get(ctx, artifact.BlobKey)
	if err != nil {
		return Output{}, err
	}

	var event domain.ChangeEvent
	if err := json.Unmarshal(raw, &event); err != nil {
		return Output{}, domain.Wrap(err, domain.CodeInvalidArgument, op,
			"the archived change event cannot be decoded")
	}

	stream, err := s.changes.GetCDCStream(ctx, in.Event.WorkspaceID, in.Event.SourceID, event.Stream)
	if err != nil {
		if domain.IsCode(err, domain.CodeNotFound) {
			// The change is archived and the event is recorded; only the interpretation
			// is missing. Registering a mapping later and re-running this stage picks it
			// up, which is better than failing an ingest nobody can retry.
			return Output{Summary: map[string]any{
				"stream":  event.Stream,
				"skipped": "no mapping is registered for this stream",
			}}, nil
		}
		return Output{}, err
	}

	// Everything below reads what is believed about this record and then writes to it.
	// Workers claim outbox items independently, so two changes to one row would otherwise
	// be applied concurrently, each seeing the state before the other.
	var out Output
	err = s.changes.WithRecordLock(ctx, in.Event.WorkspaceID, in.Event.SourceID, event.RecordID(),
		func(ctx context.Context) error {
			var err error
			out, err = s.apply(ctx, in, event, stream)
			return err
		})
	return out, err
}

// apply maps one change onto knowledge, holding the record's lock.
func (s ChangeStage) apply(ctx context.Context, in Input, event domain.ChangeEvent, stream domain.CDCStream) (Output, error) {
	now := s.cfg.now()
	if event.Operation == domain.ChangeDelete {
		return s.tombstone(ctx, in, event, now)
	}

	claims, unchanged, err := s.claimsFor(ctx, in, event, stream.Mapping)
	if err != nil {
		return Output{}, err
	}
	if len(claims) == 0 {
		return Output{Summary: map[string]any{
			"stream": event.Stream, "record": event.RecordID(),
			"claims": 0, "unchanged": unchanged,
			"upstream_changed": event.ChangedColumns(),
		}}, nil
	}

	result, err := s.committer.Assert(ctx, knowledge.AssertRequest{
		Scope:         in.Scope,
		Principal:     in.Event.CreatedBy,
		SourceEventID: in.Event.ID,
		Claims:        claims,
		// A row that does not fit the schema is a configuration problem between the
		// mapping and the ontology, not a caller mistake, and the row is worth keeping
		// while someone works out which of the two is wrong.
		OnViolation: domain.DispositionQuarantine,
	})
	if err != nil {
		return Output{}, err
	}

	return Output{Summary: map[string]any{
		"stream":     event.Stream,
		"record":     event.RecordID(),
		"operation":  string(event.Operation),
		"claims":     len(claims),
		"assertions": len(result.Assertions) - result.Duplicates,
		// Two different kinds of "nothing to do": columns whose value already matches what
		// is believed, and claims the ledger recognized as an exact replay.
		"unchanged":  unchanged,
		"duplicates": result.Duplicates,
		// What the upstream itself said moved, when it sent a before image. Worth having
		// beside our own answer: a disagreement between the two is a mapping bug.
		"upstream_changed": event.ChangedColumns(),
		"superseded":       len(result.Superseded),
		// A change processed after a later one from the same record is recorded and then
		// immediately superseded. Workers claim events independently, so this is routine
		// rather than exceptional, and a summary that omitted it would look like a claim
		// went missing.
		"superseded_on_arrival": len(result.SupersededOnArrival),
		"conflicts":             len(result.Conflicts),
		"quarantined":           len(result.Quarantined),
	}}, nil
}

// tombstone records that the source stopped claiming a record (AGENTS.md section 11.3).
//
// Retraction, not deletion. The claims keep their evidence and stay answerable as of any
// earlier knowledge time; what changes is that the system stops believing them now. A
// deleted row is news about the source's current position, not proof the fact was never
// true — and privacy erasure is a different workflow with different authorization.
func (s ChangeStage) tombstone(ctx context.Context, in Input, event domain.ChangeEvent, now time.Time) (Output, error) {
	retracted, err := s.changes.RetractSourceRecord(ctx, in.Event.WorkspaceID, in.Event.SourceID,
		event.RecordID(), now, "source tombstone: the upstream record was deleted",
		in.Event.CreatedBy.ID)
	if err != nil {
		return Output{}, err
	}

	ids := make([]string, 0, len(retracted))
	for _, id := range retracted {
		ids = append(ids, string(id))
	}
	return Output{Summary: map[string]any{
		"stream":     event.Stream,
		"record":     event.RecordID(),
		"operation":  string(domain.ChangeDelete),
		"retracted":  len(retracted),
		"assertions": ids,
	}}, nil
}

// claimsFor maps a row image onto the claims that are actually new, and reports how many
// columns were left alone because their value is already believed.
//
// A CDC stream re-sends every column on every update. Asserting all of them would record the
// same unchanged value again under a new source event id — a different fingerprint, a second
// active claim, and unbounded growth for a table written hourly. Deciding which columns are
// "unchanged" is what AGENTS.md section 11.2 asks the system to do, and doing it against
// current belief works whether or not the connector sent a before image.
func (s ChangeStage) claimsFor(ctx context.Context, in Input, event domain.ChangeEvent, mapping domain.ChangeMapping) ([]knowledge.Claim, int, error) {
	episodes, err := s.store.ListEpisodes(ctx, in.Event.WorkspaceID, in.Event.ID)
	if err != nil {
		return nil, 0, err
	}

	believed, err := s.changes.CurrentClaimsForRecord(ctx, in.Event.WorkspaceID,
		in.Event.SourceID, event.RecordID())
	if err != nil {
		return nil, 0, err
	}
	current := make(map[string]string, len(believed))
	for _, claim := range believed {
		current[claim.Predicate.Name] = claim.Object.Key()
	}

	subject := knowledge.EntityRef{
		Name: mapping.SubjectName(event),
		Type: mapping.SubjectType,
	}
	if namespace, value, ok := mapping.ExternalIdentifier(event); ok {
		// The upstream primary key, as a domain key. This is rung one of the resolution
		// ladder and the reason CDC identities do not drift: two rows with the same key
		// are the same subject however the name is spelled today.
		subject.DomainKeys = []domain.DomainKey{{Namespace: namespace, Value: value}}
	}
	// The source's own identity for the record, which is the other half of the same idea.
	sourceID := in.Event.SourceID
	subject.SourceID = &sourceID
	subject.ExternalID = event.RecordID()

	// World validity comes from the row's own columns, or from nowhere.
	//
	// Stamping the commit time as valid_from would be inventing information: a row touched
	// in February does not mean the company's legal name became true in February. It also
	// breaks deduplication, because the same unchanged value would fingerprint differently
	// on every update and accumulate a new assertion per touch — unbounded growth for a
	// table that is written hourly. Knowledge time already records when we learned it.
	validFrom := mapping.TimeAt(event, mapping.ValidFromColumn)
	validTo := mapping.TimeAt(event, mapping.ValidToColumn)

	var evidence []knowledge.EvidenceInput
	if len(episodes) > 0 {
		evidence = []knowledge.EvidenceInput{{
			EpisodeID: episodes[0].ID,
			// The row itself is the quote. A column value with no surrounding prose is
			// still verbatim source, and citing it keeps CDC-derived claims as checkable
			// as extracted ones.
			ExtractedText: truncateQuote(string(mustJSON(event.Image()))),
		}}
	}

	cells := mapping.Cells(event)
	claims := make([]knowledge.Claim, 0, len(cells))
	unchanged := 0

	for _, cell := range cells {
		// An entity-valued cell is compared after resolution, not here: the object key of
		// a relation is an entity id, and this stage does not resolve names to ids.
		if cell.EntityName == "" {
			if believedKey, known := current[cell.Predicate]; known && believedKey == cell.Object.Key() {
				unchanged++
				continue
			}
		}

		claim := knowledge.Claim{
			Subject:   subject,
			Predicate: cell.Predicate,
			Object:    cell.Object,
			ValidFrom: validFrom,
			ValidTo:   validTo,
			// Imported rather than extracted: the value was read from a field, not
			// inferred from prose, and the distinction matters when weighing a
			// contradiction between the two.
			ProvenanceMode: domain.ProvenanceImported,
			Confidence:     1,
			Evidence:       evidence,
		}
		if cell.EntityName != "" {
			claim.ObjectEntity = &knowledge.EntityRef{Name: cell.EntityName, Type: cell.EntityType}
		}
		claims = append(claims, claim)
	}
	return claims, unchanged, nil
}

// maxRowQuote bounds the stored evidence excerpt. Evidence is a pointer into archived
// source, not a second copy of it.
const maxRowQuote = 1000

func truncateQuote(text string) string {
	if len(text) <= maxRowQuote {
		return text
	}
	return text[:maxRowQuote]
}

func mustJSON(value any) []byte {
	encoded, err := json.Marshal(value)
	if err != nil {
		return []byte("{}")
	}
	return encoded
}
