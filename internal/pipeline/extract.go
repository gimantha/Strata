package pipeline

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/gimantha/strata/internal/domain"
	"github.com/gimantha/strata/internal/extraction"
	"github.com/gimantha/strata/internal/knowledge"
)

// Committer records candidate knowledge. It is the same path a human assertion takes, so
// extracted claims get identical treatment: evidence, supersession, conflict detection,
// and provenance all behave the same way.
type Committer interface {
	Assert(ctx context.Context, req knowledge.AssertRequest) (knowledge.AssertResult, error)
}

// ExtractStage asks a model for candidate knowledge and commits what survives validation.
//
// It runs one request per episode, with that episode's chunks as labeled units. Per
// episode rather than per chunk because a fact often spans a chunk boundary; not across
// episodes because a conversation turn and an unrelated document section share no context
// worth mixing.
type ExtractStage struct {
	store     LedgerStore
	extractor *extraction.Extractor
	committer Committer
	cfg       StageConfig
}

// NewExtractStage builds the stage.
func NewExtractStage(store LedgerStore, extractor *extraction.Extractor, committer Committer, cfg StageConfig) ExtractStage {
	return ExtractStage{store: store, extractor: extractor, committer: committer, cfg: cfg}
}

func (ExtractStage) Name() string { return "extract" }

// Version is part of the durable stage key. Bump it when the prompt, schema, or mapping
// changes in a way that should re-extract events already processed.
func (ExtractStage) Version() int { return 1 }

func (s ExtractStage) Execute(ctx context.Context, in Input) (Output, error) {
	const op = "pipeline.ExtractStage"

	episodes, err := s.store.ListEpisodes(ctx, in.Event.WorkspaceID, in.Event.ID)
	if err != nil {
		return Output{}, err
	}
	chunks, err := s.store.ListChunks(ctx, in.Event.WorkspaceID, in.Event.ID)
	if err != nil {
		return Output{}, err
	}

	byEpisode := map[domain.EpisodeID][]domain.Chunk{}
	for _, chunk := range chunks {
		byEpisode[chunk.EpisodeID] = append(byEpisode[chunk.EpisodeID], chunk)
	}

	var (
		requested  int
		committed  int
		duplicates int
		rejected   int
		conflicts  int
		modelRuns  []string
	)

	for _, episode := range episodes {
		episodeChunks := byEpisode[episode.ID]
		if len(episodeChunks) == 0 {
			continue
		}

		units := make([]extraction.SourceUnit, 0, len(episodeChunks))
		for _, chunk := range episodeChunks {
			if strings.TrimSpace(chunk.Content) == "" {
				continue
			}
			units = append(units, extraction.SourceUnit{
				Ref:     string(chunk.ID),
				Content: chunk.Content,
			})
		}
		if len(units) == 0 {
			continue
		}

		result, err := s.extractor.Extract(ctx, extraction.Request{
			Scope:         in.Scope,
			SourceEventID: in.Event.ID,
			Units:         units,
		})
		requested++
		if !domain.IsZero(result.ModelRun.ID) {
			modelRuns = append(modelRuns, string(result.ModelRun.ID))
		}
		if err != nil {
			// Malformed output must not fail the whole event: the run is recorded, the
			// output is discarded, and the remaining episodes still get their chance.
			// A provider that is unreachable is a different matter and is propagated so
			// the work item retries.
			if domain.IsCode(err, domain.CodeInvalidArgument) {
				rejected++
				continue
			}
			return Output{}, err
		}
		rejected += len(result.Rejections)

		claims := s.toClaims(episode, episodeChunks, result)
		if len(claims) == 0 {
			continue
		}

		asserted, err := s.committer.Assert(ctx, knowledge.AssertRequest{
			Scope:         in.Scope,
			Principal:     in.Event.CreatedBy,
			SourceEventID: in.Event.ID,
			Claims:        claims,
		})
		if err != nil {
			// A claim the ledger refuses is bad candidate data, not a reason to fail the
			// event. Extraction proposes; the ledger decides.
			if domain.IsCode(err, domain.CodeInvalidArgument) || domain.IsCode(err, domain.CodeConflict) {
				rejected += len(claims)
				continue
			}
			return Output{}, err
		}
		committed += len(asserted.Assertions) - asserted.Duplicates
		duplicates += asserted.Duplicates
		conflicts += len(asserted.Conflicts)
	}

	_ = op
	return Output{Summary: map[string]any{
		"episodes_examined": len(episodes),
		"model_requests":    requested,
		"assertions":        committed,
		"duplicates":        duplicates,
		"rejected":          rejected,
		"conflicts":         conflicts,
		"model_runs":        modelRuns,
	}}, nil
}

