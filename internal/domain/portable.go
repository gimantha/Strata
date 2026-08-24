package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
	"time"
)

// PackageFormat identifies the portable export (AGENTS.md section 29).
const PackageFormat = "strata.context-package"

// PackageVersion is the format version. Bump it when a reader from the previous version
// would misread a package rather than merely miss a field.
const PackageVersion = 1

// PackageRecordKind names what one line of a package holds.
//
// Kinds are ordered by dependency: an assertion references entities and predicates, evidence
// references assertions. An importer that reads in this order never holds a reference it
// cannot resolve.
type PackageRecordKind string

const (
	PackageOntology  PackageRecordKind = "ontology"
	PackagePredicate PackageRecordKind = "predicate"
	PackageEntity    PackageRecordKind = "entity"
	PackageAlias     PackageRecordKind = "alias"
	PackageAssertion PackageRecordKind = "assertion"
	PackageEvidence  PackageRecordKind = "evidence"
	PackageChunk     PackageRecordKind = "chunk"
	PackageEmbedding PackageRecordKind = "embedding"
)

// PackageRecordOrder is the dependency order sections must be written in.
var PackageRecordOrder = []PackageRecordKind{
	PackageOntology, PackagePredicate, PackageEntity, PackageAlias,
	PackageAssertion, PackageEvidence, PackageChunk, PackageEmbedding,
}

// PackageHeader is the first line of a package.
type PackageHeader struct {
	Format  string `json:"format"`
	Version int    `json:"version"`

	CreatedAt time.Time    `json:"created_at"`
	CreatedBy PrincipalRef `json:"created_by,omitempty"`

	// Source describes where the package came from, by slug rather than by id. Ids are
	// meaningless in another deployment, and a package that carried them as identity
	// would invite an importer to trust them (AGENTS.md section 29).
	Source PackageSource `json:"source"`

	// Policy records the narrowing the export ran under, so a reader can tell a partial
	// package from a complete one. An export filtered by clearance is a legitimate
	// artifact; one that silently looks complete is not.
	Policy PackagePolicy `json:"policy"`

	// Sections declares what this package contains, so an importer can refuse a package
	// whose contents it does not understand before reading a million lines of it.
	Sections []PackageRecordKind `json:"sections"`

	Notes string `json:"notes,omitempty"`
}

// PackageSource identifies the exporting deployment without leaking its identifiers.
type PackageSource struct {
	WorkspaceSlug  string `json:"workspace_slug"`
	GraphSpaceSlug string `json:"graph_space_slug"`
	// Instance is a free-form label for the deployment, for operators tracing where a
	// package came from.
	Instance string `json:"instance,omitempty"`
}

// PackagePolicy records what the export was allowed to include.
type PackagePolicy struct {
	MaxClassification Classification   `json:"max_classification,omitempty"`
	Classifications   []Classification `json:"classifications,omitempty"`
	// Filtered says a ceiling was in force, so material above it is absent whether or not
	// any existed. Excluded counts what the export actually dropped, which is only the
	// part it can see: policy narrows the query too, and a record the database never
	// returned cannot be counted without asking a second time.
	//
	// Two facts rather than one because they answer different questions. "Could this be
	// incomplete" decides whether a package may be treated as a backup; "was anything
	// dropped" is what an operator checks when a claim they expected is missing.
	Filtered bool `json:"filtered"`
	Excluded int  `json:"excluded,omitempty"`
}

func (h PackageHeader) Validate() error {
	const op = "domain.PackageHeader.Validate"

	if h.Format != PackageFormat {
		return Errorf(CodeInvalidArgument, op,
			"not a context package: format is %q", h.Format)
	}
	if h.Version <= 0 || h.Version > PackageVersion {
		return Errorf(CodeInvalidArgument, op,
			"package version %d is not supported by this build (highest known is %d)",
			h.Version, PackageVersion)
	}
	for _, section := range h.Sections {
		if !knownSection(section) {
			return Errorf(CodeInvalidArgument, op,
				"package declares unknown section %q", section)
		}
	}
	return nil
}

