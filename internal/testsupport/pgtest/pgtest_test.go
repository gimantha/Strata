package pgtest

import (
	"strings"
	"testing"
)

func TestMajorVersionHandlesEachPlatformsNaming(t *testing.T) {
	cases := map[string]int{
		"/usr/lib/postgresql/16":                              16,
		"/usr/lib/postgresql/9":                               9,
		"/opt/homebrew/opt/postgresql@16":                     16,
		"/usr/local/opt/postgresql@14":                        14,
		"/opt/homebrew/Cellar/postgresql@16/16.2":             16,
		"/Applications/Postgres.app/Contents/Versions/16":     16,
		"/Applications/Postgres.app/Contents/Versions/latest": 0,
		"/usr/lib/postgresql/not-a-version":                   0,
		"":                                                    0,
	}
	for root, want := range cases {
		if got := majorVersion(root); got != want {
			t.Fatalf("majorVersion(%q) = %d, want %d", root, got, want)
		}
	}
}

func TestMajorVersionOrdersNewestHighest(t *testing.T) {
	// The discovery loop picks the highest version, so ordering must be numeric rather
	// than lexicographic: "9" must not beat "16".
	if majorVersion("/opt/homebrew/opt/postgresql@9") >= majorVersion("/opt/homebrew/opt/postgresql@16") {
		t.Fatal("version comparison must be numeric, not lexicographic")
	}
}

func TestServerBinaryRootsCoverTheSupportedPlatforms(t *testing.T) {
	// A missing pattern silently degrades to skipped integration tests on that platform,
	// which is the failure mode this list exists to prevent.
	required := []string{
		"/usr/lib/postgresql/*",
		"/opt/homebrew/opt/postgresql@*",
		"/usr/local/opt/postgresql@*",
		"/Applications/Postgres.app/Contents/Versions/*",
	}
	for _, want := range required {
		found := false
		for _, pattern := range serverBinaryRoots {
			if pattern == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("discovery is missing the %s pattern", want)
		}
	}
}

// TestLocalCandidatesNeverProbeTheDefaultPort guards a safety property, not a feature.
//
// The harness creates and drops databases on whatever it connects to. Probing 5432 would
// point that at whatever a developer happens to be running locally, which on most machines
// is a real database. Every candidate must be a port this repository itself told someone to
// start a server on.
func TestLocalCandidatesNeverProbeTheDefaultPort(t *testing.T) {
	for _, dsn := range localCandidates {
		if strings.Contains(dsn, ":5432/") {
			t.Fatalf("candidate %q probes the default PostgreSQL port", dsn)
		}
		if !strings.Contains(dsn, "127.0.0.1") {
			t.Fatalf("candidate %q reaches beyond the local machine", dsn)
		}
	}
}

func TestDisplayHostOmitsCredentials(t *testing.T) {
	got := displayHost("postgres://postgres:hunter2@127.0.0.1:55433/postgres?sslmode=disable")
	if strings.Contains(got, "hunter2") {
		t.Fatalf("the discovery notice would print a password: %q", got)
	}
	if got != "127.0.0.1:55433" {
		t.Fatalf("expected host:port, got %q", got)
	}
}
