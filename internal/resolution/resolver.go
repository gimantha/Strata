// Package resolution decides which identity a mention refers to.
//
// This is the highest-risk component in the system (AGENTS.md section 12). Merging two
// identities that are not the same thing corrupts every fact about both, and the corruption
// is quiet: nothing errors, the graph just becomes wrong. So the resolver is built to
// prefer leaving things separate.
//
// Evidence is used in strength order rather than blended into a single score. An upstream
// primary key settles identity; a name that merely looks similar does not, and no amount of
// similarity is allowed to outvote the absence of a stable key.
package resolution

import (
	"context"
	"log/slog"

	"github.com/gimantha/strata/internal/domain"
)

// Version identifies the resolver's behavior. It is recorded on every decision, so a change
// in matching can be correlated with a change in outcomes.
const Version = 1

// Store is the persistence the resolver needs, declared by its consumer.
type Store interface {
	CreateEntity(ctx context.Context, e domain.Entity) (domain.Entity, error)
	GetEntity(ctx context.Context, ws domain.WorkspaceID, id domain.EntityID) (domain.Entity, error)
	AddAlias(ctx context.Context, alias domain.EntityAlias) error

	UpsertIdentifier(ctx context.Context, id domain.EntityIdentifier) (domain.EntityIdentifier, error)
	FindByIdentifier(ctx context.Context, scope domain.Scope, kind domain.IdentifierKind, namespace, value string) (domain.EntityIdentifier, error)
	FindByExactAlias(ctx context.Context, scope domain.Scope, name string) ([]domain.AliasMatch, error)
	FindBySimilarAlias(ctx context.Context, scope domain.Scope, name string, threshold float64, limit int) ([]domain.AliasMatch, error)
	ListIdentifiersInNamespace(ctx context.Context, scope domain.Scope, kind domain.IdentifierKind, namespace string) ([]domain.EntityIdentifier, error)

	CanonicalEntityID(ctx context.Context, ws domain.WorkspaceID, id domain.EntityID) (domain.EntityID, error)
	RecordResolutionDecision(ctx context.Context, decision domain.ResolutionDecision) (domain.ResolutionDecision, error)
}

// Options tunes candidate generation.
type Options struct {
	// FuzzyCandidateThreshold is the similarity at which a name is worth recording as a
	// candidate for review.
	//
	// There is deliberately no threshold at which a fuzzy match resolves on its own.
	// Trigram similarity cannot separate a typo from a different person: measured on
	// PostgreSQL, "acme corporation" against the transposition "acme corporatoin" scores
	// 0.619, while "alice chen" against the entirely different "alice chan" scores 0.571.
	// Any threshold that catches the typo also merges the two people. The ladder in
	// AGENTS.md section 12.1 calls rung 5 candidate *generation* for this reason;
	// adjudication is a later rung.
	FuzzyCandidateThreshold float64
	// MaxCandidates bounds how many near misses are recorded for one mention.
	MaxCandidates int
	// RequireTypeMatch keeps a person from resolving to an organization with the same
	// name.
	RequireTypeMatch bool
}

// DefaultOptions are the conservative settings.
func DefaultOptions() Options {
	return Options{
		FuzzyCandidateThreshold: 0.55,
		MaxCandidates:           10,
		RequireTypeMatch:        true,
	}
}

func (o Options) withDefaults() Options {
	defaults := DefaultOptions()
	if o.FuzzyCandidateThreshold <= 0 {
		o.FuzzyCandidateThreshold = defaults.FuzzyCandidateThreshold
	}
	if o.MaxCandidates <= 0 {
		o.MaxCandidates = defaults.MaxCandidates
	}
	return o
}

// Resolver walks the resolution ladder.
type Resolver struct {
	store  Store
	opts   Options
	logger *slog.Logger
}

// New builds a resolver.
func New(store Store, opts Options, logger *slog.Logger) *Resolver {
	return &Resolver{store: store, opts: opts.withDefaults(), logger: logger}
}

// Result is the outcome of resolving one mention.
type Result struct {
	EntityID domain.EntityID
	// Created reports whether a new identity was made rather than an existing one matched.
	Created    bool
	Method     domain.ResolutionMethod
	Confidence float64
	Candidates []domain.ScoredCandidate
	Decision   domain.ResolutionDecision
}

