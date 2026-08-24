package contextblock

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/gimantha/strata/internal/domain"
)

// candidate is one thing that could go in the block, with everything selection needs.
//
// Hydration happens before selection rather than after, because the properties selection
// weighs — does this claim have evidence, is it disputed, does its validity cover the
// question — are properties of the canonical record, not of the retrieval hit.
type candidate struct {
	section  domain.ContextSection
	text     string
	surface  domain.Surface
	recordID string

	relevance  float64
	confidence float64
	// evidence is quality in [0,1]: whether the claim is cited at all, and whether the
	// citation carries a quote rather than only a pointer.
	evidence float64
	// temporal is fit in [0,1] against the requested instant.
	temporal float64
	// priority is the memory-kind weight.
	priority float64

	// subjects and predicate drive coverage: a second claim about an entity already
	// covered adds less than the first claim about a new one.
	subjects  []domain.EntityID
	predicate string
	// slot is subject and predicate together, and objectKey is the value asserted for
	// it. Two claims filling the same slot with different values are the opposite of
	// redundant, however similarly they read.
	slot      string
	objectKey string

	citation domain.Citation
	conflict *domain.ConflictNote

	// terms is the normalized token set, for redundancy comparison.
	terms map[string]struct{}
}

// Store is the canonical surface hydration reads, declared by its consumer.
type Store interface {
	ProvenanceChain(ctx context.Context, ws domain.WorkspaceID, id domain.AssertionID) (domain.ProvenanceChain, error)
	ChunkProvenance(ctx context.Context, ws domain.WorkspaceID, ids []domain.ChunkID) (map[domain.ChunkID]domain.ChunkProvenance, error)
	ConflictMembers(ctx context.Context, ws domain.WorkspaceID, id domain.ConflictSetID) (domain.ConflictSet, []domain.Assertion, error)
	QueryAssertions(ctx context.Context, q domain.AssertionQuery) ([]domain.Assertion, error)
	GetEntity(ctx context.Context, ws domain.WorkspaceID, id domain.EntityID) (domain.Entity, error)
}

// hydrator turns retrieval hits into citable candidates.
type hydrator struct {
	store Store
	req   domain.ContextRequest
	now   time.Time

	entities map[domain.EntityID]domain.Entity
	dropped  []domain.DroppedItem
}

// EntityExpansionLimit bounds how many claims one matched identity contributes.
//
// An entity hit is a strong signal about the subject and no signal at all about which fact
// the asker wants, so it contributes its best-evidenced current claims and lets selection
// decide. Unbounded expansion would let one popular entity crowd out everything else before
// redundancy reduction ever ran.
const EntityExpansionLimit = 5

func (h *hydrator) hydrate(ctx context.Context, items []domain.RetrievedItem) ([]candidate, error) {
	chunkIDs := make([]domain.ChunkID, 0, len(items))
	for _, item := range items {
		if item.Surface == domain.SurfaceChunk {
			chunkIDs = append(chunkIDs, domain.ChunkID(item.RecordID))
		}
	}
	chunks, err := h.store.ChunkProvenance(ctx, h.req.Scope.WorkspaceID, chunkIDs)
	if err != nil {
		return nil, err
	}

	var out []candidate
	seen := make(map[string]bool, len(items))

	for _, item := range items {
		switch item.Surface {
		case domain.SurfaceChunk:
			provenance, ok := chunks[domain.ChunkID(item.RecordID)]
			if ok && !h.req.Policy.Allows(provenance.Chunk.Classification,
				provenance.SourceID, "", "", "") {
				// Retrieval already narrowed by policy; this is the second gate on the
				// canonical record. A citation that reproduces a passage is a disclosure
				// path of its own, and it reads the ledger rather than the projection.
				h.drop(item, domain.SectionExcerpts, domain.DropSectionExcluded,
					"policy does not permit this source material")
				continue
			}
			if !ok {
				// The projection outlived the chunk. Retrieval can race a rebuild; a
				// citation that cannot be resolved is not rendered.
				h.drop(item, domain.SectionExcerpts, domain.DropNoEvidence,
					"the chunk behind this projection no longer exists")
				continue
			}
			out = append(out, h.fromChunk(item, provenance))

		case domain.SurfaceAssertion:
			id := domain.AssertionID(item.RecordID)
			if seen[string(id)] {
				continue
			}
			seen[string(id)] = true
			built, err := h.fromAssertion(ctx, item, id, item.Score)
			if err != nil {
				return nil, err
			}
			if built != nil {
				out = append(out, *built)
			}

		case domain.SurfaceEntity:
			expanded, err := h.fromEntity(ctx, item, seen)
			if err != nil {
				return nil, err
			}
			out = append(out, expanded...)

		default:
			h.drop(item, domain.SectionExcerpts, domain.DropNoEvidence,
				"surface "+string(item.Surface)+" has no citable form")
		}
	}
	return out, nil
}

