package domain

import (
	"encoding/json"
	"testing"
	"time"
)

func TestNormalizePredicateName(t *testing.T) {
	// Open-mode extraction invents names freely; without one canonical form the registry
	// fills with near-duplicates that never match each other.
	cases := map[string]string{
		"works_at":      "WORKS_AT",
		"worksAt":       "WORKS_AT",
		"WorksAt":       "WORKS_AT",
		"works at":      "WORKS_AT",
		"works-at":      "WORKS_AT",
		"  works  at  ": "WORKS_AT",
		"WORKS_AT":      "WORKS_AT",
		"role_at_v2":    "ROLE_AT_V2",
		"hasEmail":      "HAS_EMAIL",
		"__weird__":     "WEIRD",
		"":              "",
	}
	for in, want := range cases {
		if got := NormalizePredicateName(in); got != want {
			t.Fatalf("NormalizePredicateName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNormalizeAlias(t *testing.T) {
	// Alias matching is deliberately conservative: aggressive normalization merges
	// identities that should stay separate.
	if NormalizeAlias("  Alice   Chen ") != "alice chen" {
		t.Fatal("aliases must be case-folded with collapsed whitespace")
	}
	if NormalizeAlias("Acme Corp") == NormalizeAlias("Acme Corporation") {
		t.Fatal("normalization must not conflate distinct names")
	}
}

func TestAssertionFingerprintIdentity(t *testing.T) {
	base := Assertion{
		GraphSpaceID:   "gs",
		SubjectID:      "subject",
		Predicate:      PredicateRef{Name: "ROLE_AT", Version: 1},
		Object:         ObjectOfSymbol("CEO"),
		MemoryKind:     MemorySemantic,
		SourceEventID:  "event-1",
		ProvenanceMode: ProvenanceExtracted,
	}

	if base.ComputeFingerprint() != base.ComputeFingerprint() {
		t.Fatal("fingerprints must be stable")
	}

	// Anything that makes this a different claim must change the fingerprint.
	variations := map[string]func(*Assertion){
		"subject":      func(a *Assertion) { a.SubjectID = "other" },
		"predicate":    func(a *Assertion) { a.Predicate.Name = "WORKS_AT" },
		"object":       func(a *Assertion) { a.Object = ObjectOfSymbol("CTO") },
		"scope key":    func(a *Assertion) { a.ScopeKey = "board" },
		"valid from":   func(a *Assertion) { a.Temporal.ValidFrom = ptrTime(time.Now()) },
		"source event": func(a *Assertion) { a.SourceEventID = "event-2" },
		"provenance":   func(a *Assertion) { a.ProvenanceMode = ProvenanceUserAsserted },
	}
	for name, mutate := range variations {
		variant := base
		mutate(&variant)
		if variant.ComputeFingerprint() == base.ComputeFingerprint() {
			t.Fatalf("changing the %s must change the fingerprint", name)
		}
	}

	// Knowledge time must NOT change it: reprocessing the same event later is a replay,
	// not new knowledge.
	replayed := base
	replayed.Temporal.RecordedAt = time.Now()
	if replayed.ComputeFingerprint() != base.ComputeFingerprint() {
		t.Fatal("when a claim was recorded must not affect its identity")
	}
}

func TestAssertionTemporalPredicates(t *testing.T) {
	april2 := time.Date(2026, 4, 2, 0, 0, 0, 0, time.UTC)
	april20 := time.Date(2026, 4, 20, 0, 0, 0, 0, time.UTC)
	april10 := time.Date(2026, 4, 10, 0, 0, 0, 0, time.UTC)
	april25 := time.Date(2026, 4, 25, 0, 0, 0, 0, time.UTC)

	a := Assertion{Temporal: TemporalCoordinates{
		RecordedAt:   april2,
		SupersededAt: &april20,
		ValidFrom:    ptrTime(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)),
		ValidTo:      ptrTime(time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)),
	}}

	if !a.BelievedAt(april10) {
		t.Fatal("a claim recorded April 2 and superseded April 20 was believed on April 10")
	}
	if a.BelievedAt(april25) {
		t.Fatal("it was no longer believed on April 25")
	}
	if a.BelievedAt(time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatal("it was not yet believed the day before it was recorded")
	}
	// The supersession instant itself belongs to the new belief.
	if a.BelievedAt(april20) {
		t.Fatal("belief ends at the instant of supersession")
	}

	if !a.ValidAt(time.Date(2026, 3, 25, 0, 0, 0, 0, time.UTC)) {
		t.Fatal("March 25 is inside the claimed validity")
	}
	if a.ValidAt(time.Date(2026, 4, 5, 0, 0, 0, 0, time.UTC)) {
		t.Fatal("April 5 is outside the claimed validity")
	}

	retracted := Assertion{Temporal: TemporalCoordinates{RecordedAt: april2}, RetractedAt: &april20}
	if !retracted.BelievedAt(april10) || retracted.BelievedAt(april25) {
		t.Fatal("retraction must bound belief the same way supersession does")
	}
}

func TestAssertionActiveAt(t *testing.T) {
	start := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC)

	a := Assertion{Temporal: TemporalCoordinates{ActiveFrom: &start, ExpiresAt: &end}}
	if !a.ActiveAt(start) {
		t.Fatal("context is active from its start instant")
	}
	if a.ActiveAt(end) {
		t.Fatal("context stops being active once it expires")
	}
	if a.ActiveAt(start.Add(-time.Hour)) {
		t.Fatal("context is not active before it starts")
	}

	always := Assertion{}
	if !always.ActiveAt(time.Now()) {
		t.Fatal("a claim with no lifecycle bounds is always active context")
	}
}