// toClaims maps validated candidates onto claims, attaching evidence.
//
// Nothing here trusts the model with anything structural. Scope, principal, source event,
// knowledge time, and provenance mode are supplied by the pipeline; the candidate
// contributes only what the source said.
func (s ExtractStage) toClaims(episode domain.Episode, chunks []domain.Chunk, result extraction.Result) []knowledge.Claim {
	aliasesByName := map[string][]string{}
	typesByName := map[string]string{}
	for _, entity := range result.Candidates.Entities {
		key := domain.NormalizeAlias(entity.Name)
		aliasesByName[key] = entity.Aliases
		typesByName[key] = entity.EntityType
	}

	claims := make([]knowledge.Claim, 0, len(result.Candidates.Assertions))
	for _, candidate := range result.Candidates.Assertions {
		subjectType := candidate.SubjectType
		if known, ok := typesByName[domain.NormalizeAlias(candidate.SubjectName)]; ok && known != "" {
			subjectType = known
		}

		claim := knowledge.Claim{
			Subject:   knowledge.EntityRef{Name: candidate.SubjectName, Type: subjectType},
			Predicate: candidate.Predicate,
			ScopeKey:  candidate.ScopeKey,
			ValidFrom: candidate.ValidFrom,
			ValidTo:   candidate.ValidTo,
			EventTime: candidate.EventTime,
			// A model's self-reported score is the extraction component of confidence,
			// not the whole of it. Later phases fold in source trust and corroboration.
			Confidence: candidate.Confidence,
			ConfidenceBreakdown: &domain.ConfidenceBreakdown{
				Extraction: floatPtr(candidate.Confidence),
			},
			// Extracted, never inferred: the model reported what the source states.
			ProvenanceMode:   domain.ProvenanceExtracted,
			Quarantine:       candidate.Quarantine,
			QuarantineReason: candidate.QuarantineReason,
		}

		if candidate.ObjectEntityName != "" {
			objectType := candidate.ObjectEntityType
			if known, ok := typesByName[domain.NormalizeAlias(candidate.ObjectEntityName)]; ok && known != "" {
				objectType = known
			}
			claim.ObjectEntity = &knowledge.EntityRef{
				Name: candidate.ObjectEntityName,
				Type: objectType,
			}
		} else {
			object, err := literalObject(candidate)
			if err != nil {
				continue
			}
			claim.Object = object
		}

		// Attribute the claim to the chunk its quote came from when it localizes to one,
		// and to the episode otherwise. Evidence always reaches at least the episode.
		evidence := knowledge.EvidenceInput{
			EpisodeID:     episode.ID,
			ExtractedText: candidate.EvidenceQuote,
			Confidence:    candidate.Confidence,
		}
		if !domain.IsZero(result.ModelRun.ID) {
			runID := result.ModelRun.ID
			evidence.ModelRunID = &runID
		}
		if chunk, ok := locateQuote(chunks, candidate.EvidenceQuote); ok {
			chunkID := chunk.ID
			evidence.ChunkID = &chunkID
		}
		claim.Evidence = []knowledge.EvidenceInput{evidence}

		claims = append(claims, claim)
	}
	return claims
}

// locateQuote finds the chunk a quote came from.
func locateQuote(chunks []domain.Chunk, quote string) (domain.Chunk, bool) {
	needle := strings.Join(strings.Fields(strings.ToLower(quote)), " ")
	if needle == "" {
		return domain.Chunk{}, false
	}
	for _, chunk := range chunks {
		haystack := strings.Join(strings.Fields(strings.ToLower(chunk.Content)), " ")
		if strings.Contains(haystack, needle) {
			return chunk, true
		}
	}
	return domain.Chunk{}, false
}

// literalObject converts a candidate's textual value into a typed object.
func literalObject(candidate domain.AssertionCandidate) (domain.AssertionObject, error) {
	const op = "pipeline.literalObject"

	value := strings.TrimSpace(candidate.ObjectValue)
	switch candidate.ObjectKind {
	case domain.ObjectString:
		return domain.ObjectOfString(value), nil
	case domain.ObjectSymbol:
		return domain.ObjectOfSymbol(value), nil
	case domain.ObjectURI:
		return domain.ObjectOfURI(value), nil
	case domain.ObjectDecimal:
		object := domain.ObjectOfDecimal(value)
		return object, object.Validate()
	case domain.ObjectBoolean:
		return domain.ObjectOfBool(strings.EqualFold(value, "true")), nil
	case domain.ObjectInteger:
		parsed, err := parseInt64(value)
		if err != nil {
			return domain.AssertionObject{}, err
		}
		return domain.ObjectOfInteger(parsed), nil
	case domain.ObjectTimestamp, domain.ObjectDate, domain.ObjectDuration:
		object, err := parseTemporalObject(candidate.ObjectKind, value)
		if err != nil {
			return domain.AssertionObject{}, err
		}
		return object, nil
	default:
		return domain.AssertionObject{}, domain.Errorf(domain.CodeInvalidArgument, op,
			"object kind %q cannot be built from text", candidate.ObjectKind)
	}
}

func floatPtr(v float64) *float64 { return &v }

// parseInt64 converts a model's textual integer.
func parseInt64(value string) (int64, error) {
	const op = "pipeline.parseInt64"

	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, domain.Errorf(domain.CodeInvalidArgument, op, "%q is not an integer", value)
	}
	return parsed, nil
}

// parseTemporalObject converts a model's textual timestamp, date, or duration.
func parseTemporalObject(kind domain.ObjectKind, value string) (domain.AssertionObject, error) {
	const op = "pipeline.parseTemporalObject"

	switch kind {
	case domain.ObjectDuration:
		parsed, err := time.ParseDuration(value)
		if err != nil {
			return domain.AssertionObject{}, domain.Errorf(domain.CodeInvalidArgument, op,
				"%q is not a duration", value)
		}
		return domain.ObjectOfDuration(parsed), nil
	case domain.ObjectDate:
		parsed, err := time.Parse(domain.DateLayout, value)
		if err != nil {
			return domain.AssertionObject{}, domain.Errorf(domain.CodeInvalidArgument, op,
				"%q is not a %s date", value, domain.DateLayout)
		}
		return domain.ObjectOfDate(parsed), nil
	default:
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
			if parsed, err := time.Parse(layout, value); err == nil {
				return domain.ObjectOfTimestamp(parsed), nil
			}
		}
		return domain.AssertionObject{}, domain.Errorf(domain.CodeInvalidArgument, op,
			"%q is not an RFC3339 timestamp", value)
	}
}
