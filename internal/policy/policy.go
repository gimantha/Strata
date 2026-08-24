// Package policy evaluates attribute-based access decisions and records them
// (AGENTS.md section 22).
//
// The rule this package exists to enforce is section 22.4: never retrieve unauthorized data
// into application memory and hide it afterwards. So a decision is not a boolean — it is a
// boolean plus the narrowing every query must apply, and the services that read data are
// expected to push that narrowing into their SQL rather than filter results.
//
// Evaluation itself lives in the domain, where it is a pure function of a policy set and a
// request. This package supplies the parts that need the world: loading the workspace's
// active policy, folding in a principal's clearance, caching, and writing the audit record.
package policy

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/gimantha/strata/internal/domain"
)

// Store is the persistence surface, declared by its consumer.
type Store interface {
	ActivePolicySet(ctx context.Context, ws domain.WorkspaceID) (domain.PolicySet, error)
	CreatePolicySet(ctx context.Context, set domain.PolicySet) (domain.PolicySet, error)
	GetPolicySet(ctx context.Context, ws domain.WorkspaceID, id domain.PolicySetID) (domain.PolicySet, error)
	ListPolicySets(ctx context.Context, ws domain.WorkspaceID, limit int) ([]domain.PolicySet, error)
	GrantClearance(ctx context.Context, ws domain.WorkspaceID, principal domain.PrincipalID) (domain.Classification, error)
	SetGrantClearance(ctx context.Context, ws domain.WorkspaceID, principal domain.PrincipalID, clearance domain.Classification) error
}

// Auditor records security-relevant decisions (AGENTS.md section 22.6).
type Auditor interface {
	Record(ctx context.Context, entry AuditEntry) error
}

// AuditEntry is one recorded decision.
type AuditEntry struct {
	Scope     domain.Scope
	Principal domain.PrincipalRef
	Action    domain.PolicyAction
	Purpose   string
	Allowed   bool
	Rule      string
	Reason    string
	Version   int
	Target    string
	Detail    map[string]any
}

// Options configure the service.
type Options struct {
	// CacheTTL bounds how stale a cached policy may be. Policy is read on every request
	// and changed rarely, so caching is worth it — but a revocation that takes effect
	// "eventually" is not a revocation, so the window is short and stated.
	CacheTTL time.Duration
	// Residency is where this deployment serves from, for data-residency rules.
	Residency string
	Clock     func() time.Time
}

// DefaultCacheTTL is how long an active policy set is reused.
const DefaultCacheTTL = 5 * time.Second

// Service loads policy and answers access questions.
type Service struct {
	store   Store
	auditor Auditor
	opts    Options
	logger  *slog.Logger

	mu    sync.RWMutex
	cache map[domain.WorkspaceID]cached
}

type cached struct {
	set       domain.PolicySet
	expiresAt time.Time
}