// Resolve decides which identity a mention refers to, creating one if nothing matches.
//
// The ladder is walked in order and stops at the first rung that produces an unambiguous
// answer (AGENTS.md section 12.1). A rung that finds several equally plausible identities
// does not fall through to a weaker rung: weaker evidence cannot break a tie that stronger
// evidence could not.
func (r *Resolver) Resolve(ctx context.Context, scope domain.Scope, mention domain.Mention) (Result, error) {
	const op = "resolution.Resolve"

	if err := mention.Validate(); err != nil {
		return Result{}, err
	}
	if domain.IsZero(scope.WorkspaceID) || domain.IsZero(scope.GraphSpaceID) {
		return Result{}, domain.Errorf(domain.CodeInternal, op,
			"scope was not resolved before entity resolution")
	}

	// Rung 1: the upstream system's own identity for this record.
	if mention.SourceID != nil && mention.ExternalID != "" {
		found, err := r.store.FindByIdentifier(ctx, scope, domain.IdentifierSource,
			string(*mention.SourceID), mention.ExternalID)
		switch {
		case err == nil:
			return r.settle(ctx, scope, mention, found.EntityID, domain.MethodSourceIdentifier, 1.0, nil)
		case !domain.IsCode(err, domain.CodeNotFound):
			return Result{}, err
		}
	}

	// Rung 2: a configured business key.
	for _, key := range mention.DomainKeys {
		found, err := r.store.FindByIdentifier(ctx, scope, domain.IdentifierDomain,
			key.Namespace, key.Value)
		switch {
		case err == nil:
			return r.settle(ctx, scope, mention, found.EntityID, domain.MethodDomainKey, 0.99, nil)
		case !domain.IsCode(err, domain.CodeNotFound):
			return Result{}, err
		}
	}

	// A mention carrying a key that matched nothing tells us something: the source
	// considers this a record it has not shown us before. Name matching must not then
	// bind it to an identity that already carries a *different* key in the same
	// namespace, because the source has already said those are two records. Identities
	// with no key of that kind are still fair game - that is how an entity first seen by
	// name later learns its key.
	conflicting, err := r.identitiesWithOtherKeys(ctx, scope, mention)
	if err != nil {
		return Result{}, err
	}

	// Rung 3: an exact match on a known name.
	if mention.Name != "" {
		exact, err := r.store.FindByExactAlias(ctx, scope, mention.Name)
		if err != nil {
			return Result{}, err
		}
		exact = r.filterByType(mention, exact)
		exact = excludeIdentities(exact, conflicting)

		switch len(exact) {
		case 1:
			return r.settle(ctx, scope, mention, exact[0].Entity.ID, domain.MethodExactAlias, 0.9,
				candidatesOf(exact, "exact_alias"))
		case 0:
			// Fall through to fuzzy.
		default:
			// Several identities legitimately share this exact name. Nothing weaker will
			// break that tie, so keep them separate and record the ambiguity.
			return r.keepSeparate(ctx, scope, mention, candidatesOf(exact, "exact_alias"))
		}
	}

	// Rung 5: names that are merely similar.
	//
	// This generates candidates and never decides. The similarity of two names says
	// almost nothing about whether they are the same thing, and the measured overlap
	// between typos and genuinely different people leaves no threshold that separates
	// them. Recording the near misses lets a human merge later; acting on them would
	// merge people who are not the same, silently and irreversibly in practice.
	//
	// Rungs 6 and 7 - embedding and graph-neighbourhood candidates - attach here when
	// phase 6 provides the projections, and rung 8, adjudication of those candidates,
	// above them.
	var fuzzy []domain.AliasMatch
	if mention.Name != "" {
		var err error
		fuzzy, err = r.store.FindBySimilarAlias(ctx, scope, mention.Name,
			r.opts.FuzzyCandidateThreshold, r.opts.MaxCandidates)
		if err != nil {
			return Result{}, err
		}
		fuzzy = r.filterByType(mention, fuzzy)
	}

	// Nothing settled it. Create a new identity, recording any near misses so a human can
	// merge later if they really were the same thing. Creating a duplicate is recoverable;
	// merging two different things is not.
	if len(fuzzy) > 0 {
		return r.keepSeparate(ctx, scope, mention, candidatesOf(fuzzy, "fuzzy_alias"))
	}
	return r.create(ctx, scope, mention, domain.MethodCreated, 1.0, nil)
}