func TestAssertionValidationRejectsUnexplainedInference(t *testing.T) {
	base := Assertion{
		WorkspaceID: "ws", GraphSpaceID: "gs", SubjectID: "s",
		Predicate:  PredicateRef{Name: "X", Version: 1},
		Object:     ObjectOfBool(true),
		MemoryKind: MemorySemantic,
		Status:     AssertionActive,
		Confidence: 1,
		Temporal:   TemporalCoordinates{ObservedAt: time.Now(), RecordedAt: time.Now()},
	}

	observed := base
	observed.ProvenanceMode = ProvenanceExtracted
	if err := observed.Validate(); err != nil {
		t.Fatalf("an observed claim needs no derivation: %v", err)
	}

	for _, mode := range []ProvenanceMode{ProvenanceInferred, ProvenanceDerived} {
		reasoned := base
		reasoned.ProvenanceMode = mode
		if err := reasoned.Validate(); err == nil {
			t.Fatalf("%s claims must name a derivation: inference is not observation", mode)
		}
		derivationID := DerivationID("d1")
		reasoned.DerivationID = &derivationID
		if err := reasoned.Validate(); err != nil {
			t.Fatalf("%s claim with a derivation rejected: %v", mode, err)
		}
	}
}

func TestEvidenceBoundsTheStoredExcerpt(t *testing.T) {
	e := Evidence{WorkspaceID: "ws", EpisodeID: "ep", Confidence: 1}
	if err := e.Validate(); err != nil {
		t.Fatalf("minimal evidence rejected: %v", err)
	}

	e.ExtractedText = string(make([]byte, MaxEvidenceExcerpt+1))
	if err := e.Validate(); err == nil {
		t.Fatal("evidence cites source material, it does not copy it wholesale")
	}

	noEpisode := Evidence{WorkspaceID: "ws"}
	if err := noEpisode.Validate(); err == nil {
		t.Fatal("evidence that points at nothing is not evidence")
	}
}

func TestPredicateAllowsMultipleValues(t *testing.T) {
	multi := PredicateDefinition{ConflictPolicy: ConflictPolicyCoexist}
	if !multi.AllowsMultipleValues() {
		t.Fatal("a coexisting predicate permits several simultaneous values")
	}
	functional := PredicateDefinition{Functional: true, ConflictPolicy: ConflictPolicyCoexist}
	if functional.AllowsMultipleValues() {
		t.Fatal("a functional predicate holds one value at a time")
	}
	exclusive := PredicateDefinition{ConflictPolicy: ConflictPolicyLatestWins}
	if exclusive.AllowsMultipleValues() {
		t.Fatal("latest-wins implies values replace each other")
	}
}