func knownSection(kind PackageRecordKind) bool {
	for _, known := range PackageRecordOrder {
		if known == kind {
			return true
		}
	}
	return false
}

// PackageRecord is one line of the body.
type PackageRecord struct {
	Kind PackageRecordKind `json:"kind"`
	Data json.RawMessage   `json:"data"`
}

// PackageManifest is the trailer: counts and integrity digests (AGENTS.md section 29).
//
// A trailer rather than a header, because that is what lets a package be written as a stream
// without buffering it: digests are accumulated while records are written, and the totals are
// known only at the end. A reader gets them at the end too, which is fine — nothing may be
// committed before the whole package has been verified anyway.
type PackageManifest struct {
	Format string `json:"format"`
	// Counts per section, so a truncated package is detected even if its digest somehow
	// matched.
	Counts map[PackageRecordKind]int `json:"counts"`
	// Digests per section, so a mismatch says which part is wrong rather than only that
	// something is.
	Digests map[PackageRecordKind]string `json:"digests"`
	// Digest covers every record line in order, and is what an importer checks before
	// committing anything.
	Digest string `json:"digest"`
}

// ManifestFormat marks the trailer line.
const ManifestFormat = "strata.context-package.manifest"

func (m PackageManifest) Validate() error {
	const op = "domain.PackageManifest.Validate"

	if m.Format != ManifestFormat {
		return Errorf(CodeInvalidArgument, op,
			"package is missing its manifest: found %q", m.Format)
	}
	if m.Digest == "" {
		return Errorf(CodeInvalidArgument, op, "package manifest carries no digest")
	}
	return nil
}

// PackageDigest accumulates integrity hashes while a package is written or read.
//
// One rolling hash over every record line in order, plus one per section. The ordered hash is
// what catches a reordered or truncated package; the per-section hashes are what make a
// mismatch diagnosable instead of merely alarming.
type PackageDigest struct {
	overall  hashState
	sections map[PackageRecordKind]hashState
	counts   map[PackageRecordKind]int
}

type hashState struct {
	sum []byte
}

// NewPackageDigest starts an accumulator.
func NewPackageDigest() *PackageDigest {
	return &PackageDigest{
		sections: map[PackageRecordKind]hashState{},
		counts:   map[PackageRecordKind]int{},
	}
}

// Add folds one record line into the digests.
//
// Chained rather than concatenated: each step hashes the previous digest with the new line,
// so the result depends on order. A package whose records were shuffled has the same set of
// lines and a different digest, which is the property that matters — records reference each
// other, and order is part of the meaning.
func (d *PackageDigest) Add(kind PackageRecordKind, line []byte) {
	d.overall = chain(d.overall, line)

	section := d.sections[kind]
	d.sections[kind] = chain(section, line)
	d.counts[kind]++
}

func chain(state hashState, line []byte) hashState {
	hasher := sha256.New()
	hasher.Write(state.sum)
	hasher.Write(line)
	hasher.Write([]byte{'\n'})
	return hashState{sum: hasher.Sum(nil)}
}

// Manifest renders the accumulated digests.
func (d *PackageDigest) Manifest() PackageManifest {
	manifest := PackageManifest{
		Format:  ManifestFormat,
		Counts:  map[PackageRecordKind]int{},
		Digests: map[PackageRecordKind]string{},
		Digest:  "sha256:" + hex.EncodeToString(d.overall.sum),
	}
	for kind, count := range d.counts {
		manifest.Counts[kind] = count
	}
	for kind, state := range d.sections {
		manifest.Digests[kind] = "sha256:" + hex.EncodeToString(state.sum)
	}
	return manifest
}

