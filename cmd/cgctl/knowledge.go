package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/gimantha/strata/internal/app"
	"github.com/gimantha/strata/internal/domain"
	"github.com/gimantha/strata/internal/knowledge"
	"github.com/gimantha/strata/internal/retrieval"
)

// cmdPredicateDefine sets a predicate's semantics, which is what lets the reconciler tell
// a genuine contradiction from two facts that simply coexist.
func cmdPredicateDefine(ctx context.Context, a *app.App, args []string) error {
	fs := flag.NewFlagSet("predicate define", flag.ContinueOnError)
	workspace := fs.String("workspace", "", "workspace id or slug (required)")
	name := fs.String("name", "", "predicate name, normalized to UPPER_SNAKE (required)")
	description := fs.String("description", "", "what the predicate means")
	functional := fs.Bool("functional", false, "only one value may hold at a time")
	conflictPolicy := fs.String("conflict-policy", "", "coexist, latest_wins, highest_authority, manual")
	temporalPolicy := fs.String("temporal-policy", "", "stateful, event, immutable")
	memoryKind := fs.String("memory-kind", "", "default memory kind for claims using this predicate")
	sensitivity := fs.String("sensitivity", "", "classification floor for claims using this predicate")
	keyID := fs.String("key-id", "", "act as the principal behind this API key id")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *name == "" {
		return errors.New("--name is required")
	}

	principal, ws, err := resolveScopeForWorkspace(ctx, a, *keyID, *workspace, domain.RoleAdmin)
	if err != nil {
		return err
	}

	def := domain.PredicateDefinition{
		WorkspaceID: ws,
		Name:        *name,
		Description: *description,
		Functional:  *functional,
	}
	if *conflictPolicy != "" {
		parsed, err := domain.ParseConflictPolicy(*conflictPolicy)
		if err != nil {
			return err
		}
		def.ConflictPolicy = parsed
	}
	if *temporalPolicy != "" {
		parsed, err := domain.ParseTemporalPolicy(*temporalPolicy)
		if err != nil {
			return err
		}
		def.TemporalPolicy = parsed
	}
	if *memoryKind != "" {
		parsed, err := domain.ParseMemoryKind(*memoryKind)
		if err != nil {
			return err
		}
		def.DefaultMemoryKind = parsed
	}
	if *sensitivity != "" {
		parsed, err := domain.ParseClassification(*sensitivity)
		if err != nil {
			return err
		}
		def.Sensitivity = parsed
	}

	stored, err := a.Ledger.DefinePredicate(ctx, def, principal.ID)
	if err != nil {
		return err
	}
	fmt.Printf("predicate %s defined (version %d, functional=%t, conflict policy %s)\n",
		stored.Name, stored.Version, stored.Functional, stored.ConflictPolicy)
	return nil
}

func cmdPredicateList(ctx context.Context, a *app.App, args []string) error {
	fs := flag.NewFlagSet("predicate ls", flag.ContinueOnError)
	workspace := fs.String("workspace", "", "workspace id or slug (required)")
	keyID := fs.String("key-id", "", "act as the principal behind this API key id")
	if err := fs.Parse(args); err != nil {
		return err
	}

	_, ws, err := resolveScopeForWorkspace(ctx, a, *keyID, *workspace, domain.RoleReader)
	if err != nil {
		return err
	}
	predicates, err := a.Ledger.ListPredicates(ctx, ws)
	if err != nil {
		return err
	}

	tw := newTable("NAME", "STATUS", "VERSION", "FUNCTIONAL", "CONFLICT POLICY", "TEMPORAL")
	for _, p := range predicates {
		row(tw, p.Name, string(p.Status), fmt.Sprint(p.Version), fmt.Sprint(p.Functional),
			string(p.ConflictPolicy), string(p.TemporalPolicy))
	}
	return tw.Flush()
}

func cmdEntityList(ctx context.Context, a *app.App, args []string) error {
	fs := flag.NewFlagSet("entity ls", flag.ContinueOnError)
	graphSpace := fs.String("graph-space", "", "graph space id (required)")
	entityType := fs.String("type", "", "filter by entity type")
	name := fs.String("name", "", "look up by name or alias")
	keyID := fs.String("key-id", "", "act as the principal behind this API key id")
	if err := fs.Parse(args); err != nil {
		return err
	}

	_, scope, err := resolveScopeForGraphSpace(ctx, a, *keyID, *graphSpace, domain.RoleReader)
	if err != nil {
		return err
	}

	var entities []domain.Entity
	if *name != "" {
		entities, err = a.Ledger.FindEntitiesByName(ctx, scope, *name)
	} else {
		entities, err = a.Ledger.ListEntities(ctx, scope, *entityType, 0)
	}
	if err != nil {
		return err
	}

	tw := newTable("ID", "NAME", "TYPE", "CREATED")
	for _, e := range entities {
		row(tw, string(e.ID), e.CanonicalName, e.EntityType, e.CreatedAt.UTC().Format(time.RFC3339))
	}
	return tw.Flush()
}