func (h *hydrator) drop(item domain.RetrievedItem, section domain.ContextSection, reason domain.DropReason, detail string) {
	h.dropped = append(h.dropped, domain.DroppedItem{
		Surface: item.Surface, RecordID: item.RecordID, Section: section,
		Reason: reason, Detail: detail, Relevance: item.Score,
	})
}

func (h *hydrator) fromChunk(item domain.RetrievedItem, p domain.ChunkProvenance) candidate {
	text := strings.TrimSpace(p.Chunk.Content)
	chunkID := p.Chunk.ID
	episodeID := p.Chunk.EpisodeID

	return candidate{
		section:  domain.SectionExcerpts,
		text:     text,
		surface:  domain.SurfaceChunk,
		recordID: item.RecordID,

		relevance: item.Score,
		// A source excerpt carries no confidence of its own: it is what the document
		// says, quoted. Trust in it is trust in the source, which is recorded there.
		confidence: trustWeight(p.TrustLevel),
		evidence:   1,
		temporal:   h.temporalFit(p.EventTime, nil),
		priority:   1,

		citation: domain.Citation{
			Surface:       domain.SurfaceChunk,
			ChunkID:       &chunkID,
			EpisodeID:     &episodeID,
			SourceEventID: p.SourceEventID,
			SourceID:      p.SourceID,
			SourceName:    p.SourceName,
			Quote:         text,
			Locator:       locatorText(p.Chunk.Locator),
		},
		terms: terms(text),
	}
}

// fromAssertion resolves a claim to its provenance and decides which section it belongs in.
func (h *hydrator) fromAssertion(ctx context.Context, item domain.RetrievedItem, id domain.AssertionID, relevance float64) (*candidate, error) {
	chain, err := h.store.ProvenanceChain(ctx, h.req.Scope.WorkspaceID, id)
	if err != nil {
		if domain.CodeOf(err) == domain.CodeNotFound {
			h.drop(item, domain.SectionFacts, domain.DropNoEvidence,
				"the assertion behind this projection no longer exists")
			return nil, nil
		}
		return nil, err
	}

	assertion := chain.Assertion
	if !h.req.Policy.Allows(assertion.Classification, sourceOfChain(chain),
		assertion.Predicate.Name, assertion.MemoryKind, "") {
		h.drop(item, domain.SectionFacts, domain.DropSectionExcluded,
			"policy does not permit this claim")
		return nil, nil
	}
	if len(chain.Links) == 0 && !reasoned(assertion.ProvenanceMode) {
		// Phase 8's acceptance criterion in one branch: an observed claim with no
		// evidence cannot be cited, so it is not rendered at all rather than rendered
		// unsupported.
		h.drop(item, domain.SectionFacts, domain.DropNoEvidence,
			"the claim has no evidence to cite")
		return nil, nil
	}

	objectName := assertion.Object.Display()
	if assertion.Object.Kind == domain.ObjectEntity {
		object, err := h.entity(ctx, assertion.Object.EntityID)
		if err != nil {
			return nil, err
		}
		objectName = object.CanonicalName
	}

	built := candidate{
		text:     renderClaim(chain.Subject.CanonicalName, assertion, objectName),
		surface:  domain.SurfaceAssertion,
		recordID: string(id),

		relevance:  relevance,
		confidence: assertion.Confidence,
		evidence:   evidenceQuality(chain.Links),
		temporal:   h.temporalFit(assertion.Temporal.ValidFrom, assertion.Temporal.ValidTo),
		priority:   memoryPriority(assertion.MemoryKind),

		subjects:  []domain.EntityID{assertion.SubjectID},
		predicate: assertion.Predicate.Name,
		slot:      string(assertion.SubjectID) + "|" + assertion.Predicate.Name + "|" + assertion.ScopeKey,
		objectKey: assertion.Object.Key(),
		citation:  citationFor(assertion, chain),
	}
	if assertion.Object.Kind == domain.ObjectEntity {
		built.subjects = append(built.subjects, assertion.Object.EntityID)
	}
	built.terms = terms(built.text)
	built.section = h.sectionFor(assertion, item)

	if assertion.ConflictSetID != nil {
		note, err := h.conflictNote(ctx, assertion)
		if err != nil {
			return nil, err
		}
		built.conflict = note
	}
	return &built, nil
}

