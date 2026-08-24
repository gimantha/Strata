package domain

import (
	"testing"
	"time"
)

const (
	testWorkspace  = WorkspaceID("01a00000-0000-7000-8000-000000000001")
	testGraphSpace = GraphSpaceID("01a00000-0000-7000-8000-000000000002")
)

func principalWith(role Role) Principal {
	return Principal{
		ID:     PrincipalID("analyst"),
		Kind:   PrincipalUser,
		Grants: []Grant{{PrincipalID: "analyst", WorkspaceID: testWorkspace, Role: role}},
	}
}

func readRequest(p Principal) AccessRequest {
	return AccessRequest{
		Principal: p,
		Action:    ActionRead,
		Scope:     Scope{WorkspaceID: testWorkspace, GraphSpaceID: testGraphSpace},
		At:        time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
	}
}

func TestPolicyRequiresAGrantWhateverTheRulesSay(t *testing.T) {
	// Policy narrows access inside a workspace someone already has. It must never hand out
	// access to one they do not, however permissive a rule looks (AGENTS.md section 22.1).
	set := PolicySet{
		WorkspaceID: testWorkspace,
		Name:        "permissive",
		Rules: []PolicyRule{{
			Name: "allow everyone", Effect: PolicyAllow,
			MaxClassification: ClassificationSecret,
		}},
	}

	stranger := Principal{ID: "stranger", Kind: PrincipalUser}
	if decision := set.Evaluate(readRequest(stranger)); decision.Allowed {
		t.Fatalf("a principal with no grant was allowed: %s", decision.Reason)
	}
}

func TestRoleBaselineAppliesWithNoRules(t *testing.T) {
	set := DefaultPolicySet(testWorkspace)

	cases := map[Role]map[PolicyAction]bool{
		RoleReader: {ActionRead: true, ActionWrite: false, ActionExport: false, ActionAdmin: false},
		RoleWriter: {ActionRead: true, ActionWrite: true, ActionExport: false, ActionAdmin: false},
		RoleAdmin:  {ActionRead: true, ActionWrite: true, ActionExport: true, ActionAdmin: true},
	}
	for role, expectations := range cases {
		for action, want := range expectations {
			req := readRequest(principalWith(role))
			req.Action = action
			if got := set.Evaluate(req).Allowed; got != want {
				t.Fatalf("%s performing %s: allowed=%v, want %v", role, action, got, want)
			}
		}
	}
}

func TestExportIsNotImpliedByRead(t *testing.T) {
	// Being able to look something up and being able to walk out with everything are
	// different powers. A writer can add knowledge and still not take a copy of it.
	set := DefaultPolicySet(testWorkspace)

	req := readRequest(principalWith(RoleWriter))
	req.Action = ActionExport
	if set.Evaluate(req).Allowed {
		t.Fatal("a writer was allowed to export")
	}
}

func TestDefaultClearanceCapsWhatAReaderSees(t *testing.T) {
	set := DefaultPolicySet(testWorkspace)

	decision := set.Evaluate(readRequest(principalWith(RoleReader)))
	if !decision.Allowed {
		t.Fatalf("a reader should be able to read: %s", decision.Reason)
	}
	if decision.Filters.MaxClassification != ClassificationInternal {
		t.Fatalf("expected an internal ceiling, got %q", decision.Filters.MaxClassification)
	}

	permitted := decision.Filters.PermittedClassifications()
	if len(permitted) != 2 ||
		permitted[0] != ClassificationPublic || permitted[1] != ClassificationInternal {
		t.Fatalf("expected public and internal only, got %v", permitted)
	}
}

func TestAllowRuleRaisesTheCeilingForNamedPrincipals(t *testing.T) {
	set := PolicySet{
		WorkspaceID: testWorkspace, Name: "clearances",
		DefaultClearance: ClassificationInternal,
		Rules: []PolicyRule{{
			Name: "compliance can see restricted", Effect: PolicyAllow,
			Actions:           []PolicyAction{ActionRead},
			PrincipalIDs:      []PrincipalID{"analyst"},
			MaxClassification: ClassificationRestricted,
		}},
	}

	cleared := set.Evaluate(readRequest(principalWith(RoleReader)))
	if cleared.Filters.MaxClassification != ClassificationRestricted {
		t.Fatalf("expected the rule to raise the ceiling, got %q",
			cleared.Filters.MaxClassification)
	}

	other := principalWith(RoleReader)
	other.ID = "someone-else"
	other.Grants[0].PrincipalID = "someone-else"
	if got := set.Evaluate(readRequest(other)).Filters.MaxClassification; got != ClassificationInternal {
		t.Fatalf("the rule should not apply to another principal, got %q", got)
	}
}