// cmdAssert records a claim. It exists mostly for demonstration and repair work; normal
// knowledge arrives through extraction in phase 3.
func cmdAssert(ctx context.Context, a *app.App, args []string) error {
	fs := flag.NewFlagSet("assert", flag.ContinueOnError)
	graphSpace := fs.String("graph-space", "", "graph space id (required)")
	eventID := fs.String("source-event", "", "source event this claim came from (required)")
	subject := fs.String("subject", "", "subject entity name (required)")
	subjectType := fs.String("subject-type", "thing", "subject entity type")
	predicate := fs.String("predicate", "", "predicate name (required)")
	objectEntity := fs.String("object-entity", "", "object entity name, for a relation")
	objectText := fs.String("object", "", "literal object value")
	objectKind := fs.String("object-kind", "string", "object kind: string, symbol, integer, decimal, boolean, uri")
	episodeID := fs.String("episode", "", "episode this claim is evidenced by (required)")
	quote := fs.String("quote", "", "short excerpt supporting the claim")
	validFrom := fs.String("valid-from", "", "start of world validity, RFC3339")
	validTo := fs.String("valid-to", "", "end of world validity, RFC3339")
	supersedes := fs.String("supersedes", "", "comma-separated assertion ids this claim corrects")
	scopeKey := fs.String("scope-key", "", "context in which the claim holds")
	memoryKind := fs.String("memory-kind", "",
		"episodic, semantic, procedural, preference, working, or derived")
	activeUntil := fs.String("active-until", "", "when this stops being current context, RFC3339")
	expiresAt := fs.String("expires-at", "", "when this stops being usable at all, RFC3339")
	decayFrom := fs.String("decay-from", "", "when ranking weight starts to fall, RFC3339")
	keyID := fs.String("key-id", "", "act as the principal behind this API key id")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *subject == "" || *predicate == "" || *eventID == "" {
		return errors.New("--subject, --predicate, and --source-event are required")
	}
	if *episodeID == "" {
		return errors.New("--episode is required: a claim must cite the material it came from")
	}

	principal, scope, err := resolveScopeForGraphSpace(ctx, a, *keyID, *graphSpace, domain.RoleWriter)
	if err != nil {
		return err
	}

	claim := knowledge.Claim{
		Subject:   knowledge.EntityRef{Name: *subject, Type: *subjectType},
		Predicate: *predicate,
		ScopeKey:  *scopeKey,
		Evidence: []knowledge.EvidenceInput{{
			EpisodeID:     domain.EpisodeID(*episodeID),
			ExtractedText: *quote,
		}},
	}

	if *memoryKind != "" {
		parsed, err := domain.ParseMemoryKind(*memoryKind)
		if err != nil {
			return err
		}
		claim.MemoryKind = parsed
	}
	// The context clock, distinct from world validity above: these say when the claim is
	// worth surfacing, not when it was true (AGENTS.md section 21.3).
	if claim.ActiveUntil, err = parseOptionalTime(*activeUntil, "--active-until"); err != nil {
		return err
	}
	if claim.ExpiresAt, err = parseOptionalTime(*expiresAt, "--expires-at"); err != nil {
		return err
	}
	if claim.DecayStartsAt, err = parseOptionalTime(*decayFrom, "--decay-from"); err != nil {
		return err
	}

	switch {
	case *objectEntity != "":
		claim.ObjectEntity = &knowledge.EntityRef{Name: *objectEntity}
	case *objectText != "":
		object, err := literalObject(*objectKind, *objectText)
		if err != nil {
			return err
		}
		claim.Object = object
	default:
		return errors.New("--object or --object-entity is required")
	}

	if claim.ValidFrom, err = parseOptionalTime(*validFrom, "--valid-from"); err != nil {
		return err
	}
	if claim.ValidTo, err = parseOptionalTime(*validTo, "--valid-to"); err != nil {
		return err
	}
	for _, id := range splitList(*supersedes) {
		claim.Supersedes = append(claim.Supersedes, domain.AssertionID(id))
	}

	result, err := a.Knowledge.Assert(ctx, knowledge.AssertRequest{
		Scope:         scope,
		Principal:     principal.Ref(),
		SourceEventID: domain.SourceEventID(*eventID),
		Claims:        []knowledge.Claim{claim},
	})
	if err != nil {
		return err
	}

	for _, assertion := range result.Assertions {
		fmt.Printf("assertion %s\n  %s %s %s\n  status: %s\n",
			assertion.ID, assertion.SubjectID, assertion.Predicate.Name,
			assertion.Object.Display(), assertion.Status)
	}
	if result.Duplicates > 0 {
		fmt.Printf("  %d claim(s) already recorded\n", result.Duplicates)
	}
	if len(result.Superseded) > 0 {
		fmt.Printf("  superseded %d earlier claim(s)\n", len(result.Superseded))
	}
	for _, conflict := range result.Conflicts {
		fmt.Printf("  conflict %s recorded: %s\n", conflict.ID, conflict.Reason)
	}
	return nil
}

