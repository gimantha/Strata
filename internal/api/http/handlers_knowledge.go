package http

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/gimantha/strata/internal/domain"
	"github.com/gimantha/strata/internal/knowledge"
)

// entityRefRequest names a subject or object, by identifier or by name.
type entityRefRequest struct {
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
	Type string `json:"type,omitempty"`
}

func (r entityRefRequest) toRef() knowledge.EntityRef {
	return knowledge.EntityRef{ID: domain.EntityID(r.ID), Name: r.Name, Type: r.Type}
}

// objectRequest is the typed object of a claim. Exactly one form is used, chosen by kind,
// so a caller cannot accidentally submit 42 as a string (AGENTS.md section 6.9).
type objectRequest struct {
	Kind      string          `json:"kind"`
	EntityID  string          `json:"entity_id,omitempty"`
	Text      string          `json:"text,omitempty"`
	Integer   *int64          `json:"integer,omitempty"`
	Decimal   string          `json:"decimal,omitempty"`
	Boolean   *bool           `json:"boolean,omitempty"`
	Timestamp *time.Time      `json:"timestamp,omitempty"`
	Date      string          `json:"date,omitempty"`
	Duration  string          `json:"duration,omitempty"`
	Latitude  *float64        `json:"latitude,omitempty"`
	Longitude *float64        `json:"longitude,omitempty"`
	JSON      json.RawMessage `json:"json,omitempty"`
}

func (o objectRequest) toObject() (domain.AssertionObject, error) {
	const op = "http.objectRequest"

	kind, err := domain.ParseObjectKind(o.Kind)
	if err != nil {
		return domain.AssertionObject{}, err
	}

	switch kind {
	case domain.ObjectEntity:
		return domain.ObjectOfEntity(domain.EntityID(o.EntityID)), nil
	case domain.ObjectString:
		return domain.ObjectOfString(o.Text), nil
	case domain.ObjectURI:
		return domain.ObjectOfURI(o.Text), nil
	case domain.ObjectSymbol:
		return domain.ObjectOfSymbol(o.Text), nil
	case domain.ObjectInteger:
		if o.Integer == nil {
			return domain.AssertionObject{}, domain.Errorf(domain.CodeInvalidArgument, op,
				"integer object requires an integer value")
		}
		return domain.ObjectOfInteger(*o.Integer), nil
	case domain.ObjectDecimal:
		return domain.ObjectOfDecimal(o.Decimal), nil
	case domain.ObjectBoolean:
		if o.Boolean == nil {
			return domain.AssertionObject{}, domain.Errorf(domain.CodeInvalidArgument, op,
				"boolean object requires a boolean value")
		}
		return domain.ObjectOfBool(*o.Boolean), nil
	case domain.ObjectTimestamp:
		if o.Timestamp == nil {
			return domain.AssertionObject{}, domain.Errorf(domain.CodeInvalidArgument, op,
				"timestamp object requires a timestamp value")
		}
		return domain.ObjectOfTimestamp(*o.Timestamp), nil
	case domain.ObjectDate:
		parsed, err := time.Parse(domain.DateLayout, o.Date)
		if err != nil {
			return domain.AssertionObject{}, domain.Errorf(domain.CodeInvalidArgument, op,
				"date object requires a %s date", domain.DateLayout)
		}
		return domain.ObjectOfDate(parsed), nil
	case domain.ObjectDuration:
		parsed, err := time.ParseDuration(o.Duration)
		if err != nil {
			return domain.AssertionObject{}, domain.Errorf(domain.CodeInvalidArgument, op,
				"duration object requires a duration such as 4h")
		}
		return domain.ObjectOfDuration(parsed), nil
	case domain.ObjectGeo:
		if o.Latitude == nil || o.Longitude == nil {
			return domain.AssertionObject{}, domain.Errorf(domain.CodeInvalidArgument, op,
				"geo object requires latitude and longitude")
		}
		return domain.ObjectOfGeo(*o.Latitude, *o.Longitude), nil
	case domain.ObjectJSON:
		return domain.ObjectOfJSON(o.JSON), nil
	default:
		return domain.AssertionObject{}, domain.Errorf(domain.CodeInvalidArgument, op,
			"unsupported object kind %q", o.Kind)
	}
}

type evidenceRequest struct {
	EpisodeID     string  `json:"episode_id"`
	ChunkID       string  `json:"chunk_id,omitempty"`
	QuoteStart    *int    `json:"quote_start,omitempty"`
	QuoteEnd      *int    `json:"quote_end,omitempty"`
	ExtractedText string  `json:"extracted_text,omitempty"`
	Confidence    float64 `json:"confidence,omitempty"`
}

