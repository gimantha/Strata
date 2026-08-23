// Package domain contains the pure canonical domain model of the context graph:
// typed identifiers, enumerations, temporal coordinates, and the records that make
// up the authoritative ledger.
//
// This package must not import database, HTTP, LLM, embedding, message-bus,
// telemetry, or any other provider package (AGENTS.md section 5). The guard in
// scripts/check-domain-deps.sh enforces that rule in CI.
package domain

import (
	"crypto/sha256"
	"encoding/hex"

	"github.com/google/uuid"
)

// Identifier types. Typed IDs are used everywhere instead of bare strings so the
// compiler catches scope and reference mistakes (AGENTS.md section 34).
type (
	WorkspaceID   string
	GraphSpaceID  string
	CollectionID  string
	SourceID      string
	SourceEventID string
	ArtifactID    string
	EpisodeID     string
	ChunkID       string
	PrincipalID   string
	PipelineRunID string
	StageRunID    string
	OutboxEventID string

	// Declared now because later phases' contracts already reference them.
	// No storage exists for these until their phase lands.
	EntityID             string
	AssertionID          string
	EvidenceID           string
	DerivationID         string
	ModelRunID           string
	TraceID              string
	ContextBlockID       string
	PredicateID          string
	OntologyVersionID    string
	ConflictSetID        string
	ResolutionDecisionID string
)

// newID returns a UUIDv7: globally unique and lexicographically sortable by
// creation time, which keeps index locality good for append-heavy tables
// (AGENTS.md section 6).
func newID() string {
	return uuid.Must(uuid.NewV7()).String()
}

func NewWorkspaceID() WorkspaceID     { return WorkspaceID(newID()) }
func NewGraphSpaceID() GraphSpaceID   { return GraphSpaceID(newID()) }
func NewCollectionID() CollectionID   { return CollectionID(newID()) }
func NewSourceID() SourceID           { return SourceID(newID()) }
func NewSourceEventID() SourceEventID { return SourceEventID(newID()) }
func NewArtifactID() ArtifactID       { return ArtifactID(newID()) }
func NewEpisodeID() EpisodeID         { return EpisodeID(newID()) }
func NewChunkID() ChunkID             { return ChunkID(newID()) }
func NewPipelineRunID() PipelineRunID { return PipelineRunID(newID()) }
func NewStageRunID() StageRunID       { return StageRunID(newID()) }
func NewOutboxEventID() OutboxEventID { return OutboxEventID(newID()) }
func NewEntityID() EntityID           { return EntityID(newID()) }
func NewAssertionID() AssertionID     { return AssertionID(newID()) }
func NewEvidenceID() EvidenceID       { return EvidenceID(newID()) }
func NewDerivationID() DerivationID   { return DerivationID(newID()) }
func NewPredicateID() PredicateID     { return PredicateID(newID()) }
func NewOntologyVersionID() OntologyVersionID {
	return OntologyVersionID(newID())
}
func NewConflictSetID() ConflictSetID { return ConflictSetID(newID()) }

// NewUUIDString returns a bare UUIDv7 string, for durable rows whose identifiers
// are not part of the domain vocabulary (audit entries, for example).
func NewUUIDString() string { return newID() }

// IsZero reports whether a typed identifier is unset.
func IsZero[T ~string](id T) bool { return string(id) == "" }

// ValidUUID reports whether a typed identifier parses as a UUID. Handlers use it
// to reject malformed path parameters before they reach a query.
func ValidUUID[T ~string](id T) bool {
	_, err := uuid.Parse(string(id))
	return err == nil
}

// keyVersion prefixes every derived key so the derivation scheme can change
// without silently colliding with keys minted by an older version.
const keyVersion = "v1"

// DeriveKey builds a deterministic key from ordered parts. Used for idempotency
// keys and outbox dedupe keys, where identity must be stable across replays
// rather than freshly generated (AGENTS.md sections 6, 10.4, 12).
//
// Parts are NUL-separated so ("ab","c") and ("a","bc") never collide.
func DeriveKey(namespace string, parts ...string) string {
	h := sha256.New()
	h.Write([]byte(keyVersion))
	h.Write([]byte{0})
	h.Write([]byte(namespace))
	h.Write([]byte{0})
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ContentHash is the canonical content address for raw payloads. It is both the
// blob storage key component and the change detector for idempotent replay.
func ContentHash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