// Verify compares accumulated digests against a manifest and explains any mismatch.
func (d *PackageDigest) Verify(manifest PackageManifest) error {
	const op = "domain.PackageDigest.Verify"

	computed := d.Manifest()
	if computed.Digest != manifest.Digest {
		// Name the offending section when one can be identified: "the package is corrupt"
		// is a much worse message than "the assertions section is corrupt".
		for _, kind := range PackageRecordOrder {
			declared, stated := manifest.Digests[kind]
			if !stated {
				continue
			}
			if computed.Digests[kind] != declared {
				return Errorf(CodeInvalidArgument, op,
					"package integrity check failed in the %s section: "+
						"expected %s, computed %s", kind, declared, computed.Digests[kind])
			}
		}
		return Errorf(CodeInvalidArgument, op,
			"package integrity check failed: expected %s, computed %s",
			manifest.Digest, computed.Digest)
	}

	for kind, declared := range manifest.Counts {
		if computed.Counts[kind] != declared {
			return Errorf(CodeInvalidArgument, op,
				"package declares %d %s records but contains %d",
				declared, kind, computed.Counts[kind])
		}
	}
	return nil
}

// PortableEntity is an identity as it travels.
//
// The original id is carried as provenance rather than as identity: an importer mints its own
// and records where the record came from, so two deployments cannot collide and a re-import
// can recognize what it already has.
type PortableEntity struct {
	SourceID      string         `json:"source_id"`
	CanonicalName string         `json:"canonical_name"`
	EntityType    string         `json:"entity_type"`
	Aliases       []string       `json:"aliases,omitempty"`
	Metadata      map[string]any `json:"metadata,omitempty"`
}

// PortableAssertion is a claim as it travels, with references by package-local id.
type PortableAssertion struct {
	SourceID string `json:"source_id"`

	SubjectRef string          `json:"subject_ref"`
	Predicate  string          `json:"predicate"`
	Object     AssertionObject `json:"object"`
	// ObjectEntityRef resolves an entity-valued object within the package.
	ObjectEntityRef string `json:"object_entity_ref,omitempty"`

	MemoryKind MemoryKind `json:"memory_kind,omitempty"`
	ScopeKey   string     `json:"scope_key,omitempty"`

	Temporal   PortableTemporal `json:"temporal"`
	Confidence float64          `json:"confidence,omitempty"`
	Status     AssertionStatus  `json:"status,omitempty"`

	ProvenanceMode ProvenanceMode `json:"provenance_mode,omitempty"`
	Classification Classification `json:"classification,omitempty"`

	// EvidenceRefs name the package's evidence records supporting this claim.
	EvidenceRefs []string `json:"evidence_refs,omitempty"`
	// DerivedFrom names the package-local assertions this claim was reasoned from, so a
	// derived fact stays explicable after a move (AGENTS.md sections 21.1, 29).
	DerivedFrom []string `json:"derived_from,omitempty"`
}

// PortableTemporal carries all four clocks.
//
// Every one of them, because a package that dropped knowledge time would import a corrected
// claim as though it had always been believed — which is the specific lie the ledger exists
// to prevent.
type PortableTemporal struct {
	EventTime  *time.Time `json:"event_time,omitempty"`
	ValidFrom  *time.Time `json:"valid_from,omitempty"`
	ValidTo    *time.Time `json:"valid_to,omitempty"`
	ObservedAt *time.Time `json:"observed_at,omitempty"`
	RecordedAt *time.Time `json:"recorded_at,omitempty"`

	SourceTime     *time.Time `json:"source_time,omitempty"`
	SourceSequence string     `json:"source_sequence,omitempty"`

	ActiveFrom    *time.Time `json:"active_from,omitempty"`
	ActiveUntil   *time.Time `json:"active_until,omitempty"`
	DecayStartsAt *time.Time `json:"decay_starts_at,omitempty"`
	ExpiresAt     *time.Time `json:"expires_at,omitempty"`
}