// fromEntity turns a matched identity into the claims that make it worth mentioning.
//
// An entity hit says the asker named a subject; it does not say which fact about that
// subject they wanted. Rendering the identity alone would put a line in the prompt that no
// assertion supports, which is exactly what the acceptance criterion forbids.
func (h *hydrator) fromEntity(ctx context.Context, item domain.RetrievedItem, seen map[string]bool) ([]candidate, error) {
	entityID := domain.EntityID(item.RecordID)

	claims, err := h.store.QueryAssertions(ctx, domain.AssertionQuery{
		Scope:         h.req.Scope,
		SubjectIDs:    []domain.EntityID{entityID},
		Statuses:      []domain.AssertionStatus{domain.AssertionActive},
		MinConfidence: h.req.Filters.MinConfidence,
		ValidAt:       h.req.Temporal.ValidAt,
		KnownAt:       h.req.Temporal.KnownAt,
		ActiveAt:      h.req.Temporal.ActiveAt,
		Limit:         EntityExpansionLimit,
	})
	if err != nil {
		return nil, err
	}
	if len(claims) == 0 {
		h.drop(item, domain.SectionFacts, domain.DropNoEvidence,
			"the entity matched but holds no claims that pass the filters")
		return nil, nil
	}

	out := make([]candidate, 0, len(claims))
	for i, claim := range claims {
		if seen[string(claim.ID)] {
			continue
		}
		seen[string(claim.ID)] = true

		// Discounted by position: the identity matched, the specific claim did not, and
		// the further down the list the weaker the reason to believe it was wanted.
		relevance := item.Score * (1 - 0.1*float64(i))
		built, err := h.fromAssertion(ctx, item, claim.ID, relevance)
		if err != nil {
			return nil, err
		}
		if built != nil {
			out = append(out, *built)
		}
	}
	return out, nil
}

// sectionFor decides where a claim belongs.
func (h *hydrator) sectionFor(a domain.Assertion, item domain.RetrievedItem) domain.ContextSection {
	if a.ConflictSetID != nil || a.Status == domain.AssertionDisputed {
		return domain.SectionConflicts
	}
	if a.Status == domain.AssertionSuperseded || a.Status == domain.AssertionRetracted {
		return domain.SectionHistory
	}
	if a.Temporal.ValidTo != nil && !a.Temporal.ValidTo.After(h.asOf()) {
		// Valid once, not now. Historical rather than current, and mixing the two is how
		// a prompt ends up stating last quarter's price as today's.
		return domain.SectionHistory
	}
	if item.Path != nil {
		return domain.SectionGraph
	}
	return domain.SectionFacts
}

// asOf is the instant the block describes: the requested world time, else now.
func (h *hydrator) asOf() time.Time {
	if h.req.Temporal.ValidAt != nil {
		return *h.req.Temporal.ValidAt
	}
	return h.now
}

// temporalFit scores how well a validity interval covers the instant in question.
func (h *hydrator) temporalFit(from, to *time.Time) float64 {
	at := h.asOf()
	switch {
	case from != nil && from.After(at):
		return 0.3 // not yet in force
	case to != nil && !to.After(at):
		return 0.4 // ended
	case from == nil && to == nil:
		return 0.8 // undated: applicable but unanchored
	default:
		return 1
	}
}

func (h *hydrator) entity(ctx context.Context, id domain.EntityID) (domain.Entity, error) {
	if entity, ok := h.entities[id]; ok {
		return entity, nil
	}
	entity, err := h.store.GetEntity(ctx, h.req.Scope.WorkspaceID, id)
	if err != nil {
		return domain.Entity{}, err
	}
	if h.entities == nil {
		h.entities = map[domain.EntityID]domain.Entity{}
	}
	h.entities[id] = entity
	return entity, nil
}

