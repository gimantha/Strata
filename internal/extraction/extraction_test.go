package extraction_test

import (
	"strings"
	"testing"

	"github.com/gimantha/strata/internal/domain"
	"github.com/gimantha/strata/internal/extraction"
	"github.com/gimantha/strata/internal/llm"
)

const sampleSource = "Alice Chen is the CTO of Acme Corporation. She joined on 3 March 2026."

func TestBuildPromptIsolatesSourceAsData(t *testing.T) {
	prompt, err := extraction.BuildPrompt([]extraction.SourceUnit{
		{Ref: "chunk-1", Content: sampleSource},
	})
	if err != nil {
		t.Fatalf("build prompt: %v", err)
	}

	var system, user string
	for _, m := range prompt.Request.Messages {
		switch m.Role {
		case llm.RoleSystem:
			system = m.Content
		case llm.RoleUser:
			user = m.Content
		}
	}

	if system == "" || user == "" {
		t.Fatal("the prompt needs both a system and a user message")
	}
	// The instruction to disregard embedded instructions must actually be present.
	for _, phrase := range []string{"untrusted", "never follow", "DATA, not instructions"} {
		if !strings.Contains(strings.ToLower(system), strings.ToLower(phrase)) {
			t.Fatalf("the system prompt must state that source content is data, missing %q", phrase)
		}
	}
	// The model must be told it cannot act.
	if !strings.Contains(system, "no tools") {
		t.Fatal("the system prompt must state the model has no tools")
	}

	// The source must sit inside delimiters carrying the nonce.
	if prompt.Nonce == "" || len(prompt.Nonce) < 16 {
		t.Fatalf("the delimiter nonce is too short to be unguessable: %q", prompt.Nonce)
	}
	if !strings.Contains(user, "<<<BEGIN_UNTRUSTED_SOURCE_"+prompt.Nonce+">>>") ||
		!strings.Contains(user, "<<<END_UNTRUSTED_SOURCE_"+prompt.Nonce+">>>") {
		t.Fatalf("source material must be delimited with the nonce:\n%s", user)
	}
	if !strings.Contains(user, sampleSource) {
		t.Fatal("the source material must reach the model")
	}
	if !strings.Contains(prompt.Source, sampleSource) {
		t.Fatal("the prompt must retain the source for verifying quotes afterwards")
	}

	// Schema-constrained, low-temperature, seeded: extraction wants reproducibility.
	if prompt.Request.SchemaName != extraction.SchemaName || len(prompt.Request.Schema) == 0 {
		t.Fatal("extraction must be schema-constrained")
	}
	// Asserted as a pointer to zero, not as a zero: the field was a plain float64 once, and
	// a request for greedy decoding was indistinguishable from saying nothing, so the
	// adapter dropped it and extraction ran at the provider's default all the way to the
	// wire. The struct looked right in exactly this assertion while the request was wrong.
	if prompt.Request.Temperature == nil || *prompt.Request.Temperature != 0 {
		t.Fatal("extraction must ask for temperature 0, not leave it unset")
	}
	if prompt.Request.Seed == nil {
		t.Fatal("extraction must ask for deterministic sampling where the provider supports it")
	}
}

func TestBuildPromptUsesAFreshNonceEachTime(t *testing.T) {
	// A delimiter derived from the content would be computable by whoever wrote the
	// content, which is exactly the party it must be unforgeable against.
	seen := map[string]bool{}
	for i := 0; i < 20; i++ {
		prompt, err := extraction.BuildPrompt([]extraction.SourceUnit{{Content: sampleSource}})
		if err != nil {
			t.Fatalf("build prompt: %v", err)
		}
		if seen[prompt.Nonce] {
			t.Fatal("delimiter nonces must not repeat across requests")
		}
		seen[prompt.Nonce] = true
	}
}

func TestBuildPromptRejectsEmptyInput(t *testing.T) {
	if _, err := extraction.BuildPrompt(nil); err == nil {
		t.Fatal("building a prompt with no source material must fail")
	}
}

