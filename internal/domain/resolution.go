package domain

import (
	"strings"
	"time"
)

// IdentifierKind distinguishes the two kinds of stable identifier that can settle identity
// outright (AGENTS.md section 12.1, rungs 1 and 2).
type IdentifierKind string

const (
	// IdentifierSource is an upstream system's own primary key for a record. It is the
	// strongest evidence available: the source is telling us what it considers one thing.
	IdentifierSource IdentifierKind = "source_identifier"
	// IdentifierDomain is a configured business key such as an email address, a tax
	// number, or a ticker symbol.
	IdentifierDomain IdentifierKind = "domain_key"
)

var identifierKinds = []IdentifierKind{IdentifierSource, IdentifierDomain}

func ParseIdentifierKind(s string) (IdentifierKind, error) {
	return parseEnum("identifier kind", s, identifierKinds)
}

// EntityIdentifier binds a stable external key to an identity.
type EntityIdentifier struct {
	ID           string
	WorkspaceID  WorkspaceID
	GraphSpaceID GraphSpaceID
	EntityID     EntityID
	Kind         IdentifierKind
	// Namespace scopes the value: the source id for a source identifier, or the key type
	// such as "email" for a domain key.
	Namespace string
	Value     string
	SourceID  *SourceID
	CreatedAt time.Time
}

// NormalizeIdentifierValue makes lookups predictable without being clever about it. Case
// and surrounding space are insignificant in every identifier scheme worth supporting;
// anything more aggressive would start merging things that differ.
func NormalizeIdentifierValue(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func (i EntityIdentifier) Validate() error {
	const op = "domain.EntityIdentifier.Validate"

	if IsZero(i.WorkspaceID) || IsZero(i.GraphSpaceID) {
		return Errorf(CodeInvalidArgument, op, "workspace_id and graph_space_id are required")
	}
	if IsZero(i.EntityID) {
		return Errorf(CodeInvalidArgument, op, "entity_id is required")
	}
	if _, err := ParseIdentifierKind(string(i.Kind)); err != nil {
		return err
	}
	if strings.TrimSpace(i.Namespace) == "" {
		return Errorf(CodeInvalidArgument, op, "namespace is required")
	}
	if strings.TrimSpace(i.Value) == "" {
		return Errorf(CodeInvalidArgument, op, "value is required")
	}
	return nil
}

// ResolutionMethod records which rung of the ladder settled an identity, which is the first
// thing anyone reviewing a questionable merge wants to know.
type ResolutionMethod string

const (
	// MethodSourceIdentifier matched an upstream primary key.
	MethodSourceIdentifier ResolutionMethod = "source_identifier"
	// MethodDomainKey matched a configured business key.
	MethodDomainKey ResolutionMethod = "domain_key"
	// MethodExactAlias matched a known name exactly, after normalization.
	MethodExactAlias ResolutionMethod = "exact_alias"
	// MethodCreated found nothing and made a new identity.
	MethodCreated ResolutionMethod = "created"
	// MethodAmbiguous found several plausible identities and refused to choose. A new
	// identity is created and the candidates are recorded for review: guessing here is
	// how two different people silently become one.
	MethodAmbiguous ResolutionMethod = "ambiguous_kept_separate"
	// MethodHumanMerge and MethodHumanSplit record operator decisions.
	MethodHumanMerge ResolutionMethod = "human_merge"
	MethodHumanSplit ResolutionMethod = "human_split"
)

var resolutionMethods = []ResolutionMethod{
	MethodSourceIdentifier, MethodDomainKey, MethodExactAlias,
	MethodCreated, MethodAmbiguous, MethodHumanMerge, MethodHumanSplit,
}

func ParseResolutionMethod(s string) (ResolutionMethod, error) {
	return parseEnum("resolution method", s, resolutionMethods)
}

// Automatic reports whether the method was reached without a human.
func (m ResolutionMethod) Automatic() bool {
	return m != MethodHumanMerge && m != MethodHumanSplit
}

// AliasMatch is a candidate identity found by name, with how closely it matched.
type AliasMatch struct {
	Entity     Entity
	Alias      string
	Similarity float64
}

// ScoredCandidate is one identity that was considered.
type ScoredCandidate struct {
	EntityID EntityID
	Name     string
	Score    float64
	// Features explain the score: which rung produced the candidate, what similarity it
	// scored, whether types agreed. A score with no explanation is not reviewable.
	Features map[string]any
}

// ResolutionDecision records how one mention was resolved (AGENTS.md section 12.2).
//
// Decisions are kept for every resolution, not only the interesting ones. A merge that
// turns out to be wrong is investigated through this ledger, and a decision that cannot be
// found cannot be reversed.
type ResolutionDecision struct {
	ID           ResolutionDecisionID
	WorkspaceID  WorkspaceID
	GraphSpaceID GraphSpaceID

	MentionText string
	MentionType string

	Method           ResolutionMethod
	ChosenEntityID   EntityID
	PreviousEntityID EntityID
	Confidence       float64
	ResolverVersion  int

	Candidates []ScoredCandidate
	Features   map[string]any

	HumanOverride bool
	ActorID       PrincipalID
	Reason        string
	SourceEventID SourceEventID

	// RevertedAt marks a decision that was undone, which is how a mistaken merge is
	// reversed without erasing the fact that it happened.
	RevertedAt *time.Time
	CreatedAt  time.Time
}

func (d ResolutionDecision) Validate() error {
	const op = "domain.ResolutionDecision.Validate"

	if IsZero(d.WorkspaceID) || IsZero(d.GraphSpaceID) {
		return Errorf(CodeInvalidArgument, op, "workspace_id and graph_space_id are required")
	}
	if _, err := ParseResolutionMethod(string(d.Method)); err != nil {
		return err
	}
	if d.Confidence < 0 || d.Confidence > 1 {
		return Errorf(CodeInvalidArgument, op, "confidence must be between 0 and 1")
	}
	if d.ResolverVersion < 1 {
		return Errorf(CodeInvalidArgument, op, "resolver_version is required")
	}
	return nil
}

// DomainKey is a configured business key carried by a mention.
type DomainKey struct {
	Namespace string
	Value     string
}

// Mention is an occurrence of an identity in source material, together with whatever
// evidence about identity came with it.
//
// The fields are ordered by how much they settle: an upstream identifier is far stronger
// evidence than a name, and the resolver uses them in that order rather than blending them
// into a single score (AGENTS.md section 12.1).
type Mention struct {
	Name       string
	EntityType string
	Aliases    []string

	// SourceID and ExternalID together are the upstream system's own identity for this
	// record.
	SourceID   *SourceID
	ExternalID string

	DomainKeys []DomainKey

	SourceEventID SourceEventID
}

func (m Mention) Validate() error {
	const op = "domain.Mention.Validate"

	if strings.TrimSpace(m.Name) == "" && m.ExternalID == "" && len(m.DomainKeys) == 0 {
		return Errorf(CodeInvalidArgument, op,
			"a mention needs a name, an external id, or a domain key")
	}
	if len(m.Name) > MaxCandidateNameLength {
		return Errorf(CodeInvalidArgument, op,
			"name is longer than %d characters", MaxCandidateNameLength)
	}
	if m.ExternalID != "" && m.SourceID == nil {
		return Errorf(CodeInvalidArgument, op,
			"an external id is only meaningful together with the source it came from")
	}
	for _, key := range m.DomainKeys {
		if strings.TrimSpace(key.Namespace) == "" || strings.TrimSpace(key.Value) == "" {
			return Errorf(CodeInvalidArgument, op, "a domain key needs a namespace and a value")
		}
	}
	return nil
}
