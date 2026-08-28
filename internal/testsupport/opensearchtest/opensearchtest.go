// Package opensearchtest provides a real OpenSearch cluster for integration tests.
//
// Real, never a mock, for the reason ADR 0020 gives: the behaviour under test is another
// engine's analyzer and filter semantics, and a fake would reproduce whatever its author
// believed those were — which is the belief the conformance suite exists to check.
//
// A cluster is resolved in this order:
//
//  1. TEST_OPENSEARCH_URL, if set.
//  2. A cluster on 127.0.0.1:19200 — what scripts/dev-opensearch.sh publishes.
//  3. Otherwise the test skips, unless CG_REQUIRE_OPENSEARCH=1 turns the skip into a
//     failure.
package opensearchtest

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"
)

// devURL is what scripts/dev-opensearch.sh publishes. Not 9200: a developer's own cluster
// should not have this project's indices created and dropped in it.
const devURL = "http://127.0.0.1:19200"

var (
	resolveOnce sync.Once
	resolved    string
	resolveErr  error
)

// URL returns a reachable cluster, skipping or failing when none is available.
func URL(t testing.TB) string {
	t.Helper()

	resolveOnce.Do(resolve)
	if resolveErr != nil {
		if os.Getenv("CG_REQUIRE_OPENSEARCH") == "1" {
			t.Fatalf("CG_REQUIRE_OPENSEARCH=1 but no OpenSearch is available: %v", resolveErr)
		}
		t.Skipf("skipping lexical-backend test: no OpenSearch available (%v)", resolveErr)
	}
	return resolved
}

// Available reports whether a cluster can be reached, without failing a test.
func Available() bool {
	resolveOnce.Do(resolve)
	return resolveErr == nil
}

func resolve() {
	candidates := []string{}
	if configured := os.Getenv("TEST_OPENSEARCH_URL"); configured != "" {
		candidates = append(candidates, configured)
	}
	candidates = append(candidates, devURL)

	for _, candidate := range candidates {
		if err := probe(candidate); err == nil {
			resolved = candidate
			return
		}
	}
	resolveErr = fmt.Errorf("nothing listening on %v; run scripts/dev-opensearch.sh start "+
		"or set TEST_OPENSEARCH_URL", candidates)
}

func probe(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		return err
	}
	port := parsed.Port()
	if port == "" {
		port = "80"
		if parsed.Scheme == "https" {
			port = "443"
		}
	}
	conn, err := net.DialTimeout("tcp",
		net.JoinHostPort(parsed.Hostname(), port), 3*time.Second)
	if err != nil {
		return err
	}
	return conn.Close()
}

// Index returns an index name unique to this test.
//
// One per test, because a shared index would let one test's documents answer another's
// queries — a dirty fixture that reads as a filter bug. Lowercase, because OpenSearch
// rejects an index name that is not.
func Index(t testing.TB) string {
	t.Helper()

	cleaned := make([]rune, 0, len(t.Name()))
	for _, r := range t.Name() {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			cleaned = append(cleaned, r)
		case r >= 'A' && r <= 'Z':
			cleaned = append(cleaned, r+('a'-'A'))
		default:
			cleaned = append(cleaned, '_')
		}
	}
	name := fmt.Sprintf("t_%s_%s", string(cleaned), strconv.Itoa(os.Getpid()))
	if len(name) > 200 {
		name = name[:200]
	}
	return name
}