func TestAssertionQueryNormalizeBoundsResults(t *testing.T) {
	// Unbounded reads are not permitted, so an unspecified limit gets a default and an
	// excessive one is capped.
	q := AssertionQuery{Scope: Scope{WorkspaceID: "ws"}}.Normalize()
	if q.Limit != DefaultAssertionLimit {
		t.Fatalf("expected a default limit, got %d", q.Limit)
	}
	huge := AssertionQuery{Scope: Scope{WorkspaceID: "ws"}, Limit: 10_000}.Normalize()
	if huge.Limit != MaxAssertionLimit {
		t.Fatalf("expected the limit to be capped, got %d", huge.Limit)
	}
	// Predicate names are normalized so a query matches what the registry stored.
	named := AssertionQuery{Scope: Scope{WorkspaceID: "ws"}, Predicates: []string{"worksAt"}}.Normalize()
	if named.Predicates[0] != "WORKS_AT" {
		t.Fatalf("query predicates must be normalized, got %q", named.Predicates[0])
	}

	if err := (AssertionQuery{}).Validate(); err == nil {
		t.Fatal("a query without workspace scope must be rejected")
	}
	inverted := AssertionQuery{
		Scope:        Scope{WorkspaceID: "ws"},
		ValidBetween: &TimeRange{Start: time.Now(), End: time.Now().Add(-time.Hour)},
	}
	if err := inverted.Validate(); err == nil {
		t.Fatal("an inverted range must be rejected")
	}
}

func TestAssertionStatusBelievable(t *testing.T) {
	// A disputed claim is still believed: hiding it would present contested knowledge as
	// settled, and dropping it would lose the disagreement entirely.
	if !AssertionActive.Believable() || !AssertionDisputed.Believable() {
		t.Fatal("active and disputed claims are both currently believed")
	}
	for _, s := range []AssertionStatus{AssertionSuperseded, AssertionRetracted, AssertionQuarantined, AssertionProposed} {
		if s.Believable() {
			t.Fatalf("%s must not count as current belief", s)
		}
	}
}

func ptrTime(t time.Time) *time.Time { return &t }

func TestAssertionObjectSurvivesAJSONRoundTrip(t *testing.T) {
	// A type with a custom marshaller and no matching unmarshaller is silently lossy, and
	// the loss appears wherever a value is written and read back: a portable package, a
	// queued job, a cached response. This was found by a symbol arriving with no text
	// after a package round trip.
	moment := time.Date(2026, 3, 1, 9, 30, 0, 0, time.UTC)
	day := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)

	cases := map[string]AssertionObject{
		"entity":    ObjectOfEntity(EntityID("01a0-entity")),
		"string":    ObjectOfString("industrial fasteners"),
		"uri":       ObjectOfURI("https://example.test/spec"),
		"symbol":    ObjectOfSymbol("PREMIUM"),
		"integer":   ObjectOfInteger(50000),
		"decimal":   ObjectOfDecimal("1234.5678"),
		"boolean":   ObjectOfBool(true),
		"timestamp": ObjectOfTimestamp(moment),
		"date":      ObjectOfDate(day),
		"duration":  ObjectOfDuration(90 * time.Minute),
		"geo":       ObjectOfGeo(55.86, -4.25),
		"json":      ObjectOfJSON(json.RawMessage(`{"a":1}`)),
	}

	for name, original := range cases {
		t.Run(name, func(t *testing.T) {
			encoded, err := json.Marshal(original)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}

			var decoded AssertionObject
			if err := json.Unmarshal(encoded, &decoded); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}

			if decoded.Kind != original.Kind {
				t.Fatalf("kind changed: %s then %s", original.Kind, decoded.Kind)
			}
			// The canonical key is what equality and fingerprinting use, so agreeing on it
			// is the property that actually matters.
			if decoded.Key() != original.Key() {
				t.Fatalf("key changed: %q then %q", original.Key(), decoded.Key())
			}
			if !decoded.Equal(original) {
				t.Fatalf("value changed: %+v then %+v", original, decoded)
			}
			if err := decoded.Validate(); err != nil {
				t.Fatalf("the decoded object is invalid: %v", err)
			}
		})
	}
}

func TestEmptyAssertionObjectDecodesAsEmpty(t *testing.T) {
	// A claim may legitimately carry no object yet, and an absent kind must not be an
	// error the caller has to special-case.
	var decoded AssertionObject
	if err := json.Unmarshal([]byte(`{}`), &decoded); err != nil {
		t.Fatalf("an empty object should decode: %v", err)
	}
	if decoded.Kind != "" {
		t.Fatalf("expected an empty object, got %+v", decoded)
	}
}