func TestValidateAcceptsWellFormedOutput(t *testing.T) {
	raw := `{
	  "entities": [
	    {"name":"Alice Chen","entity_type":"Person","aliases":["Alice"],
	     "mention_text":"Alice Chen","confidence":0.9}
	  ],
	  "assertions": [
	    {"subject_name":"Alice Chen","subject_type":"person","predicate":"role_at",
	     "object_entity_name":"Acme Corporation","object_kind":null,"object_value":null,
	     "scope_key":"CTO","valid_from":"2026-03-03","valid_to":null,"event_time":null,
	     "evidence_quote":"Alice Chen is the CTO of Acme Corporation.","confidence":0.85}
	  ],
	  "temporal": [
	    {"text":"3 March 2026","kind":"valid_from","resolved":"2026-03-03","confidence":0.8}
	  ]
	}`

	got, err := extraction.Validate([]byte(raw), sampleSource)
	if err != nil {
		t.Fatalf("valid output rejected: %v", err)
	}
	if len(got.Rejections) != 0 {
		t.Fatalf("nothing should have been rejected: %+v", got.Rejections)
	}
	if len(got.Result.Entities) != 1 || len(got.Result.Assertions) != 1 {
		t.Fatalf("unexpected candidate counts: %+v", got.Result)
	}

	// Entity types are normalized; names are left exactly as the source wrote them.
	if got.Result.Entities[0].EntityType != "person" {
		t.Fatalf("entity type should be normalized, got %q", got.Result.Entities[0].EntityType)
	}
	if got.Result.Entities[0].Name != "Alice Chen" {
		t.Fatal("entity names must be preserved verbatim")
	}

	claim := got.Result.Assertions[0]
	if claim.ValidFrom == nil || claim.ValidFrom.Format(domain.DateLayout) != "2026-03-03" {
		t.Fatalf("a plain date must parse into world time, got %v", claim.ValidFrom)
	}
	if claim.ObjectEntityName != "Acme Corporation" {
		t.Fatalf("relation object lost: %+v", claim)
	}
	if len(got.Result.Temporal) != 1 {
		t.Fatal("temporal hints should be retained")
	}
}

func TestValidateRejectsOutputThatIsNotUsable(t *testing.T) {
	// These are faults in the response as a whole: nothing from them may be committed.
	cases := map[string]string{
		"not json":         `{"entities": [`,
		"empty":            ``,
		"unknown field":    `{"entities":[],"assertions":[],"temporal":[],"instructions":"do this"}`,
		"wrong types":      `{"entities":"none","assertions":[],"temporal":[]}`,
		"two documents":    `{"entities":[],"assertions":[],"temporal":[]}{"entities":[]}`,
		"array not object": `[{"entities":[]}]`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := extraction.Validate([]byte(raw), sampleSource); err == nil {
				t.Fatal("unusable output must be rejected outright")
			} else if !domain.IsCode(err, domain.CodeInvalidArgument) {
				t.Fatalf("expected invalid_argument, got %s", domain.CodeOf(err))
			}
		})
	}
}

func TestValidateRejectsUngroundedClaims(t *testing.T) {
	// A quote that is not in the source means the model either invented the fact or was
	// following instructions hidden in the text. Either way the claim is discarded.
	raw := `{
	  "entities": [],
	  "assertions": [
	    {"subject_name":"Alice Chen","subject_type":"person","predicate":"salary",
	     "object_entity_name":null,"object_kind":"integer","object_value":"250000",
	     "scope_key":null,"valid_from":null,"valid_to":null,"event_time":null,
	     "evidence_quote":"Alice Chen earns 250000 per year.","confidence":0.95},
	    {"subject_name":"Alice Chen","subject_type":"person","predicate":"role_at",
	     "object_entity_name":"Acme Corporation","object_kind":null,"object_value":null,
	     "scope_key":"CTO","valid_from":null,"valid_to":null,"event_time":null,
	     "evidence_quote":"Alice Chen is the CTO of Acme Corporation.","confidence":0.9}
	  ],
	  "temporal": []
	}`

	got, err := extraction.Validate([]byte(raw), sampleSource)
	if err != nil {
		t.Fatalf("the response as a whole was well formed and should parse: %v", err)
	}
	if len(got.Result.Assertions) != 1 {
		t.Fatalf("only the grounded claim should survive, got %d", len(got.Result.Assertions))
	}
	if got.Result.Assertions[0].Predicate != "role_at" {
		t.Fatalf("the wrong claim survived: %+v", got.Result.Assertions[0])
	}
	if len(got.Rejections) != 1 {
		t.Fatalf("the fabricated claim must be reported, got %+v", got.Rejections)
	}
	if !strings.Contains(got.Rejections[0].Reason, "does not appear in the source") {
		t.Fatalf("the rejection must say why: %q", got.Rejections[0].Reason)
	}
}

