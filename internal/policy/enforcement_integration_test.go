package policy_test

import (
	"context"
	"strings"
	"testing"

	"github.com/gimantha/strata/internal/domain"
	"github.com/gimantha/strata/internal/ingest"
	"github.com/gimantha/strata/internal/knowledge"
	"github.com/gimantha/strata/internal/normalize"
	"github.com/gimantha/strata/internal/policy"
)

// seedClassified puts one claim at a stated sensitivity into a tenant.
func (h *harness) seedClassified(t *testing.T, owner tenant, text, subject string, classification domain.Classification, key string) domain.AssertionID {
	t.Helper()
	ctx := context.Background()

	receipt, err := h.gateway.Accept(ctx, ingest.Request{
		Scope:          owner.Scope(),
		Principal:      owner.Principal.Ref(),
		SourceID:       owner.Source.ID,
		MediaType:      normalize.MediaTypePlain,
		Payload:        []byte(text),
		IdempotencyKey: key,
		Classification: classification,
	})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if _, err := h.runner.Process(ctx, owner.Workspace.ID, receipt.SourceEventID, false); err != nil {
		t.Fatalf("process: %v", err)
	}

	episodes, err := h.fixture.Store.ListEpisodes(ctx, owner.Workspace.ID, receipt.SourceEventID)
	if err != nil || len(episodes) == 0 {
		t.Fatalf("episodes: %v", err)
	}

	result, err := h.service.Assert(ctx, knowledge.AssertRequest{
		Scope: owner.Scope(), Principal: owner.Principal.Ref(),
		SourceEventID: receipt.SourceEventID,
		Claims: []knowledge.Claim{{
			Subject:        knowledge.EntityRef{Name: subject, Type: "organization"},
			Predicate:      "NOTES",
			Object:         domain.ObjectOfString(text),
			Classification: classification,
			Evidence:       []knowledge.EvidenceInput{{EpisodeID: episodes[0].ID, ExtractedText: text}},
		}},
	})
	if err != nil {
		t.Fatalf("assert: %v", err)
	}
	if _, err := h.projector.ProjectEvent(ctx, owner.Scope(), receipt.SourceEventID); err != nil {
		t.Fatalf("project: %v", err)
	}
	if _, err := h.projector.ProjectEntities(ctx, owner.Scope()); err != nil {
		t.Fatalf("project entities: %v", err)
	}
	return result.Assertions[0].ID
}

// TestIntegrationClassificationFiltersApplyInsideRetrieval is AGENTS.md section 22.4: never
// retrieve unauthorized data and hide it afterwards.
//
// The test cannot see inside the SQL, so it checks the consequence: a restricted claim is
// absent from the results of a principal cleared only for internal material, while the same
// query run by a cleared principal returns it. If filtering happened after ranking, the
// uncleared principal's result count would also change — which is the observable the last
// assertion is about.
func TestIntegrationClassificationFiltersApplyInsideRetrieval(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()

	h.seedClassified(t, h.acme, "Routine maintenance is scheduled for Thornbury each spring.",
		"Thornbury Works", domain.ClassificationInternal, "class-internal")
	h.seedClassified(t, h.acme, "Thornbury Works is under investigation for Quillon fraud.",
		"Thornbury Works", domain.ClassificationRestricted, "class-restricted")

	uncleared := domain.PolicyFilters{MaxClassification: domain.ClassificationInternal}
	cleared := domain.PolicyFilters{MaxClassification: domain.ClassificationRestricted}

	restrictedVisible := func(filters domain.PolicyFilters) bool {
		result, err := h.retriever.Query(ctx, domain.QueryRequest{
			Scope: h.acme.Scope(), Query: "Thornbury", Policy: filters, Limit: 50,
		})
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		for _, item := range result.Items {
			if strings.Contains(item.Content, "Quillon") {
				return true
			}
		}
		return false
	}

	if restrictedVisible(uncleared) {
		t.Fatal("a restricted claim reached a principal cleared only for internal material")
	}
	if !restrictedVisible(cleared) {
		t.Fatal("a cleared principal could not see the restricted claim, so the filter is not selective")
	}

	// Context assembly inherits the same narrowing, and hydration re-checks it against the
	// canonical record — a citation reproduces source text and is its own disclosure path.
	block, err := h.assembler.Assemble(ctx, domain.ContextRequest{
		Scope: h.acme.Scope(), Query: "Thornbury", TokenBudget: 1500, Policy: uncleared,
	})
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	for _, item := range block.Items {
		if strings.Contains(item.Text, "Quillon") {
			t.Fatalf("a context block included restricted material: %s", item.Text)
		}
	}
	for _, citation := range block.Citations {
		if strings.Contains(citation.Quote, "Quillon") {
			t.Fatalf("a citation quoted restricted material: %s", citation.Quote)
		}
	}
}