type claimRequest struct {
	Subject      entityRefRequest  `json:"subject"`
	Predicate    string            `json:"predicate"`
	Object       *objectRequest    `json:"object,omitempty"`
	ObjectEntity *entityRefRequest `json:"object_entity,omitempty"`

	MemoryKind string `json:"memory_kind,omitempty"`
	ScopeKey   string `json:"scope_key,omitempty"`

	EventTime     *time.Time `json:"event_time,omitempty"`
	ValidFrom     *time.Time `json:"valid_from,omitempty"`
	ValidTo       *time.Time `json:"valid_to,omitempty"`
	EffectiveFrom *time.Time `json:"effective_from,omitempty"`
	EffectiveTo   *time.Time `json:"effective_to,omitempty"`
	ActiveFrom    *time.Time `json:"active_from,omitempty"`
	ActiveUntil   *time.Time `json:"active_until,omitempty"`
	DecayStartsAt *time.Time `json:"decay_starts_at,omitempty"`
	ExpiresAt     *time.Time `json:"expires_at,omitempty"`

	Confidence     float64  `json:"confidence,omitempty"`
	ProvenanceMode string   `json:"provenance_mode,omitempty"`
	Classification string   `json:"classification,omitempty"`
	Supersedes     []string `json:"supersedes,omitempty"`

	Evidence []evidenceRequest `json:"evidence,omitempty"`
}

type derivationRequest struct {
	Method            string         `json:"method"`
	RuleName          string         `json:"rule_name,omitempty"`
	RuleVersion       string         `json:"rule_version,omitempty"`
	InputAssertionIDs []string       `json:"input_assertion_ids,omitempty"`
	Parameters        map[string]any `json:"parameters,omitempty"`
}

type assertRequest struct {
	SourceEventID string             `json:"source_event_id"`
	Claims        []claimRequest     `json:"claims"`
	Derivation    *derivationRequest `json:"derivation,omitempty"`
}