func TestBlanketDenyBeatsAnyAllow(t *testing.T) {
	// Deny wins, always. Elaborate precedence schemes are where policy bugs live, because
	// two people reading the same rules reach different conclusions.
	set := PolicySet{
		WorkspaceID: testWorkspace, Name: "mixed",
		Rules: []PolicyRule{
			{Name: "allow reads", Effect: PolicyAllow, Actions: []PolicyAction{ActionRead},
				MaxClassification: ClassificationSecret},
			{Name: "suspended", Effect: PolicyDeny, PrincipalIDs: []PrincipalID{"analyst"}},
		},
	}

	decision := set.Evaluate(readRequest(principalWith(RoleAdmin)))
	if decision.Allowed {
		t.Fatal("a blanket deny must beat an allow")
	}
	if decision.Rule != "suspended" {
		t.Fatalf("the refusal should name the rule that caused it, got %q", decision.Rule)
	}
}

func TestDenyNamingResourcesNarrowsRatherThanRefuses(t *testing.T) {
	// The distinction the whole design rests on: a rule that hides some data must not make
	// the query fail, or people route around the system entirely.
	set := PolicySet{
		WorkspaceID: testWorkspace, Name: "carve-outs",
		DefaultClearance: ClassificationConfidential,
		Rules: []PolicyRule{
			{
				Name: "no hr material", Effect: PolicyDeny,
				Actions:   []PolicyAction{ActionRead},
				Roles:     []Role{RoleReader},
				SourceIDs: []SourceID{"hr-system"},
			},
			{
				Name: "no salary predicates", Effect: PolicyDeny,
				Actions: []PolicyAction{ActionRead}, Roles: []Role{RoleReader},
				Predicates: []string{"salary", "compensation band"},
			},
		},
	}

	decision := set.Evaluate(readRequest(principalWith(RoleReader)))
	if !decision.Allowed {
		t.Fatalf("a resource-scoped deny should narrow, not refuse: %s", decision.Reason)
	}
	if len(decision.Filters.DeniedSources) != 1 || decision.Filters.DeniedSources[0] != "hr-system" {
		t.Fatalf("expected the source to be excluded, got %v", decision.Filters.DeniedSources)
	}
	if len(decision.Filters.DeniedPredicates) != 2 {
		t.Fatalf("expected two excluded predicates, got %v", decision.Filters.DeniedPredicates)
	}
	// Predicate names are normalized so a rule written in prose still matches the registry.
	if decision.Filters.DeniedPredicates[0] != "COMPENSATION_BAND" {
		t.Fatalf("predicate names should be normalized, got %v", decision.Filters.DeniedPredicates)
	}
}

func TestTimeBoundedRulesApplyOnlyWhileInForce(t *testing.T) {
	from := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 5, 31, 0, 0, 0, 0, time.UTC)

	set := PolicySet{
		WorkspaceID: testWorkspace, Name: "temporary",
		Rules: []PolicyRule{{
			Name: "incident access", Effect: PolicyAllow,
			Actions: []PolicyAction{ActionRead}, PrincipalIDs: []PrincipalID{"analyst"},
			MaxClassification: ClassificationSecret,
			NotBefore:         &from, NotAfter: &until,
		}},
	}

	during := readRequest(principalWith(RoleReader))
	during.At = time.Date(2026, 5, 15, 0, 0, 0, 0, time.UTC)
	if got := set.Evaluate(during).Filters.MaxClassification; got != ClassificationSecret {
		t.Fatalf("the rule should be in force, got %q", got)
	}

	after := readRequest(principalWith(RoleReader))
	after.At = time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)
	if got := set.Evaluate(after).Filters.MaxClassification; got == ClassificationSecret {
		t.Fatal("temporary access outlived its window")
	}
}

func TestPurposeRestrictedRulesNeedTheStatedPurpose(t *testing.T) {
	set := PolicySet{
		WorkspaceID: testWorkspace, Name: "purpose",
		Rules: []PolicyRule{{
			Name: "support may see confidential", Effect: PolicyAllow,
			Actions: []PolicyAction{ActionRead}, Purposes: []string{"customer-support"},
			MaxClassification: ClassificationConfidential,
		}},
	}

	stated := readRequest(principalWith(RoleReader))
	stated.Purpose = "customer-support"
	if got := set.Evaluate(stated).Filters.MaxClassification; got != ClassificationConfidential {
		t.Fatalf("the stated purpose should unlock the rule, got %q", got)
	}

	unstated := readRequest(principalWith(RoleReader))
	if got := set.Evaluate(unstated).Filters.MaxClassification; got == ClassificationConfidential {
		t.Fatal("a purpose-restricted rule applied without the purpose")
	}
}

func TestPermittedClassificationsRespectCeilingAndCarveOuts(t *testing.T) {
	filters := PolicyFilters{
		MaxClassification:     ClassificationRestricted,
		DeniedClassifications: []Classification{ClassificationConfidential},
	}

	permitted := filters.PermittedClassifications()
	want := map[Classification]bool{
		ClassificationPublic: true, ClassificationInternal: true, ClassificationRestricted: true,
	}
	if len(permitted) != len(want) {
		t.Fatalf("expected %d levels, got %v", len(want), permitted)
	}
	for _, classification := range permitted {
		if !want[classification] {
			t.Fatalf("%s should not be permitted: %v", classification, permitted)
		}
	}
}