// ToCoordinates rebuilds temporal coordinates on import.
//
// Knowledge time is deliberately not taken from the package. When this deployment learned
// something is a fact about this deployment, and copying the exporter's clock would claim we
// believed it before the package existed. The original is kept as source time instead, where
// it belongs.
func (t PortableTemporal) ToCoordinates(now time.Time) TemporalCoordinates {
	coordinates := TemporalCoordinates{
		EventTime:      t.EventTime,
		ValidFrom:      t.ValidFrom,
		ValidTo:        t.ValidTo,
		ObservedAt:     now,
		RecordedAt:     now,
		SourceTime:     firstNonNil(t.SourceTime, t.RecordedAt),
		SourceSequence: t.SourceSequence,
		ActiveFrom:     t.ActiveFrom,
		ActiveUntil:    t.ActiveUntil,
		DecayStartsAt:  t.DecayStartsAt,
		ExpiresAt:      t.ExpiresAt,
	}
	return coordinates.Normalize()
}

// PortableTemporalOf renders a claim's clocks for export.
func PortableTemporalOf(t TemporalCoordinates) PortableTemporal {
	return PortableTemporal{
		EventTime:      t.EventTime,
		ValidFrom:      t.ValidFrom,
		ValidTo:        t.ValidTo,
		ObservedAt:     &t.ObservedAt,
		RecordedAt:     &t.RecordedAt,
		SourceTime:     t.SourceTime,
		SourceSequence: t.SourceSequence,
		ActiveFrom:     t.ActiveFrom,
		ActiveUntil:    t.ActiveUntil,
		DecayStartsAt:  t.DecayStartsAt,
		ExpiresAt:      t.ExpiresAt,
	}
}

func firstNonNil(values ...*time.Time) *time.Time {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}

// PortableEvidence is a citation as it travels.
//
// It carries the quote and the locator but not the artifact bytes: a package is knowledge,
// not a filesystem. The chunk section exists for deployments that want the passages too, and
// is optional precisely because copying source material has different policy consequences
// from copying conclusions.
type PortableEvidence struct {
	SourceID      string `json:"source_id"`
	AssertionRef  string `json:"assertion_ref"`
	ExtractedText string `json:"extracted_text,omitempty"`
	Locator       string `json:"locator,omitempty"`
	// SourceName identifies the upstream this evidence came from, so provenance survives
	// even though the source's identifiers do not.
	SourceName string  `json:"source_name,omitempty"`
	Confidence float64 `json:"confidence,omitempty"`
}

// PortableChunk is an optional passage.
type PortableChunk struct {
	SourceID       string         `json:"source_id"`
	Content        string         `json:"content"`
	Classification Classification `json:"classification,omitempty"`
	Locator        string         `json:"locator,omitempty"`
}

// PortableEmbedding is an optional precomputed vector, tagged with what produced it.
//
// Tagged because vectors from different models are not comparable, and an untagged vector is
// worse than no vector: it silently pollutes a projection with numbers from another geometry.
type PortableEmbedding struct {
	Surface   Surface   `json:"surface"`
	RecordRef string    `json:"record_ref"`
	Model     string    `json:"model"`
	Version   int       `json:"version"`
	Vector    []float32 `json:"vector"`
}

// PortablePredicate is a registry entry as it travels.
type PortablePredicate struct {
	Name              string         `json:"name"`
	Description       string         `json:"description,omitempty"`
	SubjectTypes      []string       `json:"subject_types,omitempty"`
	ObjectTypes       []string       `json:"object_types,omitempty"`
	ObjectKinds       []ObjectKind   `json:"object_kinds,omitempty"`
	Functional        bool           `json:"functional,omitempty"`
	InverseFunctional bool           `json:"inverse_functional,omitempty"`
	Symmetric         bool           `json:"symmetric,omitempty"`
	Transitive        bool           `json:"transitive,omitempty"`
	TemporalPolicy    TemporalPolicy `json:"temporal_policy,omitempty"`
	ConflictPolicy    ConflictPolicy `json:"conflict_policy,omitempty"`
	DefaultMemoryKind MemoryKind     `json:"default_memory_kind,omitempty"`
	Sensitivity       Classification `json:"sensitivity,omitempty"`
}

