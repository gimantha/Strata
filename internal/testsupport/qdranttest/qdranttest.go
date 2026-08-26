// Package qdranttest provides a real Qdrant server for integration tests.
//
// Real, never a mock, for the reason ADR 0020 gives and this package proves: the behaviour
// under test is another engine's filter semantics, and a fake would reproduce whatever its
// author believed those were — which is exactly the belief the conformance suite exists to
// check.
//
// A server is resolved in this order:
//
//  1. TEST_QDRANT_HOST/TEST_QDRANT_PORT, if set.
//  2. A server already listening on 127.0.0.1:16334 — what scripts/dev-qdrant.sh publishes.
//  3. Otherwise the test skips, unless CG_REQUIRE_QDRANT=1 turns the skip into a failure.
package qdranttest

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"
)

// devPort is what scripts/dev-qdrant.sh publishes for gRPC. Deliberately not 6334: a
// developer's own Qdrant should not have this project's collections created and dropped in
// it, the same reasoning the PostgreSQL, NATS and MinIO harnesses use.
const devPort = 16334

var (
	resolveOnce sync.Once
	resolvedTo  address
	resolveErr  error
)

type address struct {
	Host string
	Port int
}

// Address returns a reachable server, skipping or failing when none is available.
func Address(t testing.TB) (string, int) {
	t.Helper()

	resolveOnce.Do(resolve)
	if resolveErr != nil {
		if os.Getenv("CG_REQUIRE_QDRANT") == "1" {
			t.Fatalf("CG_REQUIRE_QDRANT=1 but no Qdrant is available: %v", resolveErr)
		}
		t.Skipf("skipping vector-backend test: no Qdrant available (%v)", resolveErr)
	}
	return resolvedTo.Host, resolvedTo.Port
}

// Available reports whether a server can be reached, without failing a test.
func Available() bool {
	resolveOnce.Do(resolve)
	return resolveErr == nil
}

func resolve() {
	candidates := []address{}
	if host := os.Getenv("TEST_QDRANT_HOST"); host != "" {
		port := devPort
		if raw := os.Getenv("TEST_QDRANT_PORT"); raw != "" {
			if parsed, err := strconv.Atoi(raw); err == nil {
				port = parsed
			}
		}
		candidates = append(candidates, address{Host: host, Port: port})
	}
	candidates = append(candidates, address{Host: "127.0.0.1", Port: devPort})

	for _, candidate := range candidates {
		conn, err := net.DialTimeout("tcp",
			net.JoinHostPort(candidate.Host, strconv.Itoa(candidate.Port)), 2*time.Second)
		if err == nil {
			_ = conn.Close()
			resolvedTo = candidate
			return
		}
	}
	resolveErr = fmt.Errorf("nothing listening on %v; run scripts/dev-qdrant.sh start "+
		"or set TEST_QDRANT_HOST", candidates)
}

// Collection returns a collection name unique to this test.
//
// One per test, because provisioning payload indexes takes seconds and a shared collection
// would let one test's points answer another's queries — a dirty fixture that reads as a
// filter bug.
func Collection(t testing.TB) string {
	t.Helper()

	cleaned := make([]rune, 0, len(t.Name()))
	for _, r := range t.Name() {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			cleaned = append(cleaned, r)
		default:
			cleaned = append(cleaned, '_')
		}
	}
	name := fmt.Sprintf("t_%s_%d", string(cleaned), os.Getpid())
	if len(name) > 60 {
		name = name[:60]
	}
	return name
}
