package portable

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/gimantha/strata/internal/domain"
	"github.com/gimantha/strata/internal/knowledge"
)

// ImportStore is what an import writes through.
type ImportStore interface {
	DefinePredicate(ctx context.Context, definition domain.PredicateDefinition, actor domain.PrincipalID) (domain.PredicateDefinition, error)
	GetSourceByName(ctx context.Context, ws domain.WorkspaceID, name string) (domain.Source, error)
	CreateSource(ctx context.Context, source domain.Source, actor domain.PrincipalID) (domain.Source, error)
}

// Committer writes imported knowledge through the ordinary path.
//
// An import has no privileged route into the ledger. Every claim goes through the same
// validation, resolution, reconciliation, and ontology enforcement as one asserted by hand,
// which is what stops a package from being a way to smuggle knowledge past the rules.
type Committer interface {
	Assert(ctx context.Context, req knowledge.AssertRequest) (knowledge.AssertResult, error)
}

// Importer reads portable packages.
type Importer struct {
	store     ImportStore
	committer Committer
	events    EventRecorder
	opts      Options
	logger    *slog.Logger
}

// EventRecorder creates the source event and episode an imported package is attributed to.
//
// An import is an ingestion like any other: the package is archived, an event records where
// the knowledge came from, and every imported claim cites it. Without that, imported facts
// would have no provenance in the target — exactly the property a context graph cannot give
// up, even for its own export format.
//
// It returns an episode as well as an event because evidence must point at source material,
// and in the target deployment the package *is* the source material. The exporter's chunk and
// episode ids mean nothing here; the package, archived, is what this system actually saw.
type EventRecorder interface {
	RecordImport(ctx context.Context, scope domain.Scope, principal domain.PrincipalRef, sourceID domain.SourceID, payload []byte, idempotencyKey string) (domain.SourceEventID, domain.EpisodeID, error)
}

