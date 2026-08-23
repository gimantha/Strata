package domain

import (
	"strings"
	"testing"
)

func sampleOntology() OntologyVersion {
	return OntologyVersion{
		WorkspaceID: WorkspaceID("01a00000-0000-7000-8000-000000000001"),
		Name:        "supply chain v1",
		Status:      OntologyActive,
		EntityTypes: []EntityTypeDef{
			{Name: "organization", Aliases: []string{"company", "org", "Organisation"}},
			{Name: "facility"},
			{Name: "person"},
		},
		Predicates: []PredicateConstraint{
			{
				Name:         "SUPPLIES_TO",
				SubjectTypes: []string{"organization"},
				ObjectTypes:  []string{"facility"},
				ObjectKinds:  []ObjectKind{ObjectEntity},
			},
			{
				Name:          "TIER",
				SubjectTypes:  []string{"organization"},
				ObjectKinds:   []ObjectKind{ObjectSymbol},
				AllowedValues: []string{"PREMIUM", "STANDARD", "LEGACY"},
				Functional:    true,
			},
			{Name: "NOTES"},
		},
	}
}

func TestOntologyAcceptsAConformingClaim(t *testing.T) {
	ontology := sampleOntology()

	violations := ontology.Check(ClaimShape{
		SubjectType: "organization",
		Predicate:   "SUPPLIES_TO",
		Object:      ObjectOfEntity(EntityID("e1")),
		ObjectType:  "facility",
	})
	if len(violations) != 0 {
		t.Fatalf("expected no violations, got %v", violations)
	}
}

func TestOntologyRejectsWhatItDoesNotDescribe(t *testing.T) {
	ontology := sampleOntology()

	cases := map[string]struct {
		shape ClaimShape
		want  ViolationCode
	}{
		"unknown predicate": {
			shape: ClaimShape{SubjectType: "organization", Predicate: "RUMORED_TO_SUPPLY",
				Object: ObjectOfString("bolts")},
			want: ViolationUnknownPredicate,
		},
		"unknown subject type": {
			shape: ClaimShape{SubjectType: "spacecraft", Predicate: "NOTES",
				Object: ObjectOfString("anything")},
			want: ViolationUnknownEntityType,
		},
		"subject of the wrong type": {
			shape: ClaimShape{SubjectType: "person", Predicate: "TIER",
				Object: ObjectOfSymbol("PREMIUM")},
			want: ViolationSubjectType,
		},
		"object of the wrong type": {
			shape: ClaimShape{SubjectType: "organization", Predicate: "SUPPLIES_TO",
				Object: ObjectOfEntity(EntityID("e1")), ObjectType: "person"},
			want: ViolationObjectType,
		},
		"object of the wrong kind": {
			shape: ClaimShape{SubjectType: "organization", Predicate: "TIER",
				Object: ObjectOfString("PREMIUM")},
			want: ViolationObjectKind,
		},
		"value outside a closed vocabulary": {
			shape: ClaimShape{SubjectType: "organization", Predicate: "TIER",
				Object: ObjectOfSymbol("PLATINUM")},
			want: ViolationValue,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			violations := ontology.Check(tc.shape)
			if len(violations) == 0 {
				t.Fatal("expected a violation")
			}
			found := false
			for _, violation := range violations {
				if violation.Code == tc.want {
					found = true
				}
				if violation.Detail == "" {
					t.Fatal("a violation without a detail cannot be acted on")
				}
			}
			if !found {
				t.Fatalf("expected %s, got %v", tc.want, violations)
			}
		})
	}
}

func TestOntologyReportsEveryViolationAtOnce(t *testing.T) {
	// A candidate with two problems should come back with two. Reporting them one round
	// trip at a time turns a single fix into several.
	ontology := sampleOntology()

	violations := ontology.Check(ClaimShape{
		SubjectType: "spacecraft",
		Predicate:   "TIER",
		Object:      ObjectOfSymbol("PLATINUM"),
	})
	if len(violations) < 2 {
		t.Fatalf("expected the unknown type and the bad value, got %v", violations)
	}
}

func TestOntologyAcceptsAliasedTypeSpellings(t *testing.T) {
	// Sources disagree about whether a company is an "organization", an "org", or a
	// "company". Refusing a claim over that is pedantry, not validation.
	ontology := sampleOntology()

	for _, spelling := range []string{"organization", "Organisation", "company", "ORG"} {
		violations := ontology.Check(ClaimShape{
			SubjectType: spelling,
			Predicate:   "TIER",
			Object:      ObjectOfSymbol("PREMIUM"),
		})
		if len(violations) != 0 {
			t.Fatalf("%q should resolve to organization, got %v", spelling, violations)
		}
	}
}

func TestOntologyLeavesUnconstrainedFieldsOpen(t *testing.T) {
	// NOTES declares no subject types, object types, or kinds. An ontology that had to
	// enumerate everything would be unusable for the loose predicates every corpus has.
	ontology := sampleOntology()

	for _, object := range []AssertionObject{
		ObjectOfString("free text"),
		ObjectOfInteger(42),
		ObjectOfSymbol("ANYTHING"),
	} {
		if violations := ontology.Check(ClaimShape{
			SubjectType: "person", Predicate: "NOTES", Object: object,
		}); len(violations) != 0 {
			t.Fatalf("unconstrained predicate rejected %v: %v", object.Kind, violations)
		}
	}
}

