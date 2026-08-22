package config

import (
	"strings"
	"testing"
	"time"

	"github.com/gimantha/strata/internal/domain"
)

func envFrom(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoadAppliesDefaults(t *testing.T) {
	cfg, err := LoadFrom(envFrom(map[string]string{
		"CG_DATABASE_URL": "postgres://localhost/strata",
	}))
	if err != nil {
		t.Fatalf("load with only the required key must succeed: %v", err)
	}
	if cfg.HTTPAddr != ":8080" || cfg.WorkerConcurrency != 4 || cfg.PipelineVersion != 1 {
		t.Fatalf("defaults not applied: %+v", cfg)
	}
	if cfg.EmbeddedWorker {
		t.Fatal("the embedded worker must be opt-in")
	}
	if cfg.WorkerLease != 60*time.Second {
		t.Fatalf("unexpected default lease: %s", cfg.WorkerLease)
	}
}

func TestLoadRequiresDatabaseURL(t *testing.T) {
	_, err := LoadFrom(envFrom(nil))
	if err == nil {
		t.Fatal("a missing database URL must fail fast at startup")
	}
	if !domain.IsCode(err, domain.CodeInvalidArgument) {
		t.Fatalf("expected invalid_argument, got %s", domain.CodeOf(err))
	}
}

func TestLoadReportsEveryProblemAtOnce(t *testing.T) {
	_, err := LoadFrom(envFrom(map[string]string{
		"CG_DATABASE_URL":     "postgres://localhost/strata",
		"CG_LOG_LEVEL":        "chatty",
		"CG_LOG_FORMAT":       "yaml",
		"CG_DB_MAX_CONNS":     "0",
		"CG_PIPELINE_VERSION": "0",
	}))
	if err == nil {
		t.Fatal("expected validation to fail")
	}
	msg := err.Error()
	for _, want := range []string{"CG_LOG_LEVEL", "CG_LOG_FORMAT", "CG_DB_MAX_CONNS", "CG_PIPELINE_VERSION"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error should mention %s: %s", want, msg)
		}
	}
}

func TestLoadRejectsMalformedValues(t *testing.T) {
	_, err := LoadFrom(envFrom(map[string]string{
		"CG_DATABASE_URL":    "postgres://localhost/strata",
		"CG_WORKER_LEASE":    "sixty",
		"CG_MAX_BODY_BYTES":  "lots",
		"CG_EMBEDDED_WORKER": "perhaps",
	}))
	if err == nil {
		t.Fatal("malformed values must be rejected, not silently defaulted")
	}
	for _, want := range []string{"CG_WORKER_LEASE", "CG_MAX_BODY_BYTES", "CG_EMBEDDED_WORKER"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error should mention %s: %s", want, err.Error())
		}
	}
}

func TestValidateRejectsLeaseShorterThanPollInterval(t *testing.T) {
	_, err := LoadFrom(envFrom(map[string]string{
		"CG_DATABASE_URL":         "postgres://localhost/strata",
		"CG_WORKER_LEASE":         "1s",
		"CG_WORKER_POLL_INTERVAL": "5s",
	}))
	if err == nil {
		t.Fatal("a lease shorter than the poll interval would cause endless redelivery")
	}
}

func TestValidateRejectsOverlapAtLeastChunkSize(t *testing.T) {
	_, err := LoadFrom(envFrom(map[string]string{
		"CG_DATABASE_URL":         "postgres://localhost/strata",
		"CG_CHUNK_MAX_TOKENS":     "100",
		"CG_CHUNK_OVERLAP_TOKENS": "100",
	}))
	if err == nil {
		t.Fatal("overlap equal to chunk size would never advance through the text")
	}
}

func TestRedactedHidesCredentials(t *testing.T) {
	cfg, err := LoadFrom(envFrom(map[string]string{
		"CG_DATABASE_URL": "postgres://strata:sup3rsecret@db.internal:5432/strata?sslmode=require",
	}))
	if err != nil {
		t.Fatalf("load failed: %v", err)
	}
	got, _ := cfg.Redacted()["database"].(string)
	if strings.Contains(got, "sup3rsecret") {
		t.Fatalf("redacted config still contains the password: %s", got)
	}
	if !strings.Contains(got, "db.internal:5432") {
		t.Fatalf("redaction must keep the host for diagnostics: %s", got)
	}
}

func TestRedactDSNEdgeCases(t *testing.T) {
	cases := map[string]string{
		"":                      "",
		"not-a-url":             "[redacted]",
		"postgres://host/db":    "postgres://host/db",
		"postgres://u:p@h/db":   "postgres://[redacted]@h/db",
		"postgres://u:p@w@h/db": "postgres://[redacted]@h/db",
	}
	for in, want := range cases {
		if got := redactDSN(in); got != want {
			t.Fatalf("redactDSN(%q) = %q, want %q", in, got, want)
		}
	}
}