// identitiesWithOtherKeys finds identities already bound to a different key in a namespace
// the mention also carries. The source has told us those are separate records.
func (r *Resolver) identitiesWithOtherKeys(ctx context.Context, scope domain.Scope, mention domain.Mention) (map[domain.EntityID]bool, error) {
	conflicting := map[domain.EntityID]bool{}

	namespaces := make([]struct {
		kind      domain.IdentifierKind
		namespace string
		value     string
	}, 0, len(mention.DomainKeys)+1)

	if mention.SourceID != nil && mention.ExternalID != "" {
		namespaces = append(namespaces, struct {
			kind      domain.IdentifierKind
			namespace string
			value     string
		}{domain.IdentifierSource, string(*mention.SourceID), mention.ExternalID})
	}
	for _, key := range mention.DomainKeys {
		namespaces = append(namespaces, struct {
			kind      domain.IdentifierKind
			namespace string
			value     string
		}{domain.IdentifierDomain, key.Namespace, key.Value})
	}

	for _, ns := range namespaces {
		bound, err := r.store.ListIdentifiersInNamespace(ctx, scope, ns.kind, ns.namespace)
		if err != nil {
			return nil, err
		}
		for _, identifier := range bound {
			if identifier.Value != domain.NormalizeIdentifierValue(ns.value) {
				conflicting[identifier.EntityID] = true
			}
		}
	}
	return conflicting, nil
}

// excludeIdentities drops candidates the source has already told us are other records.
func excludeIdentities(matches []domain.AliasMatch, excluded map[domain.EntityID]bool) []domain.AliasMatch {
	if len(excluded) == 0 {
		return matches
	}
	out := matches[:0]
	for _, match := range matches {
		if !excluded[match.Entity.ID] {
			out = append(out, match)
		}
	}
	return out
}

// filterByType drops candidates of a different type, so a person never resolves to an
// organization that happens to share a name.
func (r *Resolver) filterByType(mention domain.Mention, matches []domain.AliasMatch) []domain.AliasMatch {
	if !r.opts.RequireTypeMatch || mention.EntityType == "" || mention.EntityType == "unknown" {
		return matches
	}
	out := matches[:0]
	for _, match := range matches {
		if match.Entity.EntityType == mention.EntityType || match.Entity.EntityType == "unknown" {
			out = append(out, match)
		}
	}
	return out
}

// settle resolves to an existing identity, following any merge that has happened since.
func (r *Resolver) settle(ctx context.Context, scope domain.Scope, mention domain.Mention, id domain.EntityID, method domain.ResolutionMethod, confidence float64, candidates []domain.ScoredCandidate) (Result, error) {
	canonical, err := r.store.CanonicalEntityID(ctx, scope.WorkspaceID, id)
	if err != nil {
		return Result{}, err
	}

	// Record any new names and keys this mention brought with it, so the next mention has
	// stronger evidence to match on than this one did.
	if err := r.learn(ctx, scope, mention, canonical); err != nil {
		return Result{}, err
	}

	decision, err := r.record(ctx, scope, mention, domain.ResolutionDecision{
		Method:         method,
		ChosenEntityID: canonical,
		Confidence:     confidence,
		Candidates:     candidates,
	})
	if err != nil {
		return Result{}, err
	}
	return Result{
		EntityID:   canonical,
		Method:     method,
		Confidence: confidence,
		Candidates: candidates,
		Decision:   decision,
	}, nil
}

// keepSeparate creates a new identity while recording the ones it might have been.
func (r *Resolver) keepSeparate(ctx context.Context, scope domain.Scope, mention domain.Mention, candidates []domain.ScoredCandidate) (Result, error) {
	if r.logger != nil {
		r.logger.InfoContext(ctx, "ambiguous mention kept separate rather than guessed",
			slog.String("mention", mention.Name),
			slog.Int("candidates", len(candidates)))
	}
	return r.create(ctx, scope, mention, domain.MethodAmbiguous, 0.5, candidates)
}

