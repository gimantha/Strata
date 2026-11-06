package contextblock

import (
	"context"
	"slices"
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
	// focus measures exact agreement with strong identifiers recovered from the question.
	// It prevents a semantically similar record about another identity from buying context.
	focus float64
	// required marks a premise or historical counterpart brought in to make the requested
	// answer complete. It may not repeat the focal identifier in its rendered sentence.
	required   bool
	related    bool
	assertion  *domain.Assertion
	supportIDs []domain.AssertionID
	derivation *domain.Derivation
	// excerpt is the source passage a claim's evidence was read from, kept so a claim
	// brought in as a required premise can bring its checkable text with it.
	excerpt *domain.ProvenanceLink
	// message marks an excerpt that is one message of a conversation. Its neighbours in
	// the conversation are offered too (see addNeighbors).
	message bool
	// anchor is set on a neighbour: the record ID of the retrieved message it sits next
	// to, and offset says on which side (-1 before, +1 after).
	anchor string
	offset int
	// when is the source time of an excerpt: when its message or document was written.
	// It is rendered next to the excerpt, because a question about when something
	// happened is answered from it, and "last Saturday" in the text means nothing without it.
	when *time.Time

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
	ChunkNeighbors(ctx context.Context, ws domain.WorkspaceID, ids []domain.ChunkID, classifications []domain.Classification) (map[domain.ChunkID]domain.ChunkNeighbors, error)
	ConflictMembers(ctx context.Context, ws domain.WorkspaceID, id domain.ConflictSetID) (domain.ConflictSet, []domain.Assertion, error)
	QueryAssertions(ctx context.Context, q domain.AssertionQuery) ([]domain.Assertion, error)
	GetEntity(ctx context.Context, ws domain.WorkspaceID, id domain.EntityID) (domain.Entity, error)
}

// hydrator turns retrieval hits into citable candidates.
type hydrator struct {
	store Store
	req   domain.ContextRequest
	now   time.Time

	entities     map[domain.EntityID]domain.Entity
	dropped      []domain.DroppedItem
	focusTerms   []string
	requirements []string
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
	out, err = h.addNeighbors(ctx, out)
	if err != nil {
		return nil, err
	}
	expanded, err := h.expandRequirements(ctx, out, seen)
	if err != nil {
		return nil, err
	}
	return h.deriveProofs(ctx, expanded)
}

// Neighbour shares: a neighbouring message is offered at this fraction of the relevance
// of the message it sits next to.
//
// In a conversation the words of a question often match the turn that asked about the
// thing, and the answer is in the reply. On the LoCoMo dev questions (bench/), offering the
// neighbours raised evidence recall at a 4,000-token budget from 76% to 80%; the next
// message helps far more than the previous one, so it gets the larger share. Shares of
// 0.7/0.4, 0.9/0.6 and 1.0/0.8 all landed within a point of each other there; the lowest
// is kept, so a neighbour is least likely to push out a real match on other data. Both
// compete for the budget like any other excerpt.
const (
	nextMessageShare     = 0.7
	previousMessageShare = 0.4
)

