// Package portable moves knowledge between deployments (AGENTS.md section 29).
//
// A package is a stream of JSON lines: a header, then records in dependency order, then a
// manifest of counts and integrity digests. One file, greppable and diffable, written without
// buffering and verified before anything is committed.
//
// The rule that shapes the whole design is that an importer must not trust what it is given.
// Identifiers from another deployment are provenance, never identity; knowledge time is the
// importer's own; predicates arrive as candidates rather than as approved semantics; and
// nothing is committed until the digest over every record has been checked.
package portable

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"time"

	"github.com/gimantha/strata/internal/domain"
)

// Store is the canonical surface export reads.
type Store interface {
	GetWorkspace(ctx context.Context, id domain.WorkspaceID) (domain.Workspace, error)
	GetGraphSpace(ctx context.Context, id domain.GraphSpaceID) (domain.GraphSpace, error)
	QueryAssertions(ctx context.Context, q domain.AssertionQuery) ([]domain.Assertion, error)
	GetEntity(ctx context.Context, ws domain.WorkspaceID, id domain.EntityID) (domain.Entity, error)
	ListAliases(ctx context.Context, ws domain.WorkspaceID, id domain.EntityID) ([]domain.EntityAlias, error)
	ListEvidence(ctx context.Context, ws domain.WorkspaceID, id domain.AssertionID) ([]domain.Evidence, error)
	ListPredicates(ctx context.Context, ws domain.WorkspaceID) ([]domain.PredicateDefinition, error)
	GetSourceEvent(ctx context.Context, ws domain.WorkspaceID, id domain.SourceEventID) (domain.SourceEvent, error)
	GetSource(ctx context.Context, ws domain.WorkspaceID, id domain.SourceID) (domain.Source, error)
	GetDerivation(ctx context.Context, ws domain.WorkspaceID, id domain.DerivationID) (domain.Derivation, error)
	LatestOntologyVersion(ctx context.Context, ws domain.WorkspaceID) (domain.OntologyVersion, error)
}

// Options configure the exporter.
type Options struct {
	Clock    func() time.Time
	Instance string
}

// Exporter writes portable packages.
type Exporter struct {
	store  Store
	opts   Options
	logger *slog.Logger
}

