// Package neo4jtest provides a real Neo4j server for integration tests.
//
// Real, never a mock, for the reason ADR 0020 gives — and more so here than for the other
// backends. A recursive CTE and a Cypher variable-length pattern are not dialects of one
// idea, so the question a fake cannot answer is whether the two produce the same walk.
//
// A server is resolved in this order:
//
//  1. TEST_NEO4J_URI, if set.
//  2. A server on 127.0.0.1:17687 — what scripts/dev-neo4j.sh publishes.
//  3. Otherwise the test skips, unless CG_REQUIRE_NEO4J=1 turns the skip into a failure.
package neo4jtest

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"
)

const (
	devURI      = "bolt://127.0.0.1:17687"
	devUser     = "neo4j"
	devPassword = "strata-secret"
)

var (
	resolveOnce sync.Once
	resolved    string
	resolveErr  error
)

// URI returns a reachable server, skipping or failing when none is available.
func URI(t testing.TB) string {
	t.Helper()

	resolveOnce.Do(resolve)
	if resolveErr != nil {
		if os.Getenv("CG_REQUIRE_NEO4J") == "1" {
			t.Fatalf("CG_REQUIRE_NEO4J=1 but no Neo4j is available: %v", resolveErr)
		}
		t.Skipf("skipping graph-backend test: no Neo4j available (%v)", resolveErr)
	}
	return resolved
}

// Credentials returns the username and password the dev server uses.
func Credentials() (string, string) {
	user := os.Getenv("TEST_NEO4J_USER")
	if user == "" {
		user = devUser
	}
	password := os.Getenv("TEST_NEO4J_PASSWORD")
	if password == "" {
		password = devPassword
	}
	return user, password
}

// Available reports whether a server can be reached, without failing a test.
func Available() bool {
	resolveOnce.Do(resolve)
	return resolveErr == nil
}

func resolve() {
	candidates := []string{}
	if configured := os.Getenv("TEST_NEO4J_URI"); configured != "" {
		candidates = append(candidates, configured)
	}
	candidates = append(candidates, devURI)

	for _, candidate := range candidates {
		if err := probe(candidate); err == nil {
			resolved = candidate
			return
		}
	}
	resolveErr = fmt.Errorf("nothing listening on %v; run scripts/dev-neo4j.sh start "+
		"or set TEST_NEO4J_URI", candidates)
}

func probe(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		return err
	}
	port := parsed.Port()
	if port == "" {
		port = "7687"
	}
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(parsed.Hostname(), port), 3*time.Second)
	if err != nil {
		return err
	}
	return conn.Close()
}

// Workspace returns a workspace identifier unique to this test.
//
// The community edition has one database, so tests share a server and are separated the way
// the port separates tenants: by workspace. A shared workspace would let one test's edges
// answer another's traversal, which reads as a filter bug and is a dirty fixture.
func Workspace(t testing.TB) string {
	t.Helper()
	return fmt.Sprintf("01a00000-0000-7000-8000-%012d", os.Getpid()%1000000000000)
}