// conflictNote renders the other side of a recorded contradiction.
func (h *hydrator) conflictNote(ctx context.Context, a domain.Assertion) (*domain.ConflictNote, error) {
	set, members, err := h.store.ConflictMembers(ctx, h.req.Scope.WorkspaceID, *a.ConflictSetID)
	if err != nil {
		if domain.CodeOf(err) == domain.CodeNotFound {
			return nil, nil
		}
		return nil, err
	}

	note := &domain.ConflictNote{ConflictSetID: set.ID, Reason: set.Reason}
	for _, member := range members {
		if member.ID == a.ID {
			continue
		}
		objectName := member.Object.Display()
		if member.Object.Kind == domain.ObjectEntity {
			object, err := h.entity(ctx, member.Object.EntityID)
			if err != nil {
				return nil, err
			}
			objectName = object.CanonicalName
		}
		note.Others = append(note.Others, objectName)
	}
	return note, nil
}

// citationFor builds the reference a claim is rendered with.
func citationFor(a domain.Assertion, chain domain.ProvenanceChain) domain.Citation {
	citation := domain.Citation{
		Surface:       domain.SurfaceAssertion,
		AssertionID:   &a.ID,
		SourceEventID: a.SourceEventID,
		ValidFrom:     a.Temporal.ValidFrom,
		ValidTo:       a.Temporal.ValidTo,
		KnownAt:       &a.Temporal.RecordedAt,
		Confidence:    a.Confidence,
		Status:        a.Status,
	}

	for _, link := range chain.Links {
		citation.EvidenceIDs = append(citation.EvidenceIDs, link.Evidence.ID)
		if citation.Quote == "" && link.Evidence.ExtractedText != "" {
			citation.Quote = link.Evidence.ExtractedText
			episodeID := link.Episode.ID
			citation.EpisodeID = &episodeID
			citation.ChunkID = link.Evidence.ChunkID
			citation.SourceID = link.Source.ID
			citation.SourceName = link.Source.Name
			citation.Locator = locatorText(link.Episode.Locator)
		}
	}
	if citation.SourceName == "" && len(chain.Links) > 0 {
		citation.SourceID = chain.Links[0].Source.ID
		citation.SourceName = chain.Links[0].Source.Name
	}
	return citation
}

// sourceOfChain reports which source a claim came from, when its provenance says.
func sourceOfChain(chain domain.ProvenanceChain) domain.SourceID {
	if len(chain.Links) == 0 {
		return ""
	}
	return chain.Links[0].Source.ID
}

// reasoned reports whether a claim was concluded rather than read.
//
// A reasoned claim cites its derivation and the assertions it was built from, not evidence
// of its own, so requiring evidence of it would drop exactly the inferences the graph exists
// to make.
func reasoned(mode domain.ProvenanceMode) bool {
	return mode == domain.ProvenanceInferred || mode == domain.ProvenanceDerived
}

// locatorText renders where a unit sits in its source, for a citation line.
func locatorText(l domain.Locator) string {
	var parts []string
	if l.Page != nil {
		parts = append(parts, "p."+itoa(*l.Page))
	}
	if l.Section != "" {
		parts = append(parts, l.Section)
	}
	if len(l.HeadingPath) > 0 {
		parts = append(parts, strings.Join(l.HeadingPath, " > "))
	}
	if l.MessageIndex != nil {
		label := "message " + itoa(*l.MessageIndex)
		if l.Role != "" {
			label = l.Role + " " + label
		}
		parts = append(parts, label)
	}
	if l.JSONPointer != "" {
		parts = append(parts, l.JSONPointer)
	}
	if l.RowKey != "" {
		parts = append(parts, "row "+l.RowKey)
	}
	if l.CodePath != "" {
		location := l.CodePath
		if l.LineStart != nil {
			location += ":" + itoa(*l.LineStart)
		}
		parts = append(parts, location)
	}
	return strings.Join(parts, ", ")
}

func itoa(i int) string { return strconv.Itoa(i) }

// evidenceQuality scores citation strength in [0,1].
//
// A quote is worth more than a pointer: it is what makes a claim checkable without leaving
// the prompt. Independent sources are worth more than repeated ones, since the same document
// cited twice is one piece of evidence.
func evidenceQuality(links []domain.ProvenanceLink) float64 {
	if len(links) == 0 {
		return 0.5 // reasoned claims are cited by derivation instead
	}

	quoted := 0
	sources := map[domain.SourceID]struct{}{}
	for _, link := range links {
		if strings.TrimSpace(link.Evidence.ExtractedText) != "" {
			quoted++
		}
		sources[link.Source.ID] = struct{}{}
	}

	score := 0.6
	if quoted > 0 {
		score += 0.25
	}
	if len(sources) > 1 {
		score += 0.15
	}
	if score > 1 {
		score = 1
	}
	return score
}