func TestOntologyValidationCatchesSchemasThatWouldRejectEverything(t *testing.T) {
	cases := map[string]OntologyVersion{
		"no name": {
			WorkspaceID: WorkspaceID("w"),
			EntityTypes: []EntityTypeDef{{Name: "organization"}},
		},
		"empty schema": {
			WorkspaceID: WorkspaceID("w"), Name: "empty",
		},
		"duplicate entity type": {
			WorkspaceID: WorkspaceID("w"), Name: "dup",
			EntityTypes: []EntityTypeDef{{Name: "organization"}, {Name: "Organization"}},
		},
		"alias collides with another type": {
			WorkspaceID: WorkspaceID("w"), Name: "clash",
			EntityTypes: []EntityTypeDef{
				{Name: "organization", Aliases: []string{"person"}},
				{Name: "person"},
			},
		},
		"duplicate predicate": {
			WorkspaceID: WorkspaceID("w"), Name: "dup predicate",
			EntityTypes: []EntityTypeDef{{Name: "organization"}},
			Predicates:  []PredicateConstraint{{Name: "TIER"}, {Name: "tier"}},
		},
		"predicate references an undefined type": {
			// The most likely real mistake: a typo here silently rejects every claim
			// using that predicate, which looks like a broken extractor.
			WorkspaceID: WorkspaceID("w"), Name: "typo",
			EntityTypes: []EntityTypeDef{{Name: "organization"}},
			Predicates:  []PredicateConstraint{{Name: "TIER", SubjectTypes: []string{"organisaton"}}},
		},
	}

	for name, version := range cases {
		t.Run(name, func(t *testing.T) {
			if err := version.Validate(); err == nil {
				t.Fatal("expected the schema to be refused")
			}
		})
	}
}

func TestNormalizeEntityTypeHasOneCanonicalForm(t *testing.T) {
	cases := map[string]string{
		"organization":    "organization",
		"Organization":    "organization",
		"ORGANIZATION":    "organization",
		"legal entity":    "legal_entity",
		"legal-entity":    "legal_entity",
		"legalEntity":     "legal_entity",
		"  spaced  out  ": "spaced_out",
		"":                "",
		"---":             "",
		// A digit before a capital is not treated as a word boundary, matching
		// NormalizePredicateName. The two normalizers have to agree: a type and a
		// predicate that split differently would be confusing in the same schema.
		"Order2Cash": "order2cash",
	}
	for input, want := range cases {
		if got := NormalizeEntityType(input); got != want {
			t.Fatalf("NormalizeEntityType(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestConstraintBecomesARegistryEntryWithConservativeDefaults(t *testing.T) {
	// A declared predicate defaults to coexisting for the same reason a discovered one
	// does: assuming exclusivity invents contradictions the source never stated.
	definition := PredicateConstraint{Name: "supplies to"}.ToPredicateDefinition(WorkspaceID("w"))

	if definition.Name != "SUPPLIES_TO" {
		t.Fatalf("name was not normalized: %q", definition.Name)
	}
	if definition.ConflictPolicy != ConflictPolicyCoexist {
		t.Fatalf("expected coexist, got %s", definition.ConflictPolicy)
	}
	if definition.TemporalPolicy != TemporalPolicyStateful {
		t.Fatalf("expected stateful, got %s", definition.TemporalPolicy)
	}
	if definition.Status != PredicateApproved {
		// A declared predicate is not a candidate: someone wrote it down on purpose.
		t.Fatalf("expected approved, got %s", definition.Status)
	}
}

func TestOntologyNamesAreSortedForStablePrompts(t *testing.T) {
	// The vocabulary goes into an extraction prompt, and an unstable order would change
	// the request hash on every run for no reason.
	ontology := sampleOntology()

	types := strings.Join(ontology.EntityTypeNames(), ",")
	if types != "facility,organization,person" {
		t.Fatalf("entity types are not sorted: %s", types)
	}
	predicates := strings.Join(ontology.PredicateNames(), ",")
	if predicates != "NOTES,SUPPLIES_TO,TIER" {
		t.Fatalf("predicates are not sorted: %s", predicates)
	}
}

func TestGuidedBindingNeedsAVersion(t *testing.T) {
	if (OntologyBinding{Mode: OntologyGuided}).Guided() {
		// Guided with no version would validate against nothing and accept everything,
		// which is the failure that looks most like success.
		t.Fatal("guided mode without a version must not count as guided")
	}
	if !(OntologyBinding{Mode: OntologyGuided, Version: &OntologyVersion{}}).Guided() {
		t.Fatal("guided mode with a version should be guided")
	}
	if (OntologyBinding{Mode: OntologyOpen, Version: &OntologyVersion{}}).Guided() {
		t.Fatal("open mode is never guided, even with a version attached")
	}
}