// cmdAsk queries knowledge, including as of a past instant.
func cmdAsk(ctx context.Context, a *app.App, args []string) error {
	fs := flag.NewFlagSet("ask", flag.ContinueOnError)
	graphSpace := fs.String("graph-space", "", "graph space id (required)")
	predicate := fs.String("predicate", "", "filter by predicate")
	subject := fs.String("subject", "", "filter by subject entity id")
	validAt := fs.String("valid-at", "", "what held in the world at this instant, RFC3339")
	knownAt := fs.String("known-at", "", "what the system believed at this instant, RFC3339")
	activeAt := fs.String("active-at", "", "what was active context at this instant, RFC3339")
	includeSuperseded := fs.Bool("include-superseded", false, "include replaced beliefs")
	limit := fs.Int("limit", 50, "maximum results")
	keyID := fs.String("key-id", "", "act as the principal behind this API key id")
	if err := fs.Parse(args); err != nil {
		return err
	}

	_, scope, err := resolveScopeForGraphSpace(ctx, a, *keyID, *graphSpace, domain.RoleReader)
	if err != nil {
		return err
	}

	query := domain.AssertionQuery{
		Scope:             scope,
		IncludeSuperseded: *includeSuperseded,
		Limit:             *limit,
	}
	if *predicate != "" {
		query.Predicates = []string{*predicate}
	}
	if *subject != "" {
		query.SubjectIDs = []domain.EntityID{domain.EntityID(*subject)}
	}
	if query.ValidAt, err = parseOptionalTime(*validAt, "--valid-at"); err != nil {
		return err
	}
	if query.KnownAt, err = parseOptionalTime(*knownAt, "--known-at"); err != nil {
		return err
	}
	if query.ActiveAt, err = parseOptionalTime(*activeAt, "--active-at"); err != nil {
		return err
	}

	found, err := a.Knowledge.Query(ctx, query)
	if err != nil {
		return err
	}

	tw := newTable("ASSERTION", "SUBJECT", "PREDICATE", "OBJECT", "VALID FROM", "VALID TO", "STATUS")
	for _, x := range found {
		row(tw, string(x.ID), string(x.SubjectID), x.Predicate.Name, x.Object.Display(),
			formatOptional(x.Temporal.ValidFrom), formatOptional(x.Temporal.ValidTo), string(x.Status))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Printf("\n%d assertion(s)\n", len(found))
	return nil
}

// cmdProvenance walks a claim back to the bytes that support it.
func cmdProvenance(ctx context.Context, a *app.App, args []string) error {
	fs := flag.NewFlagSet("provenance", flag.ContinueOnError)
	assertionID := fs.String("assertion", "", "assertion id (required)")
	keyID := fs.String("key-id", "", "act as the principal behind this API key id")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *assertionID == "" {
		return errors.New("--assertion is required")
	}

	principal, err := resolvePrincipal(ctx, a, *keyID)
	if err != nil {
		return err
	}
	allowed := make([]domain.WorkspaceID, 0, len(principal.Grants))
	for _, grant := range principal.Grants {
		allowed = append(allowed, grant.WorkspaceID)
	}
	ws, err := a.Ledger.ResolveAssertionWorkspace(ctx, domain.AssertionID(*assertionID), allowed)
	if err != nil {
		return err
	}

	chain, err := a.Knowledge.Provenance(ctx, ws, domain.AssertionID(*assertionID))
	if err != nil {
		return err
	}

	// Resolve an entity-valued object to its name: an identifier tells an operator
	// nothing about what the claim says.
	object := chain.Assertion.Object.Display()
	if chain.Assertion.Object.Kind == domain.ObjectEntity {
		if target, err := a.Ledger.GetEntity(ctx, ws, chain.Assertion.Object.EntityID); err == nil {
			object = target.CanonicalName
		}
	}

	fmt.Printf("assertion %s\n  %s %s %s\n  status: %s, recorded %s\n",
		chain.Assertion.ID, chain.Subject.CanonicalName, chain.Assertion.Predicate.Name,
		object, chain.Assertion.Status,
		chain.Assertion.Temporal.RecordedAt.UTC().Format(time.RFC3339))

	for i, link := range chain.Links {
		fmt.Printf("\nevidence %d\n", i+1)
		if link.Evidence.ExtractedText != "" {
			fmt.Printf("  quote:        %q\n", link.Evidence.ExtractedText)
		}
		if link.Chunk != nil {
			fmt.Printf("  chunk:        %s (chars %d-%d)\n",
				link.Chunk.ID, link.Chunk.CharStart, link.Chunk.CharEnd)
		}
		fmt.Printf("  episode:      %s (sequence %d)\n", link.Episode.ID, link.Episode.Sequence)
		fmt.Printf("  artifact:     %s sha256:%s\n", link.Artifact.ID, link.Artifact.ContentHash)
		fmt.Printf("  source event: %s recorded %s\n",
			link.SourceEvent.ID, link.SourceEvent.RecordedAt.UTC().Format(time.RFC3339))
		fmt.Printf("  source:       %s (%s, trust %s)\n",
			link.Source.Name, link.Source.Kind, link.Source.TrustLevel)
	}

	if chain.Derivation != nil {
		fmt.Printf("\nderived by %s", chain.Derivation.Method)
		if chain.Derivation.RuleName != "" {
			fmt.Printf(" rule %s v%s", chain.Derivation.RuleName, chain.Derivation.RuleVersion)
		}
		fmt.Println()
		for _, support := range chain.Supports {
			fmt.Printf("  from assertion %s: %s %s %s\n", support.ID, support.SubjectID,
				support.Predicate.Name, support.Object.Display())
		}
	}
	return nil
}

func cmdConflicts(ctx context.Context, a *app.App, args []string) error {
	fs := flag.NewFlagSet("conflicts", flag.ContinueOnError)
	graphSpace := fs.String("graph-space", "", "graph space id (required)")
	all := fs.Bool("all", false, "include resolved conflicts")
	keyID := fs.String("key-id", "", "act as the principal behind this API key id")
	if err := fs.Parse(args); err != nil {
		return err
	}

	_, scope, err := resolveScopeForGraphSpace(ctx, a, *keyID, *graphSpace, domain.RoleReader)
	if err != nil {
		return err
	}
	conflicts, err := a.Ledger.ListConflictSets(ctx, scope, !*all, 0)
	if err != nil {
		return err
	}

	tw := newTable("ID", "SUBJECT", "PREDICATE", "RESOLUTION", "REASON")
	for _, c := range conflicts {
		row(tw, string(c.ID), string(c.SubjectID), c.Predicate, string(c.Resolution),
			truncate(c.Reason, 50))
	}
	return tw.Flush()
}

// resolveScopeForWorkspace resolves a principal and an authorized workspace.
func resolveScopeForWorkspace(ctx context.Context, a *app.App, keyID, workspace string, need domain.Role) (domain.Principal, domain.WorkspaceID, error) {
	principal, err := resolvePrincipal(ctx, a, keyID)
	if err != nil {
		return domain.Principal{}, "", err
	}
	ws, err := resolveWorkspace(ctx, a, workspace)
	if err != nil {
		return domain.Principal{}, "", err
	}
	if err := a.Identity.AuthorizeWorkspace(ctx, principal, ws, need); err != nil {
		return domain.Principal{}, "", err
	}
	return principal, ws, nil
}

// resolveScopeForGraphSpace resolves a principal and an authorized graph-space scope.
func resolveScopeForGraphSpace(ctx context.Context, a *app.App, keyID, graphSpace string, need domain.Role) (domain.Principal, domain.Scope, error) {
	principal, err := resolvePrincipal(ctx, a, keyID)
	if err != nil {
		return domain.Principal{}, domain.Scope{}, err
	}
	if graphSpace == "" {
		return domain.Principal{}, domain.Scope{}, errors.New("--graph-space is required")
	}
	scope, err := a.Identity.ResolveGraphSpace(ctx, principal, domain.GraphSpaceID(graphSpace), need)
	if err != nil {
		return domain.Principal{}, domain.Scope{}, err
	}
	return principal, scope, nil
}

// literalObject builds a typed object from CLI flags.
func literalObject(kind, value string) (domain.AssertionObject, error) {
	parsed, err := domain.ParseObjectKind(kind)
	if err != nil {
		return domain.AssertionObject{}, err
	}
	switch parsed {
	case domain.ObjectString:
		return domain.ObjectOfString(value), nil
	case domain.ObjectSymbol:
		return domain.ObjectOfSymbol(value), nil
	case domain.ObjectURI:
		return domain.ObjectOfURI(value), nil
	case domain.ObjectDecimal:
		return domain.ObjectOfDecimal(value), nil
	case domain.ObjectBoolean:
		return domain.ObjectOfBool(strings.EqualFold(value, "true")), nil
	case domain.ObjectInteger:
		var n int64
		if _, err := fmt.Sscanf(value, "%d", &n); err != nil {
			return domain.AssertionObject{}, fmt.Errorf("--object %q is not an integer", value)
		}
		return domain.ObjectOfInteger(n), nil
	default:
		return domain.AssertionObject{}, fmt.Errorf(
			"object kind %q is not supported from the command line; use the API", kind)
	}
}

func parseOptionalTime(value, flagName string) (*time.Time, error) {
	if value == "" {
		return nil, nil
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return nil, fmt.Errorf("%s must be an RFC3339 timestamp: %w", flagName, err)
	}
	utc := parsed.UTC()
	return &utc, nil
}

// formatOptional renders an instant in UTC. Internal time is always UTC, so displaying
// a local offset would misrepresent what is stored (AGENTS.md section 34).
func formatOptional(t *time.Time) string {
	if t == nil {
		return "-"
	}
	return t.UTC().Format(time.RFC3339)
}

func splitList(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if trimmed := strings.TrimSpace(p); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// cmdEntityMerge redirects one identity into another. Nothing is deleted, so the operation
// can be undone with "entity split".
func cmdEntityMerge(ctx context.Context, a *app.App, args []string) error {
	fs := flag.NewFlagSet("entity merge", flag.ContinueOnError)
	graphSpace := fs.String("graph-space", "", "graph space id (required)")
	from := fs.String("from", "", "entity id to redirect (required)")
	into := fs.String("into", "", "entity id to keep (required)")
	reason := fs.String("reason", "", "why these are the same thing (required)")
	keyID := fs.String("key-id", "", "act as the principal behind this API key id")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *from == "" || *into == "" {
		return errors.New("--from and --into are required")
	}
	if *reason == "" {
		return errors.New("--reason is required: a wrong merge corrupts every fact about both identities")
	}

	principal, scope, err := resolveScopeForGraphSpace(ctx, a, *keyID, *graphSpace, domain.RoleAdmin)
	if err != nil {
		return err
	}

	decision, err := a.Ledger.MergeEntities(ctx, scope.WorkspaceID,
		domain.EntityID(*from), domain.EntityID(*into), domain.ResolutionDecision{
			WorkspaceID:  scope.WorkspaceID,
			GraphSpaceID: scope.GraphSpaceID,
			Confidence:   1,
			ActorID:      principal.ID,
			Reason:       *reason,
		})
	if err != nil {
		return err
	}
	fmt.Printf("merged %s into %s\n  decision %s (reversible with: cgctl entity split --entity %s)\n",
		*from, decision.ChosenEntityID, decision.ID, *from)
	return nil
}

// cmdEntitySplit undoes a merge.
func cmdEntitySplit(ctx context.Context, a *app.App, args []string) error {
	fs := flag.NewFlagSet("entity split", flag.ContinueOnError)
	entityID := fs.String("entity", "", "entity id to separate again (required)")
	reason := fs.String("reason", "", "why the merge was wrong (required)")
	keyID := fs.String("key-id", "", "act as the principal behind this API key id")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *entityID == "" || *reason == "" {
		return errors.New("--entity and --reason are required")
	}

	principal, err := resolvePrincipal(ctx, a, *keyID)
	if err != nil {
		return err
	}
	allowed := make([]domain.WorkspaceID, 0, len(principal.Grants))
	for _, grant := range principal.Grants {
		allowed = append(allowed, grant.WorkspaceID)
	}
	ws, err := a.Ledger.ResolveEntityWorkspace(ctx, domain.EntityID(*entityID), allowed)
	if err != nil {
		return err
	}
	if err := a.Identity.AuthorizeWorkspace(ctx, principal, ws, domain.RoleAdmin); err != nil {
		return err
	}

	entity, err := a.Ledger.GetEntity(ctx, ws, domain.EntityID(*entityID))
	if err != nil {
		return err
	}
	decision, err := a.Ledger.SplitEntity(ctx, ws, domain.EntityID(*entityID),
		domain.ResolutionDecision{
			WorkspaceID:  ws,
			GraphSpaceID: entity.GraphSpaceID,
			Confidence:   1,
			ActorID:      principal.ID,
			Reason:       *reason,
		})
	if err != nil {
		return err
	}
	fmt.Printf("%s stands on its own again (decision %s)\n", *entityID, decision.ID)
	return nil
}

// cmdEntityIdentity shows how an identity relates to others.
func cmdEntityIdentity(ctx context.Context, a *app.App, args []string) error {
	fs := flag.NewFlagSet("entity identity", flag.ContinueOnError)
	entityID := fs.String("entity", "", "entity id (required)")
	keyID := fs.String("key-id", "", "act as the principal behind this API key id")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *entityID == "" {
		return errors.New("--entity is required")
	}

	principal, err := resolvePrincipal(ctx, a, *keyID)
	if err != nil {
		return err
	}
	allowed := make([]domain.WorkspaceID, 0, len(principal.Grants))
	for _, grant := range principal.Grants {
		allowed = append(allowed, grant.WorkspaceID)
	}
	id := domain.EntityID(*entityID)
	ws, err := a.Ledger.ResolveEntityWorkspace(ctx, id, allowed)
	if err != nil {
		return err
	}

	entity, err := a.Ledger.GetEntity(ctx, ws, id)
	if err != nil {
		return err
	}
	canonical, err := a.Ledger.CanonicalEntityID(ctx, ws, id)
	if err != nil {
		return err
	}
	cluster, err := a.Ledger.IdentityCluster(ctx, ws, id)
	if err != nil {
		return err
	}
	identifiers, err := a.Ledger.ListIdentifiers(ctx, ws, id)
	if err != nil {
		return err
	}
	aliases, err := a.Ledger.ListAliases(ctx, ws, id)
	if err != nil {
		return err
	}

	fmt.Printf("entity %s\n  name: %s (%s)\n", entity.ID, entity.CanonicalName, entity.EntityType)
	if canonical != id {
		fmt.Printf("  merged into: %s\n", canonical)
	}
	if len(cluster) > 1 {
		fmt.Printf("  identity cluster (%d):\n", len(cluster))
		for _, member := range cluster {
			marker := " "
			if member == canonical {
				marker = "*"
			}
			fmt.Printf("    %s %s\n", marker, member)
		}
	}
	if len(identifiers) > 0 {
		fmt.Println("  stable keys:")
		for _, identifier := range identifiers {
			fmt.Printf("    %s %s=%s\n", identifier.Kind, identifier.Namespace, identifier.Value)
		}
	}
	if len(aliases) > 0 {
		names := make([]string, 0, len(aliases))
		for _, alias := range aliases {
			names = append(names, alias.Alias)
		}
		fmt.Printf("  known names: %s\n", strings.Join(names, ", "))
	}
	return nil
}

// cmdResolutions shows the decision ledger, defaulting to the entries worth reviewing.
func cmdResolutions(ctx context.Context, a *app.App, args []string) error {
	fs := flag.NewFlagSet("resolutions", flag.ContinueOnError)
	graphSpace := fs.String("graph-space", "", "graph space id (required)")
	all := fs.Bool("all", false, "include routine automatic resolutions")
	limit := fs.Int("limit", 50, "maximum rows")
	keyID := fs.String("key-id", "", "act as the principal behind this API key id")
	if err := fs.Parse(args); err != nil {
		return err
	}

	_, scope, err := resolveScopeForGraphSpace(ctx, a, *keyID, *graphSpace, domain.RoleReader)
	if err != nil {
		return err
	}
	decisions, err := a.Ledger.ListResolutionDecisions(ctx, scope, !*all, *limit)
	if err != nil {
		return err
	}

	tw := newTable("DECISION", "MENTION", "METHOD", "CHOSEN", "CANDIDATES", "REASON")
	for _, decision := range decisions {
		reason := decision.Reason
		if decision.RevertedAt != nil {
			reason = "[reverted] " + reason
		}
		row(tw, string(decision.ID), truncate(decision.MentionText, 24), string(decision.Method),
			string(decision.ChosenEntityID), fmt.Sprint(len(decision.Candidates)),
			truncate(reason, 40))
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	fmt.Printf("\n%d decision(s)\n", len(decisions))
	if !*all {
		fmt.Println("showing ambiguous outcomes and human overrides; pass --all for every resolution")
	}
	return nil
}

// cmdProjectionsRebuild drops a workspace's projections and replays them from the ledger.
//
// This is safe by construction: projections hold no history of their own, so the worst a
// rebuild can cost is the time and the embedding spend.
func cmdProjectionsRebuild(ctx context.Context, a *app.App, args []string) error {
	fs := flag.NewFlagSet("projections rebuild", flag.ContinueOnError)
	workspace := fs.String("workspace", "", "workspace id or slug (required)")
	keyID := fs.String("key-id", "", "act as the principal behind this API key id")
	if err := fs.Parse(args); err != nil {
		return err
	}

	_, ws, err := resolveScopeForWorkspace(ctx, a, *keyID, *workspace, domain.RoleAdmin)
	if err != nil {
		return err
	}

	fmt.Println("dropping projections and replaying from the ledger...")
	stats, err := a.Projector.Rebuild(ctx, ws)
	if err != nil {
		return err
	}

	fmt.Printf("replayed %d event(s)\n  lexical records: %d\n  vectors: %d (%d embedded, %d reused)\n  graph edges: %d\n",
		stats.Events, stats.Lexical, stats.Vectors, stats.Embedded, stats.Reused, stats.Edges)
	return nil
}

// cmdProjectionsStatus reports how far each projection has consumed the ledger.
func cmdProjectionsStatus(ctx context.Context, a *app.App, args []string) error {
	fs := flag.NewFlagSet("projections status", flag.ContinueOnError)
	workspace := fs.String("workspace", "", "workspace id or slug (required)")
	keyID := fs.String("key-id", "", "act as the principal behind this API key id")
	if err := fs.Parse(args); err != nil {
		return err
	}

	_, ws, err := resolveScopeForWorkspace(ctx, a, *keyID, *workspace, domain.RoleReader)
	if err != nil {
		return err
	}

	checkpoints, err := a.Ledger.ListCheckpoints(ctx, ws)
	if err != nil {
		return err
	}
	counts, err := a.Ledger.CountProjected(ctx, ws)
	if err != nil {
		return err
	}

	tw := newTable("PROJECTION", "RECORDS", "CONSUMED THROUGH", "LAST REBUILD", "ERROR")
	for _, checkpoint := range checkpoints {
		row(tw, checkpoint.Projection, fmt.Sprint(counts[checkpoint.Projection]),
			formatOptional(checkpoint.LastRecordedAt), formatOptional(checkpoint.RebuiltAt),
			truncate(checkpoint.LastError, 40))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if len(checkpoints) == 0 {
		fmt.Println("no projections have run yet for this workspace")
	}
	return nil
}

// cmdSearch runs hybrid retrieval.
func cmdSearch(ctx context.Context, a *app.App, args []string) error {
	fs := flag.NewFlagSet("search", flag.ContinueOnError)
	graphSpace := fs.String("graph-space", "", "graph space id (required)")
	text := fs.String("query", "", "what to search for (required)")
	modes := fs.String("modes", "", "comma-separated: lexical, exact, vector, entity, graph; empty lets the planner decide")
	validAt := fs.String("valid-at", "", "only what held at this instant, RFC3339")
	surfaces := fs.String("surfaces", "", "comma-separated: chunk, episode, entity, assertion")
	limit := fs.Int("limit", 10, "maximum results")
	explain := fs.Bool("explain", false, "show the plan and the signals behind each score")
	keyID := fs.String("key-id", "", "act as the principal behind this API key id")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *text == "" {
		return errors.New("--query is required")
	}

	principal, scope, err := resolveScopeForGraphSpace(ctx, a, *keyID, *graphSpace, domain.RoleReader)
	if err != nil {
		return err
	}

	req := domain.QueryRequest{
		Scope:     scope,
		Query:     *text,
		Principal: principal.Ref(),
		Limit:     *limit,
		Explain:   *explain,
	}
	for _, mode := range splitList(*modes) {
		parsed, err := domain.ParseRetrievalMode(mode)
		if err != nil {
			return err
		}
		req.Modes = append(req.Modes, parsed)
	}
	for _, surface := range splitList(*surfaces) {
		parsed, err := domain.ParseSurface(surface)
		if err != nil {
			return err
		}
		req.Filters.Surfaces = append(req.Filters.Surfaces, parsed)
	}
	if req.Temporal.ValidAt, err = parseOptionalTime(*validAt, "--valid-at"); err != nil {
		return err
	}

	result, err := a.Retriever.Query(ctx, req)
	if err != nil {
		return err
	}

	tw := newTable("SCORE", "SURFACE", "FOUND BY", "CONTENT")
	for _, item := range result.Items {
		found := make([]string, 0, len(item.FoundBy))
		for _, mode := range item.FoundBy {
			found = append(found, string(mode))
		}
		row(tw, fmt.Sprintf("%.5f", item.Score), string(item.Surface),
			strings.Join(found, "+"),
			truncate(strings.ReplaceAll(item.Content, "\n", " "), 56))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Printf("\n%d result(s) from %d candidate(s)\n", len(result.Items), result.Total)

	if result.Plan != nil {
		fmt.Println("\nplan:")
		for _, line := range retrieval.Explain(*result.Plan) {
			fmt.Println("  " + line)
		}
		for _, item := range result.Items {
			if item.Path != nil {
				fmt.Printf("  %s reached via %s at depth %d\n",
					truncate(item.Content, 30), item.Path.ViaPredicate, item.Path.Depth)
			}
		}
	}
	return nil
}

// cmdContext assembles a prompt-ready context block.
func cmdContext(ctx context.Context, a *app.App, args []string) error {
	fs := flag.NewFlagSet("context", flag.ContinueOnError)
	graphSpace := fs.String("graph-space", "", "graph space id (required)")
	text := fs.String("query", "", "the question the context should answer (required)")
	budget := fs.Int("budget", domain.DefaultTokenBudget, "token budget for the whole block")
	maxItems := fs.Int("max-items", 0, "maximum items to select")
	sections := fs.String("sections", "", "comma-separated: facts, history, graph, excerpts, conflicts")
	validAt := fs.String("valid-at", "", "assemble what held at this instant, RFC3339")
	explain := fs.Bool("explain", false, "show the budget, the selection signals, and what was dropped")
	keyID := fs.String("key-id", "", "act as the principal behind this API key id")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *text == "" {
		return errors.New("--query is required")
	}

	principal, scope, err := resolveScopeForGraphSpace(ctx, a, *keyID, *graphSpace, domain.RoleReader)
	if err != nil {
		return err
	}

	req := domain.ContextRequest{
		Scope:       scope,
		Query:       *text,
		Principal:   principal.Ref(),
		TokenBudget: *budget,
		MaxItems:    *maxItems,
		Explain:     *explain,
	}
	for _, section := range splitList(*sections) {
		parsed, err := domain.ParseContextSection(section)
		if err != nil {
			return err
		}
		req.Sections = append(req.Sections, parsed)
	}
	if req.Temporal.ValidAt, err = parseOptionalTime(*validAt, "--valid-at"); err != nil {
		return err
	}

	block, err := a.Assembler.Assemble(ctx, req)
	if err != nil {
		return err
	}

	fmt.Println(block.Text)
	fmt.Printf("\n%d item(s), %d of %d tokens (%d scaffolding, estimator %s ±%.0f%%)\n",
		len(block.Items), block.Budget.Used, block.Budget.Limit, block.Budget.Scaffolding,
		block.Budget.Estimator, block.Budget.Tolerance*100)

	if !*explain {
		return nil
	}

	if len(block.Budget.BySection) > 0 {
		tw := newTable("SECTION", "TOKENS")
		for _, section := range domain.ContextSections() {
			if tokens := block.Budget.BySection[section]; tokens > 0 {
				row(tw, string(section), fmt.Sprintf("%d", tokens))
			}
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}

	if len(block.Dropped) > 0 {
		fmt.Printf("\ndropped %d candidate(s):\n", len(block.Dropped))
		dw := newTable("REASON", "SURFACE", "DETAIL")
		for _, dropped := range block.Dropped {
			row(dw, string(dropped.Reason), string(dropped.Surface), truncate(dropped.Detail, 52))
		}
		if err := dw.Flush(); err != nil {
			return err
		}
	}
	return nil
}