// ToDefinition rebuilds a registry entry on import.
func (p PortablePredicate) ToDefinition(ws WorkspaceID) PredicateDefinition {
	definition := PredicateDefinition{
		WorkspaceID:       ws,
		Name:              NormalizePredicateName(p.Name),
		Description:       p.Description,
		SubjectTypes:      p.SubjectTypes,
		ObjectTypes:       p.ObjectTypes,
		ObjectKinds:       p.ObjectKinds,
		Functional:        p.Functional,
		InverseFunctional: p.InverseFunctional,
		Symmetric:         p.Symmetric,
		Transitive:        p.Transitive,
		TemporalPolicy:    p.TemporalPolicy,
		ConflictPolicy:    p.ConflictPolicy,
		DefaultMemoryKind: p.DefaultMemoryKind,
		Sensitivity:       p.Sensitivity,
		// Imported, not approved: a predicate arriving in a package has been declared
		// somewhere else, and adopting another deployment's semantics unreviewed is how a
		// functional predicate quietly starts retiring claims here.
		Status: PredicateCandidate,
	}
	if definition.TemporalPolicy == "" {
		definition.TemporalPolicy = TemporalPolicyStateful
	}
	if definition.ConflictPolicy == "" {
		definition.ConflictPolicy = ConflictPolicyCoexist
	}
	if definition.DefaultMemoryKind == "" {
		definition.DefaultMemoryKind = MemorySemantic
	}
	if definition.Sensitivity == "" {
		definition.Sensitivity = ClassificationInternal
	}
	return definition
}

// PortablePredicateOf renders a registry entry for export.
func PortablePredicateOf(d PredicateDefinition) PortablePredicate {
	return PortablePredicate{
		Name:              d.Name,
		Description:       d.Description,
		SubjectTypes:      d.SubjectTypes,
		ObjectTypes:       d.ObjectTypes,
		ObjectKinds:       d.ObjectKinds,
		Functional:        d.Functional,
		InverseFunctional: d.InverseFunctional,
		Symmetric:         d.Symmetric,
		Transitive:        d.Transitive,
		TemporalPolicy:    d.TemporalPolicy,
		ConflictPolicy:    d.ConflictPolicy,
		DefaultMemoryKind: d.DefaultMemoryKind,
		Sensitivity:       d.Sensitivity,
	}
}

// ImportSummary reports what an import did.
type ImportSummary struct {
	Header PackageHeader

	Entities   int
	Assertions int
	Evidence   int
	Predicates int
	Chunks     int
	Embeddings int

	// Duplicates are records the target already had. A re-import is expected to be mostly
	// duplicates, and reporting them separately is what makes that visible rather than
	// looking like the import did nothing.
	Duplicates int
	// Rejected records failed validation or policy and were not committed, with reasons.
	Rejected []ImportRejection
}

// ImportRejection explains one refused record.
type ImportRejection struct {
	Kind     PackageRecordKind `json:"kind"`
	SourceID string            `json:"source_id,omitempty"`
	Reason   string            `json:"reason"`
}

// Describe renders a one-line summary, for logs and CLI output.
func (s ImportSummary) Describe() string {
	parts := []string{
		itoa(s.Entities) + " entities",
		itoa(s.Assertions) + " assertions",
		itoa(s.Evidence) + " evidence",
	}
	if s.Predicates > 0 {
		parts = append(parts, itoa(s.Predicates)+" predicates")
	}
	if s.Chunks > 0 {
		parts = append(parts, itoa(s.Chunks)+" chunks")
	}
	if s.Duplicates > 0 {
		parts = append(parts, itoa(s.Duplicates)+" already present")
	}
	if len(s.Rejected) > 0 {
		parts = append(parts, itoa(len(s.Rejected))+" rejected")
	}
	return strings.Join(parts, ", ")
}

// SortedSections returns the sections a package declares, in dependency order.
func SortedSections(sections []PackageRecordKind) []PackageRecordKind {
	position := map[PackageRecordKind]int{}
	for i, kind := range PackageRecordOrder {
		position[kind] = i
	}

	out := append([]PackageRecordKind(nil), sections...)
	sort.Slice(out, func(i, j int) bool { return position[out[i]] < position[out[j]] })
	return out
}