func TestFiltersRefuseRecordsTheQueryShouldNotHaveReturned(t *testing.T) {
	// The second gate. Filters are enforced in SQL, and this is what catches a query that
	// forgot to apply them — belt and braces on the path where a miss hands over data.
	filters := PolicyFilters{
		MaxClassification: ClassificationInternal,
		DeniedSources:     []SourceID{"hr-system"},
		DeniedPredicates:  []string{"SALARY"},
		DeniedEntityTypes: []string{"person"},
	}

	if filters.Allows(ClassificationRestricted, "crm", "TIER", MemorySemantic, "organization") {
		t.Fatal("a record above the ceiling was allowed")
	}
	if filters.Allows(ClassificationInternal, "hr-system", "TIER", MemorySemantic, "organization") {
		t.Fatal("a record from an excluded source was allowed")
	}
	if filters.Allows(ClassificationInternal, "crm", "salary", MemorySemantic, "organization") {
		t.Fatal("an excluded predicate was allowed")
	}
	if filters.Allows(ClassificationInternal, "crm", "TIER", MemorySemantic, "Person") {
		t.Fatal("an excluded entity type was allowed")
	}
	if !filters.Allows(ClassificationInternal, "crm", "TIER", MemorySemantic, "organization") {
		t.Fatal("a permitted record was refused")
	}
}

func TestClearanceOnlyEverNarrows(t *testing.T) {
	// Two limits on what someone may see combine to the tighter one. If a grant's clearance
	// could raise a policy ceiling, granting workspace access would quietly hand over
	// everything in it.
	if got := LeastPermissive(ClassificationInternal, ClassificationSecret); got != ClassificationInternal {
		t.Fatalf("expected the tighter limit, got %q", got)
	}
	if got := LeastPermissive(ClassificationSecret, ClassificationPublic); got != ClassificationPublic {
		t.Fatalf("expected the tighter limit, got %q", got)
	}
	if got := LeastPermissive("", ClassificationConfidential); got != ClassificationConfidential {
		t.Fatalf("an unset limit should yield, got %q", got)
	}
	if got := LeastPermissive(ClassificationConfidential, ""); got != ClassificationConfidential {
		t.Fatalf("an unset limit should yield, got %q", got)
	}
}

func TestPolicyValidationCatchesRulesThatCannotWork(t *testing.T) {
	before := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	after := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)

	cases := map[string]PolicySet{
		"no name": {WorkspaceID: testWorkspace},
		"rule without a name": {WorkspaceID: testWorkspace, Name: "x",
			Rules: []PolicyRule{{Effect: PolicyAllow}}},
		"unknown effect": {WorkspaceID: testWorkspace, Name: "x",
			Rules: []PolicyRule{{Name: "r", Effect: "maybe"}}},
		"unknown action": {WorkspaceID: testWorkspace, Name: "x",
			Rules: []PolicyRule{{Name: "r", Effect: PolicyAllow, Actions: []PolicyAction{"delete"}}}},
		"duplicate rule": {WorkspaceID: testWorkspace, Name: "x", Rules: []PolicyRule{
			{Name: "r", Effect: PolicyAllow}, {Name: "r", Effect: PolicyDeny},
		}},
		"window that never opens": {WorkspaceID: testWorkspace, Name: "x",
			Rules: []PolicyRule{{Name: "r", Effect: PolicyAllow,
				NotBefore: &before, NotAfter: &after}}},
	}

	for name, set := range cases {
		t.Run(name, func(t *testing.T) {
			if err := set.Validate(); err == nil {
				t.Fatal("expected the policy to be refused")
			}
		})
	}
}

func TestDecisionsAreDeterministic(t *testing.T) {
	// The same policy and the same request must produce byte-identical filters, or the SQL
	// changes between requests and the audit record stops being comparable.
	set := PolicySet{
		WorkspaceID: testWorkspace, Name: "many rules",
		Rules: []PolicyRule{
			{Name: "a", Effect: PolicyDeny, SourceIDs: []SourceID{"z", "a"}},
			{Name: "b", Effect: PolicyDeny, SourceIDs: []SourceID{"m", "a"}},
			{Name: "c", Effect: PolicyAllow, Predicates: []string{"tier", "region"}},
		},
	}

	first := set.Evaluate(readRequest(principalWith(RoleReader)))
	second := set.Evaluate(readRequest(principalWith(RoleReader)))

	if len(first.Filters.DeniedSources) != 3 {
		t.Fatalf("expected three distinct sources, got %v", first.Filters.DeniedSources)
	}
	for i := range first.Filters.DeniedSources {
		if first.Filters.DeniedSources[i] != second.Filters.DeniedSources[i] {
			t.Fatal("two evaluations of one policy produced different filters")
		}
	}
	if first.Filters.DeniedSources[0] != "a" {
		t.Fatalf("filters should be sorted, got %v", first.Filters.DeniedSources)
	}
}