func New(store Store, auditor Auditor, opts Options, logger *slog.Logger) *Service {
	if opts.CacheTTL <= 0 {
		opts.CacheTTL = DefaultCacheTTL
	}
	if opts.Clock == nil {
		opts.Clock = func() time.Time { return time.Now().UTC() }
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Service{
		store: store, auditor: auditor, opts: opts, logger: logger,
		cache: map[domain.WorkspaceID]cached{},
	}
}

// Authorize answers one access question and records it.
//
// Every read path calls this before touching data, and applies the returned filters to its
// queries. Returning filters rather than post-filtering results is the whole design: a
// principal cleared for internal material must never have restricted rows in memory, not
// even briefly, not even to count them.
func (s *Service) Authorize(ctx context.Context, req domain.AccessRequest) (domain.Decision, error) {
	set, err := s.activeSet(ctx, req.Scope.WorkspaceID)
	if err != nil {
		return domain.Decision{}, err
	}

	if req.At.IsZero() {
		req.At = s.opts.Clock()
	}
	if req.Residency == "" {
		req.Residency = s.opts.Residency
	}

	decision := set.Evaluate(req)

	// A grant's clearance can only lower the ceiling, never raise it. Otherwise granting
	// somebody access to a workspace would quietly hand them everything in it, and the
	// policy would be advice.
	clearance, err := s.store.GrantClearance(ctx, req.Scope.WorkspaceID, req.Principal.ID)
	if err != nil {
		return domain.Decision{}, err
	}
	if clearance != "" && decision.Allowed {
		decision.Filters.MaxClassification = domain.LeastPermissive(
			decision.Filters.MaxClassification, clearance)
	}

	s.audit(ctx, req, decision)
	return decision, nil
}

// AuthorizeRead is the common case.
func (s *Service) AuthorizeRead(ctx context.Context, principal domain.Principal, scope domain.Scope, purpose string) (domain.Decision, error) {
	return s.Authorize(ctx, domain.AccessRequest{
		Principal: principal, Action: domain.ActionRead, Scope: scope, Purpose: purpose,
	})
}

// audit records the decision, refusals included.
//
// A refused request is the one most worth having in the log, so failure to write an audit
// row must not be swallowed silently — but it also must not turn a correct refusal into an
// error the caller might retry into a different outcome.
func (s *Service) audit(ctx context.Context, req domain.AccessRequest, decision domain.Decision) {
	if s.auditor == nil {
		return
	}
	entry := AuditEntry{
		Scope:     req.Scope,
		Principal: req.Principal.Ref(),
		Action:    req.Action,
		Purpose:   req.Purpose,
		Allowed:   decision.Allowed,
		Rule:      decision.Rule,
		Reason:    decision.Reason,
		Version:   decision.PolicyVersion,
	}
	if decision.Filters.MaxClassification != "" {
		entry.Detail = map[string]any{
			"max_classification": string(decision.Filters.MaxClassification),
		}
	}
	if err := s.auditor.Record(ctx, entry); err != nil {
		s.logger.ErrorContext(ctx, "cannot write a security audit record",
			slog.String("action", string(req.Action)),
			slog.Bool("allowed", decision.Allowed),
			slog.String("error", err.Error()))
	}
}

// activeSet returns the workspace's policy, cached briefly.
func (s *Service) activeSet(ctx context.Context, ws domain.WorkspaceID) (domain.PolicySet, error) {
	now := s.opts.Clock()

	s.mu.RLock()
	entry, ok := s.cache[ws]
	s.mu.RUnlock()
	if ok && now.Before(entry.expiresAt) {
		return entry.set, nil
	}

	set, err := s.store.ActivePolicySet(ctx, ws)
	if err != nil {
		return domain.PolicySet{}, err
	}

	s.mu.Lock()
	s.cache[ws] = cached{set: set, expiresAt: now.Add(s.opts.CacheTTL)}
	s.mu.Unlock()
	return set, nil
}

// invalidate drops a workspace's cached policy, so a change takes effect at once for the
// process that made it rather than after the TTL.
func (s *Service) invalidate(ws domain.WorkspaceID) {
	s.mu.Lock()
	delete(s.cache, ws)
	s.mu.Unlock()
}

// DefineRequest creates a new policy version.
type DefineRequest struct {
	Scope     domain.Scope
	Principal domain.PrincipalRef

	Name             string
	Notes            string
	DefaultClearance domain.Classification
	Rules            []domain.PolicyRule
	Activate         bool
}

// Define appends an immutable policy version.
func (s *Service) Define(ctx context.Context, req DefineRequest) (domain.PolicySet, error) {
	set := domain.PolicySet{
		WorkspaceID:      req.Scope.WorkspaceID,
		Name:             req.Name,
		Notes:            req.Notes,
		DefaultClearance: req.DefaultClearance,
		Rules:            req.Rules,
		Active:           req.Activate,
		CreatedBy:        req.Principal,
	}
	if err := set.Validate(); err != nil {
		return domain.PolicySet{}, err
	}

	created, err := s.store.CreatePolicySet(ctx, set)
	if err != nil {
		return domain.PolicySet{}, err
	}
	s.invalidate(req.Scope.WorkspaceID)

	if s.auditor != nil {
		// A policy change is itself a security-sensitive operation
		// (AGENTS.md section 22.6).
		_ = s.auditor.Record(ctx, AuditEntry{
			Scope: req.Scope, Principal: req.Principal, Action: domain.ActionAdmin,
			Allowed: true, Reason: "policy set defined", Version: created.Version,
			Target: string(created.ID),
			Detail: map[string]any{
				"rules": len(created.Rules), "active": created.Active,
				"default_clearance": string(created.DefaultClearance),
			},
		})
	}

	s.logger.InfoContext(ctx, "policy set defined",
		slog.Int("version", created.Version),
		slog.String("name", created.Name),
		slog.Int("rules", len(created.Rules)),
		slog.Bool("active", created.Active))
	return created, nil
}

// Active returns the workspace's current policy.
func (s *Service) Active(ctx context.Context, ws domain.WorkspaceID) (domain.PolicySet, error) {
	return s.activeSet(ctx, ws)
}

// List returns the policy history.
func (s *Service) List(ctx context.Context, ws domain.WorkspaceID, limit int) ([]domain.PolicySet, error) {
	return s.store.ListPolicySets(ctx, ws, limit)
}

// SetClearance records a principal's clearance ceiling in one workspace.
func (s *Service) SetClearance(ctx context.Context, scope domain.Scope, actor domain.PrincipalRef, principal domain.PrincipalID, clearance domain.Classification) error {
	if err := s.store.SetGrantClearance(ctx, scope.WorkspaceID, principal, clearance); err != nil {
		return err
	}
	if s.auditor != nil {
		_ = s.auditor.Record(ctx, AuditEntry{
			Scope: scope, Principal: actor, Action: domain.ActionAdmin, Allowed: true,
			Reason: "clearance changed", Target: string(principal),
			Detail: map[string]any{"max_classification": string(clearance)},
		})
	}
	return nil
}

// Explain evaluates a request without recording it, for "why can this principal see that".
//
// Separate from Authorize because an operator asking a hypothetical should not fill the
// audit log with decisions nobody acted on.
func (s *Service) Explain(ctx context.Context, req domain.AccessRequest) (domain.Decision, domain.PolicySet, error) {
	set, err := s.activeSet(ctx, req.Scope.WorkspaceID)
	if err != nil {
		return domain.Decision{}, domain.PolicySet{}, err
	}
	if req.At.IsZero() {
		req.At = s.opts.Clock()
	}
	if req.Residency == "" {
		req.Residency = s.opts.Residency
	}

	decision := set.Evaluate(req)
	clearance, err := s.store.GrantClearance(ctx, req.Scope.WorkspaceID, req.Principal.ID)
	if err != nil {
		return domain.Decision{}, domain.PolicySet{}, err
	}
	if clearance != "" && decision.Allowed {
		decision.Filters.MaxClassification = domain.LeastPermissive(
			decision.Filters.MaxClassification, clearance)
	}
	return decision, set, nil
}