func NewImporter(store ImportStore, committer Committer, events EventRecorder, opts Options, logger *slog.Logger) *Importer {
	if opts.Clock == nil {
		opts.Clock = func() time.Time { return time.Now().UTC() }
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Importer{store: store, committer: committer, events: events, opts: opts, logger: logger}
}

// ImportRequest asks for one package to be read in.
type ImportRequest struct {
	Scope     domain.Scope
	Principal domain.PrincipalRef

	// SourceName attributes the imported knowledge. Defaults to a name derived from the
	// package's origin, so an operator can tell imported facts from locally observed ones
	// without reading provenance chains.
	SourceName string

	// DryRun validates and reports without committing, which is how a package from an
	// unfamiliar deployment is inspected before it is trusted.
	DryRun bool
	// AcceptPredicates registers predicate definitions the package declares. Off by
	// default: adopting another deployment's semantics unreviewed is how a predicate
	// marked functional elsewhere quietly starts retiring claims here.
	AcceptPredicates bool
}

// Import reads a package and commits what it contains.
//
// Nothing is committed until the whole stream has been read and its digest verified. That
// ordering is the point: a package that was truncated, reordered, or edited in transit must
// not leave half its knowledge behind, and the only way to know is to reach the manifest.
func (i *Importer) Import(ctx context.Context, req ImportRequest, r io.Reader) (domain.ImportSummary, error) {
	const op = "portable.Import"

	staged, header, err := i.read(ctx, r)
	if err != nil {
		return domain.ImportSummary{}, err
	}

	summary := domain.ImportSummary{
		Header:     header,
		Entities:   len(staged.entities),
		Assertions: len(staged.assertions),
		Evidence:   len(staged.evidence),
		Predicates: len(staged.predicates),
		Chunks:     len(staged.chunks),
	}
	if req.DryRun {
		// Verified and described, nothing written. The digest check above already ran, so
		// a dry run is a real integrity check rather than a syntax check.
		return summary, nil
	}

	source, err := i.resolveSource(ctx, req, header)
	if err != nil {
		return domain.ImportSummary{}, err
	}

	// The package itself is archived as the source material for everything it contains, so
	// an imported claim's provenance reaches something real in this deployment rather than
	// dangling at a reference to another one.
	payload, err := json.Marshal(header)
	if err != nil {
		return domain.ImportSummary{}, domain.Wrap(err, domain.CodeInternal, op,
			"cannot encode the package header")
	}
	eventID, episodeID, err := i.events.RecordImport(ctx, req.Scope, req.Principal, source,
		payload, "package:"+staged.manifest.Digest)
	if err != nil {
		return domain.ImportSummary{}, err
	}

	if req.AcceptPredicates {
		for _, predicate := range staged.predicates {
			if _, err := i.store.DefinePredicate(ctx,
				predicate.ToDefinition(req.Scope.WorkspaceID), req.Principal.ID); err != nil {
				return domain.ImportSummary{}, err
			}
		}
	} else if len(staged.predicates) > 0 {
		summary.Rejected = append(summary.Rejected, domain.ImportRejection{
			Kind:   domain.PackagePredicate,
			Reason: "predicate definitions were not accepted; pass accept_predicates to adopt them",
		})
	}

	claims, rejected := i.buildClaims(staged, episodeID)
	summary.Rejected = append(summary.Rejected, rejected...)

	if len(claims) > 0 {
		result, err := i.committer.Assert(ctx, knowledge.AssertRequest{
			Scope:         req.Scope,
			Principal:     req.Principal,
			SourceEventID: eventID,
			Claims:        claims,
			// A package may carry claims this deployment's schema refuses. Quarantining
			// keeps them visible for review rather than failing the whole import or
			// silently dropping them.
			OnViolation: domain.DispositionQuarantine,
		})
		if err != nil {
			return domain.ImportSummary{}, err
		}
		summary.Assertions = len(result.Assertions) - result.Duplicates
		summary.Duplicates = result.Duplicates
	}

	i.logger.InfoContext(ctx, "context package imported",
		slog.String("from_workspace", header.Source.WorkspaceSlug),
		slog.String("digest", staged.manifest.Digest),
		slog.Int("assertions", summary.Assertions),
		slog.Int("duplicates", summary.Duplicates),
		slog.Int("rejected", len(summary.Rejected)))
	return summary, nil
}

// stagedPackage holds a package's contents after verification.
type stagedPackage struct {
	manifest    domain.PackageManifest
	predicates  []domain.PortablePredicate
	entities    map[string]domain.PortableEntity
	entityOrder []string
	assertions  []domain.PortableAssertion
	evidence    map[string][]domain.PortableEvidence
	chunks      []domain.PortableChunk
}

// read streams a package, verifying its digest before returning anything.
func (i *Importer) read(ctx context.Context, r io.Reader) (stagedPackage, domain.PackageHeader, error) {
	const op = "portable.read"

	staged := stagedPackage{
		entities: map[string]domain.PortableEntity{},
		evidence: map[string][]domain.PortableEvidence{},
	}

	scanner := bufio.NewScanner(r)
	// Row images and long quotes are ordinary; a line larger than this is corruption.
	scanner.Buffer(make([]byte, 0, 64<<10), 8<<20)

	var (
		header   domain.PackageHeader
		digest   = domain.NewPackageDigest()
		manifest domain.PackageManifest
		line     int
		haveHead bool
		haveTail bool
	)

	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return stagedPackage{}, domain.PackageHeader{}, err
		}
		line++
		raw := strings.TrimSpace(scanner.Text())
		if raw == "" {
			continue
		}

		if !haveHead {
			if err := json.Unmarshal([]byte(raw), &header); err != nil {
				return stagedPackage{}, domain.PackageHeader{},
					domain.Wrap(err, domain.CodeInvalidArgument, op,
						"cannot decode the package header")
			}
			if err := header.Validate(); err != nil {
				return stagedPackage{}, domain.PackageHeader{}, err
			}
			haveHead = true
			continue
		}

		// The manifest is the last line. Probing for it before decoding a record keeps the
		// format single-pass without a length prefix or a sentinel.
		var probe struct {
			Format string `json:"format"`
		}
		if err := json.Unmarshal([]byte(raw), &probe); err == nil && probe.Format == domain.ManifestFormat {
			if err := json.Unmarshal([]byte(raw), &manifest); err != nil {
				return stagedPackage{}, domain.PackageHeader{},
					domain.Wrap(err, domain.CodeInvalidArgument, op,
						"cannot decode the package manifest")
			}
			haveTail = true
			continue
		}
		if haveTail {
			return stagedPackage{}, domain.PackageHeader{},
				domain.Errorf(domain.CodeInvalidArgument, op,
					"package continues past its manifest at line %d", line)
		}

		var record domain.PackageRecord
		if err := json.Unmarshal([]byte(raw), &record); err != nil {
			return stagedPackage{}, domain.PackageHeader{},
				domain.Wrap(err, domain.CodeInvalidArgument, op,
					"cannot decode line "+strconv.Itoa(line))
		}
		digest.Add(record.Kind, []byte(raw))

		if err := staged.add(record); err != nil {
			return stagedPackage{}, domain.PackageHeader{}, err
		}
	}
	if err := scanner.Err(); err != nil {
		return stagedPackage{}, domain.PackageHeader{},
			domain.Wrap(err, domain.CodeInvalidArgument, op, "cannot read the package")
	}

	if !haveHead {
		return stagedPackage{}, domain.PackageHeader{},
			domain.Errorf(domain.CodeInvalidArgument, op, "package is empty")
	}
	if !haveTail {
		// A package without its trailer is a truncated download, and importing most of one
		// is worse than importing none: the missing part is silent.
		return stagedPackage{}, domain.PackageHeader{},
			domain.Errorf(domain.CodeInvalidArgument, op,
				"package has no manifest; it was truncated in transit")
	}
	if err := manifest.Validate(); err != nil {
		return stagedPackage{}, domain.PackageHeader{}, err
	}
	if err := digest.Verify(manifest); err != nil {
		return stagedPackage{}, domain.PackageHeader{}, err
	}

	staged.manifest = manifest
	return staged, header, nil
}