func NewExporter(store Store, opts Options, logger *slog.Logger) *Exporter {
	if opts.Clock == nil {
		opts.Clock = func() time.Time { return time.Now().UTC() }
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Exporter{store: store, opts: opts, logger: logger}
}

// ExportRequest asks for one package.
type ExportRequest struct {
	Scope     domain.Scope
	Principal domain.PrincipalRef
	// Policy is the caller's access narrowing. Export is policy-aware by construction:
	// what a principal may not read, they may not package (AGENTS.md phase 13).
	Policy domain.PolicyFilters

	// IncludeSuperseded carries history as well as current belief.
	IncludeSuperseded bool
	// IncludeChunks copies the source passages. Off by default: a package is knowledge,
	// and copying source material has different policy consequences from copying
	// conclusions.
	IncludeChunks bool
	Limit         int
	Notes         string
}

// ExportResult reports what was written.
type ExportResult struct {
	Manifest domain.PackageManifest
	Header   domain.PackageHeader
	Bytes    int64
}

// Export streams a package to a writer.
//
// Records go out in dependency order — ontology, predicates, entities, aliases, assertions,
// evidence — so an importer reading forwards never holds a reference it cannot resolve. The
// digest is accumulated as lines are written and the manifest is the last line, which is what
// lets this stream instead of buffering a workspace in memory.
func (e *Exporter) Export(ctx context.Context, req ExportRequest, w io.Writer) (ExportResult, error) {
	const op = "portable.Export"

	workspace, err := e.store.GetWorkspace(ctx, req.Scope.WorkspaceID)
	if err != nil {
		return ExportResult{}, err
	}
	graphSpace, err := e.store.GetGraphSpace(ctx, req.Scope.GraphSpaceID)
	if err != nil {
		return ExportResult{}, err
	}

	sections := []domain.PackageRecordKind{
		domain.PackagePredicate, domain.PackageEntity, domain.PackageAlias,
		domain.PackageAssertion, domain.PackageEvidence,
	}
	if req.IncludeChunks {
		sections = append(sections, domain.PackageChunk)
	}

	header := domain.PackageHeader{
		Format:    domain.PackageFormat,
		Version:   domain.PackageVersion,
		CreatedAt: e.opts.Clock(),
		CreatedBy: req.Principal,
		Source: domain.PackageSource{
			WorkspaceSlug:  workspace.Slug,
			GraphSpaceSlug: graphSpace.Slug,
			Instance:       e.opts.Instance,
		},
		Policy: domain.PackagePolicy{
			MaxClassification: req.Policy.MaxClassification,
			Classifications:   req.Policy.PermittedClassifications(),
			// Filled in after the pass: whether policy was in force is not interesting,
			// whether it removed anything is.
		},
		Sections: sections,
		Notes:    req.Notes,
	}

	writer := &packageWriter{
		out:    bufio.NewWriter(w),
		digest: domain.NewPackageDigest(),
	}
	if err := writer.header(header); err != nil {
		return ExportResult{}, err
	}

	// Ontology first: it is what makes the predicates and entity types in the rest of the
	// package interpretable, and an importer may want to refuse a package whose schema it
	// cannot accept before reading any knowledge.
	if ontology, err := e.store.LatestOntologyVersion(ctx, req.Scope.WorkspaceID); err == nil {
		if err := writer.record(domain.PackageOntology, ontology); err != nil {
			return ExportResult{}, err
		}
		header.Sections = append([]domain.PackageRecordKind{domain.PackageOntology}, header.Sections...)
	} else if !domain.IsCode(err, domain.CodeNotFound) {
		return ExportResult{}, err
	}

	predicates, err := e.store.ListPredicates(ctx, req.Scope.WorkspaceID)
	if err != nil {
		return ExportResult{}, err
	}
	for _, predicate := range predicates {
		if err := writer.record(domain.PackagePredicate,
			domain.PortablePredicateOf(predicate)); err != nil {
			return ExportResult{}, err
		}
	}

	limit := req.Limit
	if limit <= 0 || limit > domain.MaxAssertionLimit {
		limit = domain.MaxAssertionLimit
	}
	statuses := []domain.AssertionStatus{domain.AssertionActive, domain.AssertionDisputed}
	if req.IncludeSuperseded {
		statuses = append(statuses, domain.AssertionSuperseded, domain.AssertionRetracted)
	}

	assertions, err := e.store.QueryAssertions(ctx, domain.AssertionQuery{
		Scope:    req.Scope,
		Statuses: statuses,
		// Policy narrows the query, not the output. What a principal may not read, they
		// may not package.
		Classifications:   req.Policy.PermittedClassifications(),
		IncludeSuperseded: req.IncludeSuperseded,
		Limit:             limit,
	})
	if err != nil {
		return ExportResult{}, err
	}

	// Entities are written before the claims that reference them, so the package reads
	// forwards. Gathering them first costs one pass over the assertions and saves the
	// importer from having to buffer unresolved references.
	entities, order, err := e.gatherEntities(ctx, req, assertions)
	if err != nil {
		return ExportResult{}, err
	}
	for _, id := range order {
		entity := entities[id]
		aliases, err := e.store.ListAliases(ctx, req.Scope.WorkspaceID, id)
		if err != nil {
			return ExportResult{}, err
		}

		portable := domain.PortableEntity{
			SourceID:      string(entity.ID),
			CanonicalName: entity.CanonicalName,
			EntityType:    entity.EntityType,
			Metadata:      entity.Metadata,
		}
		for _, alias := range aliases {
			portable.Aliases = append(portable.Aliases, alias.Alias)
		}
		if err := writer.record(domain.PackageEntity, portable); err != nil {
			return ExportResult{}, err
		}
	}

	sourceNames := map[domain.SourceEventID]string{}
	excluded := 0

	for _, assertion := range assertions {
		if !req.Policy.Allows(assertion.Classification, "", assertion.Predicate.Name,
			assertion.MemoryKind, "") {
			// The second gate, on the record rather than the query. An export is the one
			// path where a missed filter hands over a complete copy.
			excluded++
			continue
		}

		portable, evidence, err := e.portableAssertion(ctx, req, assertion, sourceNames)
		if err != nil {
			return ExportResult{}, err
		}
		if err := writer.record(domain.PackageAssertion, portable); err != nil {
			return ExportResult{}, err
		}
		for _, item := range evidence {
			if err := writer.record(domain.PackageEvidence, item); err != nil {
				return ExportResult{}, err
			}
		}
	}

	manifest, written, err := writer.finish()
	if err != nil {
		return ExportResult{}, err
	}

	// Excluded counts only what this pass dropped. The query was narrowed too, and what
	// the database never returned cannot be counted here — which is exactly why Filtered
	// is a separate fact rather than "excluded > 0".
	header.Policy.Excluded = excluded
	header.Policy.Filtered = req.Policy.Restrictive()

	e.logger.InfoContext(ctx, "context package exported",
		slog.String("workspace", workspace.Slug),
		slog.String("graph_space", graphSpace.Slug),
		slog.Int("assertions", manifest.Counts[domain.PackageAssertion]),
		slog.Int("entities", manifest.Counts[domain.PackageEntity]),
		slog.Bool("filtered", header.Policy.Filtered),
		slog.String("digest", manifest.Digest))

	_ = op
	return ExportResult{Manifest: manifest, Header: header, Bytes: written}, nil
}

// gatherEntities collects every identity the exported claims reference, in a stable order.
func (e *Exporter) gatherEntities(ctx context.Context, req ExportRequest, assertions []domain.Assertion) (map[domain.EntityID]domain.Entity, []domain.EntityID, error) {
	entities := map[domain.EntityID]domain.Entity{}
	var order []domain.EntityID

	add := func(id domain.EntityID) error {
		if domain.IsZero(id) {
			return nil
		}
		if _, seen := entities[id]; seen {
			return nil
		}
		entity, err := e.store.GetEntity(ctx, req.Scope.WorkspaceID, id)
		if err != nil {
			if domain.IsCode(err, domain.CodeNotFound) {
				return nil
			}
			return err
		}
		entities[id] = entity
		order = append(order, id)
		return nil
	}

	for _, assertion := range assertions {
		if err := add(assertion.SubjectID); err != nil {
			return nil, nil, err
		}
		if assertion.Object.Kind == domain.ObjectEntity {
			if err := add(assertion.Object.EntityID); err != nil {
				return nil, nil, err
			}
		}
	}
	return entities, order, nil
}

// portableAssertion renders one claim and its evidence.
func (e *Exporter) portableAssertion(ctx context.Context, req ExportRequest, assertion domain.Assertion, sourceNames map[domain.SourceEventID]string) (domain.PortableAssertion, []domain.PortableEvidence, error) {
	portable := domain.PortableAssertion{
		SourceID:       string(assertion.ID),
		SubjectRef:     string(assertion.SubjectID),
		Predicate:      assertion.Predicate.Name,
		Object:         assertion.Object,
		MemoryKind:     assertion.MemoryKind,
		ScopeKey:       assertion.ScopeKey,
		Temporal:       domain.PortableTemporalOf(assertion.Temporal),
		Confidence:     assertion.Confidence,
		Status:         assertion.Status,
		ProvenanceMode: assertion.ProvenanceMode,
		Classification: assertion.Classification,
	}
	if assertion.Object.Kind == domain.ObjectEntity {
		portable.ObjectEntityRef = string(assertion.Object.EntityID)
	}

	// A derived fact carries what it was reasoned from, so a conclusion stays explicable
	// after a move rather than arriving as an unsupported assertion.
	if assertion.DerivationID != nil {
		derivation, err := e.store.GetDerivation(ctx, req.Scope.WorkspaceID, *assertion.DerivationID)
		if err == nil {
			for _, input := range derivation.InputAssertionIDs {
				portable.DerivedFrom = append(portable.DerivedFrom, string(input))
			}
		} else if !domain.IsCode(err, domain.CodeNotFound) {
			return domain.PortableAssertion{}, nil, err
		}
	}

	evidence, err := e.store.ListEvidence(ctx, req.Scope.WorkspaceID, assertion.ID)
	if err != nil {
		return domain.PortableAssertion{}, nil, err
	}

	out := make([]domain.PortableEvidence, 0, len(evidence))
	for _, item := range evidence {
		name, known := sourceNames[item.SourceEventID]
		if !known {
			if event, err := e.store.GetSourceEvent(ctx, req.Scope.WorkspaceID, item.SourceEventID); err == nil {
				if source, err := e.store.GetSource(ctx, req.Scope.WorkspaceID, event.SourceID); err == nil {
					name = source.Name
				}
			}
			sourceNames[item.SourceEventID] = name
		}

		portable.EvidenceRefs = append(portable.EvidenceRefs, string(item.ID))
		out = append(out, domain.PortableEvidence{
			SourceID:      string(item.ID),
			AssertionRef:  string(assertion.ID),
			ExtractedText: item.ExtractedText,
			SourceName:    name,
			Confidence:    item.Confidence,
		})
	}
	return portable, out, nil
}

// packageWriter emits lines and accumulates digests.
type packageWriter struct {
	out     *bufio.Writer
	digest  *domain.PackageDigest
	written int64
}

func (w *packageWriter) header(header domain.PackageHeader) error {
	return w.write(header)
}

func (w *packageWriter) record(kind domain.PackageRecordKind, data any) error {
	const op = "portable.record"

	encoded, err := json.Marshal(data)
	if err != nil {
		return domain.Wrap(err, domain.CodeInternal, op, "cannot encode a "+string(kind)+" record")
	}
	line, err := json.Marshal(domain.PackageRecord{Kind: kind, Data: encoded})
	if err != nil {
		return domain.Wrap(err, domain.CodeInternal, op, "cannot encode a "+string(kind)+" record")
	}

	// The digest covers the exact bytes written, so a reader that recomputes it from the
	// file gets the same answer without having to reproduce this encoder's choices.
	w.digest.Add(kind, line)
	return w.writeLine(line)
}

func (w *packageWriter) finish() (domain.PackageManifest, int64, error) {
	manifest := w.digest.Manifest()
	if err := w.write(manifest); err != nil {
		return domain.PackageManifest{}, 0, err
	}
	if err := w.out.Flush(); err != nil {
		return domain.PackageManifest{}, 0, domain.Wrap(err, domain.CodeInternal,
			"portable.finish", "cannot flush the package")
	}
	return manifest, w.written, nil
}

func (w *packageWriter) write(value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return domain.Wrap(err, domain.CodeInternal, "portable.write", "cannot encode")
	}
	return w.writeLine(encoded)
}

func (w *packageWriter) writeLine(line []byte) error {
	n, err := w.out.Write(append(line, '\n'))
	w.written += int64(n)
	if err != nil {
		return domain.Wrap(err, domain.CodeInternal, "portable.write", "cannot write")
	}
	return nil
}
