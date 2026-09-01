package config

import (
	"strings"
	"testing"
	"time"
)

func TestRetentionConfiguration(t *testing.T) {
	base := map[string]string{
		"CG_DATABASE_URL": "postgres://localhost/strata",
		"CG_BLOB_DIR":     "/tmp/blobs",
	}
	load := func(overrides map[string]string) (Config, error) {
		env := map[string]string{}
		for k, v := range base {
			env[k] = v
		}
		for k, v := range overrides {
			env[k] = v
		}
		return LoadFrom(func(key string) string { return env[key] })
	}

	t.Run("the default keeps everything", func(t *testing.T) {
		// Not "keeps for a year". A system whose subject is provenance should not begin
		// deleting an operator's records because it was upgraded.
		cfg, err := load(nil)
		if err != nil {
			t.Fatalf("the default configuration is invalid: %v", err)
		}
		if cfg.RetentionTraces != 0 || cfg.RetentionOutbox != 0 ||
			cfg.RetentionAudit != 0 || cfg.RetentionPipelineRuns != 0 {
			t.Error("a fresh configuration would delete records")
		}
		if cfg.RetentionInterval != time.Hour {
			t.Errorf("sweep interval = %s, want 1h", cfg.RetentionInterval)
		}
	})

	t.Run("a negative window is refused", func(t *testing.T) {
		// Zero already means forever, so a negative value is a mistake rather than a
		// stronger version of it — and a cutoff in the future would delete everything.
		_, err := load(map[string]string{"CG_RETENTION_TRACES": "-1h"})
		if err == nil {
			t.Fatal("a negative retention window was accepted")
		}
		if !strings.Contains(err.Error(), "CG_RETENTION_TRACES") {
			t.Errorf("the error does not name the setting: %v", err)
		}
	})

	t.Run("a non-positive sweep interval is refused", func(t *testing.T) {
		for _, value := range []string{"0", "-5m"} {
			if _, err := load(map[string]string{"CG_RETENTION_INTERVAL": value}); err == nil {
				t.Errorf("interval %q was accepted; the sweeper would spin", value)
			}
		}
	})

	t.Run("a configured window is read", func(t *testing.T) {
		cfg, err := load(map[string]string{"CG_RETENTION_TRACES": "720h"})
		if err != nil {
			t.Fatalf("valid retention was rejected: %v", err)
		}
		if cfg.RetentionTraces != 720*time.Hour {
			t.Errorf("traces retention = %s, want 720h", cfg.RetentionTraces)
		}
	})
}