// addNeighbors offers the messages just before and after each retrieved conversation
// message.
//
// A neighbour comes from the same source event as the message it sits next to, so it shares
// that message's source, collection and graph space, which the caller was already allowed
// to see. Its classification can differ, so the store filters on it in the query, and the
// policy is checked again here on the record itself, as for any retrieved passage.
func (h *hydrator) addNeighbors(ctx context.Context, candidates []candidate) ([]candidate, error) {
	present := map[string]bool{}
	var anchors []domain.ChunkID
	for _, c := range candidates {
		if c.surface != domain.SurfaceChunk {
			continue
		}
		present[c.recordID] = true
		if c.message && c.anchor == "" {
			anchors = append(anchors, domain.ChunkID(c.recordID))
		}
	}
	if len(anchors) == 0 {
		return candidates, nil
	}

	allowed := h.req.Policy.PermittedClassifications()
	if len(h.req.Filters.Classifications) > 0 {
		var narrowed []domain.Classification
		for _, c := range allowed {
			if slices.Contains(h.req.Filters.Classifications, c) {
				narrowed = append(narrowed, c)
			}
		}
		allowed = narrowed
	}
	neighbors, err := h.store.ChunkNeighbors(ctx, h.req.Scope.WorkspaceID, anchors, allowed)
	if err != nil {
		return nil, err
	}

	type offer struct {
		id     domain.ChunkID
		anchor candidate
		offset int
		share  float64
	}
	var offers []offer
	var ids []domain.ChunkID
	byID := map[string]candidate{}
	for _, c := range candidates {
		if c.surface == domain.SurfaceChunk {
			byID[c.recordID] = c
		}
	}
	for _, anchorID := range anchors {
		n := neighbors[anchorID]
		anchor := byID[string(anchorID)]
		for _, side := range []offer{
			{id: n.Next, anchor: anchor, offset: 1, share: nextMessageShare},
			{id: n.Previous, anchor: anchor, offset: -1, share: previousMessageShare},
		} {
			if side.id == "" || present[string(side.id)] {
				continue
			}
			present[string(side.id)] = true
			offers = append(offers, side)
			ids = append(ids, side.id)
		}
	}
	if len(offers) == 0 {
		return candidates, nil
	}

	chunks, err := h.store.ChunkProvenance(ctx, h.req.Scope.WorkspaceID, ids)
	if err != nil {
		return nil, err
	}
	for _, o := range offers {
		p, ok := chunks[o.id]
		if !ok || !h.req.Policy.Allows(p.Chunk.Classification, p.SourceID, "", "", "") {
			continue
		}
		item := domain.RetrievedItem{
			Surface: domain.SurfaceChunk, RecordID: string(o.id), Score: o.anchor.relevance * o.share,
		}
		c := h.fromChunk(item, p)
		// Related: reached through a retrieved message, so exempt from the focus gate
		// the way a passage reached through a claim is.
		c.related = true
		c.anchor = o.anchor.recordID
		c.offset = o.offset
		candidates = append(candidates, c)
	}
	return candidates, nil
}

const RequirementExpansionLimit = 16

func (h *hydrator) expandRequirements(ctx context.Context, candidates []candidate, seen map[string]bool) ([]candidate, error) {
	// An as-of question needs the bounds of the interval it asks about, whichever words it
	// used: the claim that held at that instant, and the claim that ended it.
	needsHistory := hasRequirement(h.requirements, "history") || hasRequirement(h.requirements, "comparison_inputs") ||
		hasRequirement(h.requirements, "absence_proof") || h.req.Temporal.ValidAt != nil
	needsInputs := hasRequirement(h.requirements, "decision_trace")
	if (!needsHistory && !needsInputs) || expansionPolicyUnsupported(h.req.Policy) {
		return candidates, nil
	}
	base := append([]candidate(nil), candidates...)
	// Expansion follows the question's subject. When any candidate names the identifier
	// the question asked about, candidates that do not are not seeds: their history and
	// premises are about something else, and marking them required would carry them past
	// the focus gate that would otherwise drop them. That is how a question about one
	// record came to state another record's budget as a known fact.
	focused := anyFocused(base)
	added, excerpts := 0, 0
	add := func(id domain.AssertionID, score float64) error {
		if added >= RequirementExpansionLimit || seen[string(id)] {
			return nil
		}
		item := domain.RetrievedItem{Surface: domain.SurfaceAssertion, RecordID: string(id), Score: score}
		built, err := h.fromAssertion(ctx, item, id, score)
		if err != nil {
			return err
		}
		if built == nil {
			return nil
		}
		built.required = true
		seen[string(id)] = true
		candidates = append(candidates, *built)
		added++
		// A premise is only as useful as its checkable text. The claim's own sentence is
		// the graph's paraphrase; the passage it was read from is what a reader can
		// verify and quote, so it comes along, once per passage. It is offered, not
		// required: it competes for the excerpt budget on its own merit, because a
		// passage brought in for a premise must not displace the passage that answers
		// the question.
		if built.excerpt != nil && excerpts < RequiredExcerptLimit {
			key := "chunk:" + string(built.excerpt.Chunk.ID)
			if !seen[key] {
				seen[key] = true
				candidates = append(candidates, h.excerptOf(*built.excerpt, score))
				excerpts++
			}
		}
		return nil
	}
	for _, candidate := range base {
		if candidate.assertion == nil || added >= RequirementExpansionLimit {
			continue
		}
		if focused && candidate.focus == 0 {
			continue
		}
		if needsInputs {
			for _, id := range candidate.supportIDs {
				if err := add(id, candidate.relevance*.95); err != nil {
					return nil, err
				}
			}
		}
		if needsHistory {
			a := candidate.assertion
			claims, err := h.store.QueryAssertions(ctx, domain.AssertionQuery{
				Scope: h.req.Scope, SubjectIDs: []domain.EntityID{a.SubjectID},
				Predicates: []string{a.Predicate.Name}, ScopeKey: a.ScopeKey,
				KnownAt: h.req.Temporal.KnownAt, IncludeSuperseded: true,
				MinConfidence:   h.req.Filters.MinConfidence,
				Classifications: h.req.Policy.PermittedClassifications(),
				SourceIDs:       h.req.Policy.AllowedSources,
				MemoryKinds:     h.req.Policy.AllowedMemoryKinds,
				Limit:           8,
			})
			if err != nil {
				return nil, err
			}
			for _, claim := range claims {
				if err := add(claim.ID, candidate.relevance*.90); err != nil {
					return nil, err
				}
			}
		}
	}
	return candidates, nil
}

