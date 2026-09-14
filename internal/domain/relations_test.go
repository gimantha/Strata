package domain

import "testing"

func TestContextQualifiersAreCanonicalAndReadable(t *testing.T) {
	encoded, err := EncodeContextQualifiers(map[string]string{
		"Decision Phase": "security review", "Project": "Project-042",
	})
	if err != nil {
		t.Fatal(err)
	}
	if encoded != "decision_phase=security+review;project=Project-042" {
		t.Fatalf("encoded qualifiers = %q", encoded)
	}
	if got := DisplayContextQualifiers(encoded); got != "decision phase security review, project Project-042" {
		t.Fatalf("display = %q", got)
	}
	if got := ContextQualifiers("legacy-project")["context"]; got != "legacy-project" {
		t.Fatalf("legacy context = %q", got)
	}
}