// handleAssert commits claims against a source event.
//
// A claim must name the ingestion it came from: knowledge with no traceable origin is
// exactly what this architecture refuses to hold (AGENTS.md section 2.2).
func (s *Server) handleAssert(w http.ResponseWriter, r *http.Request) {
	scope, ok := s.authorizedGraphSpace(w, r, domain.RoleWriter)
	if !ok {
		return
	}

	var req assertRequest
	if err := decodeJSON(r, s.cfg.MaxBodyBytes, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	if len(req.Claims) == 0 {
		s.writeError(w, r, domain.Errorf(domain.CodeInvalidArgument, "http.handleAssert",
			"at least one claim is required"))
		return
	}

	claims := make([]knowledge.Claim, 0, len(req.Claims))
	for _, item := range req.Claims {
		claim, err := toClaim(item)
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		claims = append(claims, claim)
	}

	assertReq := knowledge.AssertRequest{
		Scope:         scope,
		Principal:     principalFrom(r.Context()).Ref(),
		SourceEventID: domain.SourceEventID(req.SourceEventID),
		Claims:        claims,
	}
	if req.Derivation != nil {
		inputs := make([]domain.AssertionID, 0, len(req.Derivation.InputAssertionIDs))
		for _, id := range req.Derivation.InputAssertionIDs {
			inputs = append(inputs, domain.AssertionID(id))
		}
		assertReq.Derivation = &knowledge.DerivationInput{
			Method:            req.Derivation.Method,
			RuleName:          req.Derivation.RuleName,
			RuleVersion:       req.Derivation.RuleVersion,
			InputAssertionIDs: inputs,
			Parameters:        req.Derivation.Parameters,
		}
	}

	result, err := s.knowledge.Assert(r.Context(), assertReq)
	if err != nil {
		s.writeError(w, r, err)
		return
	}

	assertions := make([]assertionResponse, 0, len(result.Assertions))
	for _, a := range result.Assertions {
		assertions = append(assertions, toAssertionResponse(a))
	}
	conflicts := make([]string, 0, len(result.Conflicts))
	for _, c := range result.Conflicts {
		conflicts = append(conflicts, string(c.ID))
	}
	superseded := make([]string, 0, len(result.Superseded))
	for _, id := range result.Superseded {
		superseded = append(superseded, string(id))
	}

	s.writeJSON(w, r, http.StatusCreated, map[string]any{
		"assertions": assertions,
		"duplicates": result.Duplicates,
		"superseded": superseded,
		"conflicts":  conflicts,
	})
}

func toClaim(item claimRequest) (knowledge.Claim, error) {
	const op = "http.toClaim"

	claim := knowledge.Claim{
		Subject:       item.Subject.toRef(),
		Predicate:     item.Predicate,
		ScopeKey:      item.ScopeKey,
		EventTime:     item.EventTime,
		ValidFrom:     item.ValidFrom,
		ValidTo:       item.ValidTo,
		EffectiveFrom: item.EffectiveFrom,
		EffectiveTo:   item.EffectiveTo,
		ActiveFrom:    item.ActiveFrom,
		ActiveUntil:   item.ActiveUntil,
		DecayStartsAt: item.DecayStartsAt,
		ExpiresAt:     item.ExpiresAt,
		Confidence:    item.Confidence,
	}

	switch {
	case item.ObjectEntity != nil:
		ref := item.ObjectEntity.toRef()
		claim.ObjectEntity = &ref
	case item.Object != nil:
		object, err := item.Object.toObject()
		if err != nil {
			return knowledge.Claim{}, err
		}
		claim.Object = object
	default:
		return knowledge.Claim{}, domain.Errorf(domain.CodeInvalidArgument, op,
			"a claim needs an object or an object_entity")
	}

	if item.MemoryKind != "" {
		kind, err := domain.ParseMemoryKind(item.MemoryKind)
		if err != nil {
			return knowledge.Claim{}, err
		}
		claim.MemoryKind = kind
	}
	if item.ProvenanceMode != "" {
		mode, err := domain.ParseProvenanceMode(item.ProvenanceMode)
		if err != nil {
			return knowledge.Claim{}, err
		}
		claim.ProvenanceMode = mode
	}
	if item.Classification != "" {
		classification, err := domain.ParseClassification(item.Classification)
		if err != nil {
			return knowledge.Claim{}, err
		}
		claim.Classification = classification
	}
	for _, id := range item.Supersedes {
		claim.Supersedes = append(claim.Supersedes, domain.AssertionID(id))
	}
	for _, e := range item.Evidence {
		input := knowledge.EvidenceInput{
			EpisodeID:     domain.EpisodeID(e.EpisodeID),
			QuoteStart:    e.QuoteStart,
			QuoteEnd:      e.QuoteEnd,
			ExtractedText: e.ExtractedText,
			Confidence:    e.Confidence,
		}
		if e.ChunkID != "" {
			chunkID := domain.ChunkID(e.ChunkID)
			input.ChunkID = &chunkID
		}
		claim.Evidence = append(claim.Evidence, input)
	}
	return claim, nil
}

// assertionQueryRequest is the temporal query shape from AGENTS.md section 25.3.
type assertionQueryRequest struct {
	SubjectIDs      []string `json:"subject_ids,omitempty"`
	Predicates      []string `json:"predicates,omitempty"`
	ObjectEntityIDs []string `json:"object_entity_ids,omitempty"`
	ScopeKey        string   `json:"scope_key,omitempty"`
	MemoryKinds     []string `json:"memory_kinds,omitempty"`
	Statuses        []string `json:"statuses,omitempty"`
	MinConfidence   float64  `json:"min_confidence,omitempty"`
	ProvenanceModes []string `json:"provenance_modes,omitempty"`

	SourceIDs     []string `json:"source_ids,omitempty"`
	MinTrustLevel string   `json:"min_trust_level,omitempty"`
	// ChangedSince is a cursor in one source's own ordering. It requires exactly one
	// entry in SourceIDs, since positions from different sources are not comparable.
	ChangedSince *sourcePositionRequest `json:"changed_since,omitempty"`

	ValidAt      *time.Time  `json:"valid_at,omitempty"`
	ValidBetween []time.Time `json:"valid_between,omitempty"`
	KnownAt      *time.Time  `json:"known_at,omitempty"`
	EventBetween []time.Time `json:"event_between,omitempty"`
	ActiveAt     *time.Time  `json:"active_at,omitempty"`

	IncludeSuperseded bool `json:"include_superseded,omitempty"`
	Limit             int  `json:"limit,omitempty"`
	Offset            int  `json:"offset,omitempty"`
}

// sourcePositionRequest is a position in a source's own sequence.
type sourcePositionRequest struct {
	Sequence   string     `json:"sequence,omitempty"`
	Version    string     `json:"version,omitempty"`
	CommitTime *time.Time `json:"commit_time,omitempty"`
	SourceTime *time.Time `json:"source_time,omitempty"`
}

// handleQueryAssertions answers structural and temporal questions about knowledge.
func (s *Server) handleQueryAssertions(w http.ResponseWriter, r *http.Request) {
	scope, ok := s.authorizedGraphSpace(w, r, domain.RoleReader)
	if !ok {
		return
	}

	var req assertionQueryRequest
	if err := decodeJSON(r, s.cfg.MaxBodyBytes, &req); err != nil {
		s.writeError(w, r, err)
		return
	}

	query := domain.AssertionQuery{
		Scope:             scope,
		Predicates:        req.Predicates,
		ScopeKey:          req.ScopeKey,
		MinConfidence:     req.MinConfidence,
		ValidAt:           req.ValidAt,
		KnownAt:           req.KnownAt,
		ActiveAt:          req.ActiveAt,
		IncludeSuperseded: req.IncludeSuperseded,
		Limit:             req.Limit,
		Offset:            req.Offset,
	}
	for _, id := range req.SubjectIDs {
		query.SubjectIDs = append(query.SubjectIDs, domain.EntityID(id))
	}
	for _, id := range req.ObjectEntityIDs {
		query.ObjectEntityIDs = append(query.ObjectEntityIDs, domain.EntityID(id))
	}
	for _, kind := range req.MemoryKinds {
		parsed, err := domain.ParseMemoryKind(kind)
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		query.MemoryKinds = append(query.MemoryKinds, parsed)
	}
	for _, status := range req.Statuses {
		parsed, err := domain.ParseAssertionStatus(status)
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		query.Statuses = append(query.Statuses, parsed)
	}
	for _, mode := range req.ProvenanceModes {
		parsed, err := domain.ParseProvenanceMode(mode)
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		query.ProvenanceModes = append(query.ProvenanceModes, parsed)
	}
	for _, id := range req.SourceIDs {
		query.SourceIDs = append(query.SourceIDs, domain.SourceID(id))
	}
	if req.MinTrustLevel != "" {
		parsed, err := domain.ParseTrustLevel(req.MinTrustLevel)
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		query.MinTrustLevel = parsed
	}
	if req.ChangedSince != nil {
		query.ChangedSince = &domain.SourcePosition{
			Sequence:   req.ChangedSince.Sequence,
			Version:    req.ChangedSince.Version,
			CommitTime: req.ChangedSince.CommitTime,
			SourceTime: req.ChangedSince.SourceTime,
		}
	}

	var err error
	if query.ValidBetween, err = toTimeRange(req.ValidBetween, "valid_between"); err != nil {
		s.writeError(w, r, err)
		return
	}
	if query.EventBetween, err = toTimeRange(req.EventBetween, "event_between"); err != nil {
		s.writeError(w, r, err)
		return
	}

	found, err := s.knowledge.Query(r.Context(), query)
	if err != nil {
		s.writeError(w, r, err)
		return
	}

	out := make([]assertionResponse, 0, len(found))
	for _, a := range found {
		out = append(out, toAssertionResponse(a))
	}
	s.writeJSON(w, r, http.StatusOK, map[string]any{"assertions": out, "count": len(out)})
}

func toTimeRange(values []time.Time, field string) (*domain.TimeRange, error) {
	if len(values) == 0 {
		return nil, nil
	}
	if len(values) != 2 {
		return nil, domain.Errorf(domain.CodeInvalidArgument, "http.toTimeRange",
			"%s must be a pair of timestamps", field)
	}
	return &domain.TimeRange{Start: values[0], End: values[1]}, nil
}

// handleGetAssertion returns one claim.
func (s *Server) handleGetAssertion(w http.ResponseWriter, r *http.Request) {
	ws, id, ok := s.resolveAssertion(w, r)
	if !ok {
		return
	}

	assertion, err := s.knowledge.Get(r.Context(), ws, id)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.writeJSON(w, r, http.StatusOK, toAssertionResponse(assertion))
}

// handleAssertionProvenance walks a claim back to the source material behind it.
func (s *Server) handleAssertionProvenance(w http.ResponseWriter, r *http.Request) {
	ws, id, ok := s.resolveAssertion(w, r)
	if !ok {
		return
	}

	chain, err := s.knowledge.Provenance(r.Context(), ws, id)
	if err != nil {
		s.writeError(w, r, err)
		return
	}

	links := make([]map[string]any, 0, len(chain.Links))
	for _, link := range chain.Links {
		entry := map[string]any{
			"evidence": map[string]any{
				"id":             string(link.Evidence.ID),
				"extracted_text": link.Evidence.ExtractedText,
				"confidence":     link.Evidence.Confidence,
			},
			"episode": map[string]any{
				"id":       string(link.Episode.ID),
				"sequence": link.Episode.Sequence,
				"content":  link.Episode.Content,
				"locator":  link.Episode.Locator,
			},
			"artifact": map[string]any{
				"id":           string(link.Artifact.ID),
				"content_hash": link.Artifact.ContentHash,
				"media_type":   link.Artifact.MediaType,
				"size_bytes":   link.Artifact.SizeBytes,
				"blob_key":     link.Artifact.BlobKey,
			},
			"source_event": map[string]any{
				"id":           string(link.SourceEvent.ID),
				"external_id":  link.SourceEvent.ExternalID,
				"operation":    string(link.SourceEvent.Operation),
				"content_hash": link.SourceEvent.ContentHash,
				"recorded_at":  link.SourceEvent.RecordedAt.UTC().Format(timeFormat),
			},
			"source": map[string]any{
				"id":          string(link.Source.ID),
				"name":        link.Source.Name,
				"kind":        string(link.Source.Kind),
				"trust_level": string(link.Source.TrustLevel),
			},
		}
		if link.Chunk != nil {
			entry["chunk"] = map[string]any{
				"id":         string(link.Chunk.ID),
				"sequence":   link.Chunk.Sequence,
				"content":    link.Chunk.Content,
				"char_start": link.Chunk.CharStart,
				"char_end":   link.Chunk.CharEnd,
			}
		}
		links = append(links, entry)
	}

	body := map[string]any{
		"assertion": toAssertionResponse(chain.Assertion),
		"subject": map[string]any{
			"id":             string(chain.Subject.ID),
			"canonical_name": chain.Subject.CanonicalName,
			"entity_type":    chain.Subject.EntityType,
		},
		"evidence_chain": links,
	}
	if chain.Derivation != nil {
		supports := make([]assertionResponse, 0, len(chain.Supports))
		for _, support := range chain.Supports {
			supports = append(supports, toAssertionResponse(support))
		}
		body["derivation"] = map[string]any{
			"id":           string(chain.Derivation.ID),
			"method":       chain.Derivation.Method,
			"rule_name":    chain.Derivation.RuleName,
			"rule_version": chain.Derivation.RuleVersion,
			"parameters":   chain.Derivation.Parameters,
			"supports":     supports,
		}
	}
	s.writeJSON(w, r, http.StatusOK, body)
}

type retractRequest struct {
	Reason string `json:"reason"`
}

// handleRetractAssertion withdraws a claim. The claim is not deleted: queries as of an
// earlier knowledge time still see it.
func (s *Server) handleRetractAssertion(w http.ResponseWriter, r *http.Request) {
	ws, id, ok := s.resolveAssertion(w, r)
	if !ok {
		return
	}
	principal := principalFrom(r.Context())
	if err := s.identity.AuthorizeWorkspace(r.Context(), principal, ws, domain.RoleWriter); err != nil {
		s.writeError(w, r, err)
		return
	}

	var req retractRequest
	if err := decodeJSON(r, s.cfg.MaxBodyBytes, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	if req.Reason == "" {
		s.writeError(w, r, domain.Errorf(domain.CodeInvalidArgument, "http.handleRetractAssertion",
			"a retraction must say why"))
		return
	}

	retracted, err := s.knowledge.Retract(r.Context(), ws, id, req.Reason, principal.ID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.writeJSON(w, r, http.StatusOK, toAssertionResponse(retracted))
}

// resolveAssertion resolves the {assertion_id} path parameter within the caller's grants.
func (s *Server) resolveAssertion(w http.ResponseWriter, r *http.Request) (domain.WorkspaceID, domain.AssertionID, bool) {
	const op = "http.resolveAssertion"

	raw := r.PathValue("assertion_id")
	if !domain.ValidUUID(domain.AssertionID(raw)) {
		s.writeError(w, r, domain.Errorf(domain.CodeInvalidArgument, op, "malformed assertion id"))
		return "", "", false
	}
	id := domain.AssertionID(raw)

	ws, err := s.ledger.ResolveAssertionWorkspace(r.Context(), id, grantedWorkspaces(principalFrom(r.Context())))
	if err != nil {
		s.writeError(w, r, err)
		return "", "", false
	}
	return ws, id, true
}

// grantedWorkspaces lists the workspaces a principal may read.
func grantedWorkspaces(p domain.Principal) []domain.WorkspaceID {
	out := make([]domain.WorkspaceID, 0, len(p.Grants))
	for _, grant := range p.Grants {
		out = append(out, grant.WorkspaceID)
	}
	return out
}

type assertionResponse struct {
	ID               string                 `json:"id"`
	WorkspaceID      string                 `json:"workspace_id"`
	GraphSpaceID     string                 `json:"graph_space_id"`
	SubjectID        string                 `json:"subject_id"`
	Predicate        string                 `json:"predicate"`
	PredicateVersion int                    `json:"predicate_version"`
	Object           domain.AssertionObject `json:"object"`
	MemoryKind       string                 `json:"memory_kind"`
	ScopeKey         string                 `json:"scope_key,omitempty"`
	Temporal         temporalResponse       `json:"temporal"`
	Confidence       float64                `json:"confidence"`
	Status           string                 `json:"status"`
	SupersedesID     string                 `json:"supersedes_id,omitempty"`
	ConflictSetID    string                 `json:"conflict_set_id,omitempty"`
	ProvenanceMode   string                 `json:"provenance_mode"`
	DerivationID     string                 `json:"derivation_id,omitempty"`
	SourceEventID    string                 `json:"source_event_id"`
	Classification   string                 `json:"classification"`
	RetractedAt      string                 `json:"retracted_at,omitempty"`
	RetractionReason string                 `json:"retraction_reason,omitempty"`
}

// temporalResponse exposes all four clock layers separately, because collapsing them is
// the modeling mistake this system exists to avoid (AGENTS.md section 7.1).
type temporalResponse struct {
	EventTime     string `json:"event_time,omitempty"`
	ValidFrom     string `json:"valid_from,omitempty"`
	ValidTo       string `json:"valid_to,omitempty"`
	EffectiveFrom string `json:"effective_from,omitempty"`
	EffectiveTo   string `json:"effective_to,omitempty"`
	ObservedAt    string `json:"observed_at"`
	RecordedAt    string `json:"recorded_at"`
	SupersededAt  string `json:"superseded_at,omitempty"`
	ActiveFrom    string `json:"active_from,omitempty"`
	ActiveUntil   string `json:"active_until,omitempty"`
	ExpiresAt     string `json:"expires_at,omitempty"`
}

func toAssertionResponse(a domain.Assertion) assertionResponse {
	out := assertionResponse{
		ID:               string(a.ID),
		WorkspaceID:      string(a.WorkspaceID),
		GraphSpaceID:     string(a.GraphSpaceID),
		SubjectID:        string(a.SubjectID),
		Predicate:        a.Predicate.Name,
		PredicateVersion: a.Predicate.Version,
		Object:           a.Object,
		MemoryKind:       string(a.MemoryKind),
		ScopeKey:         a.ScopeKey,
		Confidence:       a.Confidence,
		Status:           string(a.Status),
		ProvenanceMode:   string(a.ProvenanceMode),
		SourceEventID:    string(a.SourceEventID),
		Classification:   string(a.Classification),
		RetractionReason: a.RetractionReason,
		Temporal: temporalResponse{
			EventTime:     formatTimePtr(a.Temporal.EventTime),
			ValidFrom:     formatTimePtr(a.Temporal.ValidFrom),
			ValidTo:       formatTimePtr(a.Temporal.ValidTo),
			EffectiveFrom: formatTimePtr(a.Temporal.EffectiveFrom),
			EffectiveTo:   formatTimePtr(a.Temporal.EffectiveTo),
			ObservedAt:    a.Temporal.ObservedAt.UTC().Format(timeFormat),
			RecordedAt:    a.Temporal.RecordedAt.UTC().Format(timeFormat),
			SupersededAt:  formatTimePtr(a.Temporal.SupersededAt),
			ActiveFrom:    formatTimePtr(a.Temporal.ActiveFrom),
			ActiveUntil:   formatTimePtr(a.Temporal.ActiveUntil),
			ExpiresAt:     formatTimePtr(a.Temporal.ExpiresAt),
		},
	}
	if a.SupersedesID != nil {
		out.SupersedesID = string(*a.SupersedesID)
	}
	if a.ConflictSetID != nil {
		out.ConflictSetID = string(*a.ConflictSetID)
	}
	if a.DerivationID != nil {
		out.DerivationID = string(*a.DerivationID)
	}
	out.RetractedAt = formatTimePtr(a.RetractedAt)
	return out
}

func formatTimePtr(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(timeFormat)
}