// RequiredExcerptLimit bounds how many source passages are offered alongside required
// premises. Each is a whole passage, so the bound protects the budget for the claims and
// for the passages retrieval itself ranked.
const RequiredExcerptLimit = 4

// anyFocused reports whether any candidate names one of the question's identifiers.
func anyFocused(candidates []candidate) bool {
	for _, c := range candidates {
		if c.focus > 0 {
			return true
		}
	}
	return false
}

// focusedOnly returns the candidates that name one of the question's identifiers, or all of
// them when none does.
func focusedOnly(candidates []candidate) []candidate {
	if !anyFocused(candidates) {
		return candidates
	}
	out := make([]candidate, 0, len(candidates))
	for _, c := range candidates {
		if c.focus > 0 || c.required {
			out = append(out, c)
		}
	}
	return out
}

// excerptOf builds a source-excerpt candidate from the passage a claim's evidence came from.
func (h *hydrator) excerptOf(link domain.ProvenanceLink, relevance float64) candidate {
	text := strings.TrimSpace(link.Chunk.Content)
	chunkID := link.Chunk.ID
	episodeID := link.Episode.ID
	recorded := link.SourceEvent.RecordedAt
	return candidate{
		section:    domain.SectionExcerpts,
		text:       text,
		surface:    domain.SurfaceChunk,
		recordID:   string(chunkID),
		relevance:  relevance,
		confidence: trustWeight(link.Source.TrustLevel),
		evidence:   1,
		temporal:   h.temporalFit(link.SourceEvent.EventTime, nil),
		priority:   1,
		focus:      focusScore(text, h.focusTerms),
		when:       firstTime(link.Episode.EventTime, link.SourceEvent.EventTime),
		// Related rather than required: reached through a claim, exempt from the focus
		// gate for that reason, but carrying no requirement bonus of its own.
		related: true,
		citation: domain.Citation{
			Surface:          domain.SurfaceChunk,
			ChunkID:          &chunkID,
			EpisodeID:        &episodeID,
			SourceEventID:    link.SourceEvent.ID,
			SourceExternalID: link.SourceEvent.ExternalID,
			IdempotencyKey:   link.SourceEvent.IdempotencyKey,
			SourceSequence:   link.SourceEvent.SourceSequence,
			SourceVersion:    link.SourceEvent.SourceVersion,
			EventTime:        link.SourceEvent.EventTime,
			RecordedAt:       &recorded,
			SourceID:         link.Source.ID,
			SourceName:       link.Source.Name,
			Quote:            text,
			Locator:          locatorText(link.Episode.Locator),
		},
		terms: terms(text),
	}
}

func expansionPolicyUnsupported(policy domain.PolicyFilters) bool {
	return len(policy.DeniedSources) > 0 || len(policy.AllowedCollections) > 0 ||
		len(policy.DeniedCollections) > 0 || len(policy.DeniedMemoryKinds) > 0
}

