package config

import (
	"strings"
	"testing"
)

// TestQueryPlannerConfiguration covers the two ways an LLM planner can be misconfigured.
func TestQueryPlannerConfiguration(t *testing.T) {
	base := map[string]string{
		"CG_DATABASE_URL": "postgres://localhost/strata",
		"CG_BLOB_DIR":     "/tmp/blobs",
	}
	load := func(overrides map[string]string) error {
		env := map[string]string{}
		for k, v := range base {
			env[k] = v
		}
		for k, v := range overrides {
			env[k] = v
		}
		_, err := LoadFrom(func(key string) string { return env[key] })
		return err
	}

	t.Run("the default needs no model", func(t *testing.T) {
		if err := load(nil); err != nil {
			t.Fatalf("the default configuration is invalid: %v", err)
		}
	})

	t.Run("llm planning without a provider is refused", func(t *testing.T) {
		// Otherwise the planner would be configured, never usable, and silently fall back
		// on every query — a deployment paying attention to its config would believe it
		// had a feature it did not.
		err := load(map[string]string{"CG_QUERY_PLANNER": "llm"})
		if err == nil {
			t.Fatal("llm planning with no model was accepted")
		}
		if !strings.Contains(err.Error(), "CG_LLM_PROVIDER") {
			t.Errorf("the error does not say what is missing: %v", err)
		}
	})

	t.Run("llm planning with redacted query text is refused", func(t *testing.T) {
		// The combination is contradictory rather than merely unusual: redaction keeps the
		// words of a question out of the trace, and planning sends those same words to a
		// provider. Accepting it would defeat a compliance control silently.
		err := load(map[string]string{
			"CG_QUERY_PLANNER":     "llm",
			"CG_LLM_PROVIDER":      "mock",
			"CG_REDACT_QUERY_TEXT": "true",
		})
		if err == nil {
			t.Fatal("planning was accepted alongside query redaction")
		}
		if !strings.Contains(err.Error(), "REDACT_QUERY_TEXT") {
			t.Errorf("the error does not name the conflict: %v", err)
		}
	})

	t.Run("an unknown planner is refused", func(t *testing.T) {
		if err := load(map[string]string{"CG_QUERY_PLANNER": "telepathy"}); err == nil {
			t.Fatal("an unknown planner was accepted")
		}
	})
}