// add stages one decoded record.
func (s *stagedPackage) add(record domain.PackageRecord) error {
	const op = "portable.add"

	switch record.Kind {
	case domain.PackagePredicate:
		var predicate domain.PortablePredicate
		if err := json.Unmarshal(record.Data, &predicate); err != nil {
			return domain.Wrap(err, domain.CodeInvalidArgument, op, "malformed predicate record")
		}
		s.predicates = append(s.predicates, predicate)

	case domain.PackageEntity:
		var entity domain.PortableEntity
		if err := json.Unmarshal(record.Data, &entity); err != nil {
			return domain.Wrap(err, domain.CodeInvalidArgument, op, "malformed entity record")
		}
		if _, seen := s.entities[entity.SourceID]; !seen {
			s.entityOrder = append(s.entityOrder, entity.SourceID)
		}
		s.entities[entity.SourceID] = entity

	case domain.PackageAssertion:
		var assertion domain.PortableAssertion
		if err := json.Unmarshal(record.Data, &assertion); err != nil {
			return domain.Wrap(err, domain.CodeInvalidArgument, op, "malformed assertion record")
		}
		s.assertions = append(s.assertions, assertion)

	case domain.PackageEvidence:
		var evidence domain.PortableEvidence
		if err := json.Unmarshal(record.Data, &evidence); err != nil {
			return domain.Wrap(err, domain.CodeInvalidArgument, op, "malformed evidence record")
		}
		s.evidence[evidence.AssertionRef] = append(s.evidence[evidence.AssertionRef], evidence)

	case domain.PackageChunk:
		var chunk domain.PortableChunk
		if err := json.Unmarshal(record.Data, &chunk); err != nil {
			return domain.Wrap(err, domain.CodeInvalidArgument, op, "malformed chunk record")
		}
		s.chunks = append(s.chunks, chunk)

	case domain.PackageOntology, domain.PackageAlias, domain.PackageEmbedding:
		// Read and digested, not yet applied. Ontology adoption and vector import are
		// their own decisions with their own consequences, and silently applying them
		// because they happened to be in the file would be the wrong default.

	default:
		return domain.Errorf(domain.CodeInvalidArgument, op,
			"package contains an unknown record kind %q", record.Kind)
	}
	return nil
}