// memoryPriority weights kinds of memory against each other (AGENTS.md section 20.2).
func memoryPriority(kind domain.MemoryKind) float64 {
	switch kind {
	case domain.MemorySemantic:
		return 1
	case domain.MemoryProcedural:
		return 0.95
	case domain.MemoryEpisodic:
		return 0.85
	case domain.MemoryPreference:
		return 0.9
	default:
		return 0.8
	}
}

// trustWeight maps a source's registered trust onto the same [0,1] scale as confidence.
func trustWeight(level domain.TrustLevel) float64 {
	switch level {
	case domain.TrustAuthoritative:
		return 1
	case domain.TrustStandard:
		return 0.8
	case domain.TrustLow:
		return 0.5
	default:
		return 0.6
	}
}

// renderClaim writes a claim as a readable sentence, qualified by the time it holds.
//
// The projection renders claims too, lowercased and unqualified, because it renders them to
// be matched. This renders them to be read: a prompt line that omits "until March" invites
// the model to state a lapsed fact as current.
func renderClaim(subject string, a domain.Assertion, object string) string {
	// Lowercased: predicate names are identifiers (SUPPLIES_TO, role_at) and reading one
	// mid-sentence in its stored casing looks like emphasis that was never meant.
	predicate := strings.ToLower(strings.ReplaceAll(a.Predicate.Name, "_", " "))

	var b strings.Builder
	b.WriteString(strings.TrimSpace(subject))
	b.WriteString(" ")
	b.WriteString(predicate)
	b.WriteString(" ")
	b.WriteString(strings.TrimSpace(object))
	if a.ScopeKey != "" {
		b.WriteString(" (")
		b.WriteString(a.ScopeKey)
		b.WriteString(")")
	}
	if qualifier := validityQualifier(a.Temporal); qualifier != "" {
		b.WriteString(" ")
		b.WriteString(qualifier)
	}
	return b.String()
}

func validityQualifier(t domain.TemporalCoordinates) string {
	const day = "2006-01-02"
	switch {
	case t.ValidFrom != nil && t.ValidTo != nil:
		return "[" + t.ValidFrom.UTC().Format(day) + " to " + t.ValidTo.UTC().Format(day) + "]"
	case t.ValidFrom != nil:
		return "[since " + t.ValidFrom.UTC().Format(day) + "]"
	case t.ValidTo != nil:
		return "[until " + t.ValidTo.UTC().Format(day) + "]"
	default:
		return ""
	}
}

// terms reduces text to a set of normalized words for redundancy comparison.
func terms(text string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, field := range strings.Fields(strings.ToLower(text)) {
		word := strings.Trim(field, ".,;:!?\"'()[]{}")
		if len(word) < 3 {
			continue // articles and prepositions make everything look similar
		}
		out[word] = struct{}{}
	}
	return out
}

// redundancy scores how much one candidate repeats another, in [0,1].
//
// Lexical overlap is the general case, with one structural exception: two claims about the
// same subject and predicate that assert different values are contrastive, not repetitive.
// "tier LEGACY until March" and "tier PREMIUM since March" overlap on four of eight words
// and are exactly the pair a reader must see together. Judging them by wording alone would
// silently drop the correction and keep the stale value, or the reverse.
func redundancy(a, b candidate) float64 {
	if a.slot != "" && a.slot == b.slot && a.objectKey != b.objectKey {
		return 0
	}
	return similarity(a.terms, b.terms)
}

// similarity is Jaccard overlap between two term sets, in [0,1].
//
// Lexical rather than semantic on purpose: redundancy reduction must work with no embedder
// configured, and near-duplicate passages — the case this exists for — are lexically near
// duplicates. Two differently worded statements of the same fact are not caught, which is a
// known limit rather than an oversight.
func similarity(a, b map[string]struct{}) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	shared := 0
	smaller, larger := a, b
	if len(b) < len(a) {
		smaller, larger = b, a
	}
	for word := range smaller {
		if _, ok := larger[word]; ok {
			shared++
		}
	}
	union := len(a) + len(b) - shared
	if union == 0 {
		return 0
	}
	return float64(shared) / float64(union)
}