// create makes a new identity and records what it knows about it.
func (r *Resolver) create(ctx context.Context, scope domain.Scope, mention domain.Mention, method domain.ResolutionMethod, confidence float64, candidates []domain.ScoredCandidate) (Result, error) {
	entityType := mention.EntityType
	if entityType == "" {
		entityType = "unknown"
	}
	name := mention.Name
	if name == "" {
		// A mention identified only by a key still needs a display name; the key is the
		// most honest one available.
		name = mention.ExternalID
		if name == "" && len(mention.DomainKeys) > 0 {
			name = mention.DomainKeys[0].Value
		}
	}

	created, err := r.store.CreateEntity(ctx, domain.Entity{
		WorkspaceID:   scope.WorkspaceID,
		GraphSpaceID:  scope.GraphSpaceID,
		CanonicalName: name,
		EntityType:    entityType,
	})
	if err != nil {
		return Result{}, err
	}
	if err := r.learn(ctx, scope, mention, created.ID); err != nil {
		return Result{}, err
	}

	decision, err := r.record(ctx, scope, mention, domain.ResolutionDecision{
		Method:         method,
		ChosenEntityID: created.ID,
		Confidence:     confidence,
		Candidates:     candidates,
	})
	if err != nil {
		return Result{}, err
	}
	return Result{
		EntityID:   created.ID,
		Created:    true,
		Method:     method,
		Confidence: confidence,
		Candidates: candidates,
		Decision:   decision,
	}, nil
}

// learn records the names and keys a mention carried, so later mentions match on stronger
// evidence. This is how the ladder improves with use: a record seen once by name and later
// by primary key becomes matchable by that key.
func (r *Resolver) learn(ctx context.Context, scope domain.Scope, mention domain.Mention, id domain.EntityID) error {
	for _, alias := range append([]string{mention.Name}, mention.Aliases...) {
		if alias == "" {
			continue
		}
		if err := r.store.AddAlias(ctx, domain.EntityAlias{
			WorkspaceID: scope.WorkspaceID,
			EntityID:    id,
			Alias:       alias,
			Confidence:  1,
		}); err != nil {
			return err
		}
	}

	if mention.SourceID != nil && mention.ExternalID != "" {
		if _, err := r.store.UpsertIdentifier(ctx, domain.EntityIdentifier{
			WorkspaceID:  scope.WorkspaceID,
			GraphSpaceID: scope.GraphSpaceID,
			EntityID:     id,
			Kind:         domain.IdentifierSource,
			Namespace:    string(*mention.SourceID),
			Value:        mention.ExternalID,
			SourceID:     mention.SourceID,
		}); err != nil {
			// A key already bound elsewhere is a genuine upstream conflict. It must not
			// silently rebind, and it must not fail the ingest either: the claim is still
			// good, the identity is just less certain than the key suggested.
			if !domain.IsCode(err, domain.CodeConflict) {
				return err
			}
			if r.logger != nil {
				r.logger.WarnContext(ctx, "source identifier is already bound to another entity",
					slog.String("external_id", mention.ExternalID))
			}
		}
	}

	for _, key := range mention.DomainKeys {
		if _, err := r.store.UpsertIdentifier(ctx, domain.EntityIdentifier{
			WorkspaceID:  scope.WorkspaceID,
			GraphSpaceID: scope.GraphSpaceID,
			EntityID:     id,
			Kind:         domain.IdentifierDomain,
			Namespace:    key.Namespace,
			Value:        key.Value,
		}); err != nil {
			if !domain.IsCode(err, domain.CodeConflict) {
				return err
			}
			if r.logger != nil {
				r.logger.WarnContext(ctx, "domain key is already bound to another entity",
					slog.String("namespace", key.Namespace))
			}
		}
	}
	return nil
}

func (r *Resolver) record(ctx context.Context, scope domain.Scope, mention domain.Mention, decision domain.ResolutionDecision) (domain.ResolutionDecision, error) {
	decision.WorkspaceID = scope.WorkspaceID
	decision.GraphSpaceID = scope.GraphSpaceID
	decision.MentionText = mention.Name
	decision.MentionType = mention.EntityType
	decision.ResolverVersion = Version
	decision.SourceEventID = mention.SourceEventID
	return r.store.RecordResolutionDecision(ctx, decision)
}

// candidatesOf renders matches as recorded candidates.
func candidatesOf(matches []domain.AliasMatch, rung string) []domain.ScoredCandidate {
	out := make([]domain.ScoredCandidate, 0, len(matches))
	for _, match := range matches {
		out = append(out, domain.ScoredCandidate{
			EntityID: match.Entity.ID,
			Name:     match.Entity.CanonicalName,
			Score:    match.Similarity,
			Features: map[string]any{
				"rung":        rung,
				"matched_via": match.Alias,
				"entity_type": match.Entity.EntityType,
			},
		})
	}
	return out
}