// buildClaims turns staged records into claims, resolving package-local references.
//
// References are resolved by name rather than by carrying identifiers across: the target
// deployment's resolver decides identity, exactly as it would for a claim arriving from any
// other source. An imported entity that matches an existing one merges into it, which is what
// makes importing into a populated workspace useful rather than duplicative.
func (i *Importer) buildClaims(staged stagedPackage, episodeID domain.EpisodeID) ([]knowledge.Claim, []domain.ImportRejection) {
	var (
		claims   []knowledge.Claim
		rejected []domain.ImportRejection
	)

	for _, assertion := range staged.assertions {
		subject, ok := staged.entities[assertion.SubjectRef]
		if !ok {
			rejected = append(rejected, domain.ImportRejection{
				Kind: domain.PackageAssertion, SourceID: assertion.SourceID,
				Reason: "the package references a subject it does not contain",
			})
			continue
		}

		claim := knowledge.Claim{
			Subject: knowledge.EntityRef{
				Name: subject.CanonicalName, Type: subject.EntityType,
				Aliases: subject.Aliases,
			},
			Predicate:      assertion.Predicate,
			Object:         assertion.Object,
			ScopeKey:       assertion.ScopeKey,
			MemoryKind:     assertion.MemoryKind,
			Confidence:     assertion.Confidence,
			Classification: assertion.Classification,
			// Imported, whatever the package said it was. A claim extracted by another
			// deployment was extracted there; here it arrived in a file, and recording
			// anything else would misstate how this system came to believe it.
			ProvenanceMode: domain.ProvenanceImported,
		}

		if assertion.ObjectEntityRef != "" {
			object, ok := staged.entities[assertion.ObjectEntityRef]
			if !ok {
				rejected = append(rejected, domain.ImportRejection{
					Kind: domain.PackageAssertion, SourceID: assertion.SourceID,
					Reason: "the package references an object entity it does not contain",
				})
				continue
			}
			claim.ObjectEntity = &knowledge.EntityRef{
				Name: object.CanonicalName, Type: object.EntityType,
			}
		}

		temporal := assertion.Temporal.ToCoordinates(i.opts.Clock())
		claim.EventTime = temporal.EventTime
		claim.ValidFrom = temporal.ValidFrom
		claim.ValidTo = temporal.ValidTo
		claim.ActiveFrom = temporal.ActiveFrom
		claim.ActiveUntil = temporal.ActiveUntil
		claim.DecayStartsAt = temporal.DecayStartsAt
		claim.ExpiresAt = temporal.ExpiresAt

		for _, item := range staged.evidence[assertion.SourceID] {
			// Evidence travels as a quote, not as a chunk reference: the chunk ids belonged
			// to another deployment. It cites the archived package, because that is the
			// source material this deployment actually holds, and it keeps the original
			// quote, because that is what makes the claim checkable after the move.
			quote := item.ExtractedText
			if item.SourceName != "" {
				quote = "[" + item.SourceName + "] " + quote
			}
			claim.Evidence = append(claim.Evidence, knowledge.EvidenceInput{
				EpisodeID:     episodeID,
				ExtractedText: quote,
				Confidence:    item.Confidence,
			})
		}
		if len(claim.Evidence) == 0 {
			// A claim that arrived without evidence still cites the package it came in,
			// so provenance reaches something real rather than dangling.
			claim.Evidence = []knowledge.EvidenceInput{{
				EpisodeID:     episodeID,
				ExtractedText: "imported from " + assertion.SourceID,
			}}
		}

		claims = append(claims, claim)
	}
	return claims, rejected
}

// resolveSource finds or creates the source imported knowledge is attributed to.
func (i *Importer) resolveSource(ctx context.Context, req ImportRequest, header domain.PackageHeader) (domain.SourceID, error) {
	name := req.SourceName
	if name == "" {
		// Named after where it came from, so imported facts are distinguishable from
		// locally observed ones at a glance rather than by walking provenance.
		name = "import:" + header.Source.WorkspaceSlug + "/" + header.Source.GraphSpaceSlug
	}

	existing, err := i.store.GetSourceByName(ctx, req.Scope.WorkspaceID, name)
	if err == nil {
		return existing.ID, nil
	}
	if !domain.IsCode(err, domain.CodeNotFound) {
		return "", err
	}

	created, err := i.store.CreateSource(ctx, domain.Source{
		WorkspaceID: req.Scope.WorkspaceID,
		Kind:        domain.SourceKindImport,
		Name:        name,
		URI:         header.Source.Instance,
		// Imported knowledge is not authoritative here by default. Another deployment's
		// confidence is not this one's, and a package should not outrank locally observed
		// facts merely by arriving.
		TrustLevel:     domain.TrustStandard,
		Classification: domain.ClassificationInternal,
	}, req.Principal.ID)
	if err != nil {
		return "", err
	}
	return created.ID, nil
}

// Verify reads a package and checks its integrity without importing anything.
func Verify(ctx context.Context, r io.Reader) (domain.PackageHeader, domain.PackageManifest, error) {
	importer := &Importer{opts: Options{Clock: func() time.Time { return time.Now().UTC() }},
		logger: slog.New(slog.DiscardHandler)}

	staged, header, err := importer.read(ctx, r)
	if err != nil {
		return domain.PackageHeader{}, domain.PackageManifest{}, err
	}
	return header, staged.manifest, nil
}