func hasRequirement(requirements []string, want string) bool {
	for _, requirement := range requirements {
		if requirement == want {
			return true
		}
	}
	return false
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
		focus:      focusScore(text, h.focusTerms),
		when:       firstTime(p.EventTime, p.SourceEventTime),
		message:    p.Chunk.Locator.MessageIndex != nil || p.EpisodeLocator.MessageIndex != nil,

		citation: domain.Citation{
			Surface:          domain.SurfaceChunk,
			ChunkID:          &chunkID,
			EpisodeID:        &episodeID,
			SourceEventID:    p.SourceEventID,
			SourceExternalID: p.SourceExternalID,
			IdempotencyKey:   p.IdempotencyKey,
			SourceSequence:   p.SourceSequence,
			SourceVersion:    p.SourceVersion,
			EventTime:        p.SourceEventTime,
			RecordedAt:       &p.RecordedAt,
			SourceID:         p.SourceID,
			SourceName:       p.SourceName,
			Quote:            text,
			Locator:          locatorText(p.Chunk.Locator),
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
		focus:      focusScore(renderClaim(chain.Subject.CanonicalName, assertion, objectName), h.focusTerms),
		related:    item.Path != nil,
		assertion:  &assertion,
		derivation: chain.Derivation,

		subjects:  []domain.EntityID{assertion.SubjectID},
		predicate: assertion.Predicate.Name,
		slot:      string(assertion.SubjectID) + "|" + assertion.Predicate.Name + "|" + assertion.ScopeKey,
		objectKey: assertion.Object.Key(),
		citation:  citationFor(assertion, chain),
	}
	for _, support := range chain.Supports {
		built.supportIDs = append(built.supportIDs, support.ID)
	}
	for i := range chain.Links {
		if chain.Links[i].Chunk != nil && strings.TrimSpace(chain.Links[i].Chunk.Content) != "" {
			built.excerpt = &chain.Links[i]
			break
		}
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
	if a.Temporal.ValidFrom != nil && a.Temporal.ValidFrom.After(h.asOf()) {
		// Not yet in force at the instant the block describes. It is shown because it
		// bounds the interval the question asked about, and it is labelled with its start
		// so it cannot be read as the state at that instant.
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
			citation.SourceExternalID = link.SourceEvent.ExternalID
			citation.IdempotencyKey = link.SourceEvent.IdempotencyKey
			citation.SourceSequence = link.SourceEvent.SourceSequence
			citation.SourceVersion = link.SourceEvent.SourceVersion
			citation.EventTime = link.SourceEvent.EventTime
			recorded := link.SourceEvent.RecordedAt
			citation.RecordedAt = &recorded
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
	// A chat message's pointer only repeats its index ("/messages/14" next to
	// "message 14"), so it is left out then.
	if l.JSONPointer != "" && (l.MessageIndex == nil || l.JSONPointer != "/messages/"+itoa(*l.MessageIndex)) {
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

// firstTime returns the first time that is set.
func firstTime(times ...*time.Time) *time.Time {
	for _, t := range times {
		if t != nil {
			return t
		}
	}
	return nil
}

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
		b.WriteString(domain.DisplayContextQualifiers(a.ScopeKey))
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

// focusScore reports how completely an item contains the exact anchors recovered from the
// question. Anchors are deliberately few and precise; broad query vocabulary belongs in
// relevance scoring, not in this gate.
func focusScore(text string, anchors []string) float64 {
	if len(anchors) == 0 {
		return 0
	}
	haystack := strings.ToLower(text)
	matched := 0
	for _, anchor := range anchors {
		if strings.Contains(haystack, strings.ToLower(anchor)) {
			matched++
		}
	}
	return float64(matched) / float64(len(anchors))
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
	// Assertions about different identities are coverage, not repetition, even when a
	// source template makes their sentences nearly identical.
	if len(a.subjects) > 0 && len(b.subjects) > 0 && !sharedSubject(a.subjects, b.subjects) {
		return 0
	}
	return similarity(a.terms, b.terms)
}

func sharedSubject(a, b []domain.EntityID) bool {
	for _, left := range a {
		for _, right := range b {
			if left == right {
				return true
			}
		}
	}
	return false
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