// TestIntegrationPolicyDecisionsAreAudited covers AGENTS.md section 22.6.
func TestIntegrationPolicyDecisionsAreAudited(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()

	principal := h.acme.Principal
	before, err := h.fixture.Store.CountAuditEvents(ctx, h.acme.Workspace.ID, "policy.read")
	if err != nil {
		t.Fatalf("count audit: %v", err)
	}

	decision, err := h.policy.AuthorizeRead(ctx, principal, h.acme.Scope(), "customer-support")
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	if !decision.Allowed {
		t.Fatalf("an admin should be allowed to read: %s", decision.Reason)
	}

	after, err := h.fixture.Store.CountAuditEvents(ctx, h.acme.Workspace.ID, "policy.read")
	if err != nil {
		t.Fatalf("count audit: %v", err)
	}
	if after != before+1 {
		t.Fatalf("expected the decision to be recorded: %d then %d", before, after)
	}

	// A refusal is the record most worth having. A principal with no grant here.
	stranger := h.globex.Principal
	refused, err := h.policy.AuthorizeRead(ctx, stranger, h.acme.Scope(), "")
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	if refused.Allowed {
		t.Fatal("a principal from another workspace was allowed")
	}
	denied, err := h.fixture.Store.CountAuditEvents(ctx, h.acme.Workspace.ID, "policy.read")
	if err != nil {
		t.Fatalf("count audit: %v", err)
	}
	if denied != after+1 {
		t.Fatal("a refusal was not recorded")
	}
}

// TestIntegrationPolicyVersionsAreImmutableAndOneIsActive holds the property audit records
// depend on: a decision naming a policy version must be checkable against what it said.
func TestIntegrationPolicyVersionsAreImmutableAndOneIsActive(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()

	first, err := h.policy.Define(ctx, policy.DefineRequest{
		Scope: h.acme.Scope(), Principal: h.acme.Principal.Ref(),
		Name: "baseline", DefaultClearance: domain.ClassificationInternal, Activate: true,
		Rules: []domain.PolicyRule{{Name: "readers", Effect: domain.PolicyAllow,
			Actions: []domain.PolicyAction{domain.ActionRead}, Roles: []domain.Role{domain.RoleReader}}},
	})
	if err != nil {
		t.Fatalf("define: %v", err)
	}

	second, err := h.policy.Define(ctx, policy.DefineRequest{
		Scope: h.acme.Scope(), Principal: h.acme.Principal.Ref(),
		Name: "tightened", DefaultClearance: domain.ClassificationPublic, Activate: true,
	})
	if err != nil {
		t.Fatalf("define second: %v", err)
	}
	if second.Version != first.Version+1 {
		t.Fatalf("versions must be sequential: %d then %d", first.Version, second.Version)
	}

	// The earlier version still says what it always said.
	reloaded, err := h.fixture.Store.GetPolicySet(ctx, h.acme.Workspace.ID, first.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(reloaded.Rules) != 1 || reloaded.DefaultClearance != domain.ClassificationInternal {
		t.Fatal("an earlier policy version changed when a later one was defined")
	}
	if reloaded.Active {
		t.Fatal("activating a new version should deactivate the previous one")
	}

	active, err := h.policy.Active(ctx, h.acme.Workspace.ID)
	if err != nil {
		t.Fatalf("active: %v", err)
	}
	if active.ID != second.ID {
		t.Fatalf("expected version %d to be active, got %d", second.Version, active.Version)
	}
}

// TestIntegrationClearanceOnlyNarrows checks that a grant's clearance cannot raise what
// policy permits — otherwise granting workspace access would hand over everything in it.
func TestIntegrationClearanceOnlyNarrows(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()

	if _, err := h.policy.Define(ctx, policy.DefineRequest{
		Scope: h.acme.Scope(), Principal: h.acme.Principal.Ref(),
		Name: "generous", DefaultClearance: domain.ClassificationRestricted, Activate: true,
	}); err != nil {
		t.Fatalf("define: %v", err)
	}

	generous, err := h.policy.AuthorizeRead(ctx, h.acme.Principal, h.acme.Scope(), "")
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	if generous.Filters.MaxClassification != domain.ClassificationRestricted {
		t.Fatalf("expected the policy ceiling, got %q", generous.Filters.MaxClassification)
	}

	// A tighter clearance on the grant wins.
	if err := h.policy.SetClearance(ctx, h.acme.Scope(), h.acme.Principal.Ref(),
		h.acme.Principal.ID, domain.ClassificationPublic); err != nil {
		t.Fatalf("set clearance: %v", err)
	}
	narrowed, err := h.policy.AuthorizeRead(ctx, h.acme.Principal, h.acme.Scope(), "")
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	if narrowed.Filters.MaxClassification != domain.ClassificationPublic {
		t.Fatalf("a clearance should narrow the ceiling, got %q",
			narrowed.Filters.MaxClassification)
	}

	// A looser clearance does not.
	if err := h.policy.SetClearance(ctx, h.acme.Scope(), h.acme.Principal.Ref(),
		h.acme.Principal.ID, domain.ClassificationSecret); err != nil {
		t.Fatalf("set clearance: %v", err)
	}
	raised, err := h.policy.AuthorizeRead(ctx, h.acme.Principal, h.acme.Scope(), "")
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	if raised.Filters.MaxClassification != domain.ClassificationRestricted {
		t.Fatalf("a clearance must not raise the policy ceiling, got %q",
			raised.Filters.MaxClassification)
	}
}