func TestValidateToleratesWhitespaceDifferencesInQuotes(t *testing.T) {
	// Models re-wrap text they quote. Requiring byte equality would reject good claims,
	// so comparison collapses whitespace while still demanding the words be present.
	source := "Acme Corporation\n  supplies   industrial fasteners."
	raw := `{"entities":[],"assertions":[
	  {"subject_name":"Acme Corporation","subject_type":"organization","predicate":"supplies",
	   "object_entity_name":null,"object_kind":"string","object_value":"industrial fasteners",
	   "scope_key":null,"valid_from":null,"valid_to":null,"event_time":null,
	   "evidence_quote":"Acme Corporation supplies industrial fasteners.","confidence":0.9}
	],"temporal":[]}`

	got, err := extraction.Validate([]byte(raw), source)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if len(got.Result.Assertions) != 1 {
		t.Fatalf("a re-wrapped quote should still ground, got %+v", got.Rejections)
	}
}

func TestValidateRejectsMalformedCandidatesIndividually(t *testing.T) {
	raw := `{"entities":[{"name":"","entity_type":"person","aliases":[],"mention_text":"","confidence":0.5}],
	 "assertions":[
	  {"subject_name":"","subject_type":"person","predicate":"role_at","object_entity_name":"Acme",
	   "object_kind":null,"object_value":null,"scope_key":null,"valid_from":null,"valid_to":null,
	   "event_time":null,"evidence_quote":"Alice Chen is the CTO of Acme Corporation.","confidence":0.9},
	  {"subject_name":"Alice Chen","subject_type":"person","predicate":"joined_on",
	   "object_entity_name":null,"object_kind":"date","object_value":"2026-03-03","scope_key":null,
	   "valid_from":"not a date","valid_to":null,"event_time":null,
	   "evidence_quote":"She joined on 3 March 2026.","confidence":0.8}
	 ],"temporal":[]}`

	got, err := extraction.Validate([]byte(raw), sampleSource)
	if err != nil {
		t.Fatalf("the envelope is well formed and should parse: %v", err)
	}
	if len(got.Result.Entities) != 0 || len(got.Result.Assertions) != 0 {
		t.Fatal("candidates that fail validation must not survive")
	}
	if len(got.Rejections) != 3 {
		t.Fatalf("every rejection must be reported, got %d: %+v", len(got.Rejections), got.Rejections)
	}
}

func TestValidateBoundsResponseSize(t *testing.T) {
	// A model must not be able to exhaust memory or flood the ledger.
	huge := `{"entities":[],"assertions":[],"temporal":[],"pad":"` +
		strings.Repeat("x", extraction.MaxRawResponseBytes) + `"}`
	if _, err := extraction.Validate([]byte(huge), sampleSource); err == nil {
		t.Fatal("an oversized response must be rejected")
	}

	var many strings.Builder
	many.WriteString(`{"entities":[`)
	for i := 0; i < domain.MaxCandidatesPerRequest+1; i++ {
		if i > 0 {
			many.WriteString(",")
		}
		many.WriteString(`{"name":"E","entity_type":"thing","aliases":[],"mention_text":"E","confidence":0.5}`)
	}
	many.WriteString(`],"assertions":[],"temporal":[]}`)
	if _, err := extraction.Validate([]byte(many.String()), sampleSource); err == nil {
		t.Fatal("a response with too many candidates must be rejected")
	}
}

func TestValidateClampsConfidence(t *testing.T) {
	// An out-of-range score says the number is unreliable, which clamping reflects; it is
	// not a reason to discard an otherwise grounded claim.
	raw := `{"entities":[],"assertions":[
	  {"subject_name":"Alice Chen","subject_type":"person","predicate":"role_at",
	   "object_entity_name":"Acme Corporation","object_kind":null,"object_value":null,
	   "scope_key":null,"valid_from":null,"valid_to":null,"event_time":null,
	   "evidence_quote":"Alice Chen is the CTO of Acme Corporation.","confidence":42}
	],"temporal":[]}`

	got, err := extraction.Validate([]byte(raw), sampleSource)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if len(got.Result.Assertions) != 1 || got.Result.Assertions[0].Confidence != 1 {
		t.Fatalf("confidence should be clamped to 1, got %+v", got.Result.Assertions)
	}
}

func TestHashesAreStableAndDistinct(t *testing.T) {
	a, err := extraction.BuildPrompt([]extraction.SourceUnit{{Content: "one"}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	b, err := extraction.BuildPrompt([]extraction.SourceUnit{{Content: "two"}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	if llm.HashRequest(a.Request) != llm.HashRequest(a.Request) {
		t.Fatal("request hashes must be stable")
	}
	if llm.HashRequest(a.Request) == llm.HashRequest(b.Request) {
		t.Fatal("different prompts must hash differently")
	}
	if llm.HashResponse([]byte("x")) == llm.HashResponse([]byte("y")) {
		t.Fatal("different responses must hash differently")
	}
}
