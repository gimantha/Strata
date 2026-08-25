// Package natstest provides a real NATS JetStream server for integration tests.
//
// Real, never a mock, for the same reason integration tests use real PostgreSQL: the
// behavior under test is delivery semantics — queue groups, redelivery, acknowledgement,
// backpressure — and a fake would reproduce whatever the author already believed.
//
// A server is resolved in this order:
//
//  1. TEST_NATS_URL, if set (CI service container, or a shared dev server).
//  2. A server already listening on 127.0.0.1:14222 — the port scripts/dev-nats.sh uses.
//  3. Otherwise the test skips, unless CG_REQUIRE_NATS=1 turns the skip into a failure so
//     CI cannot silently lose the distributed coverage.
package natstest

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// devPort is the port scripts/dev-nats.sh publishes. Deliberately not 4222: a developer's
// own NATS server on the default port is not this project's to create streams in.
const devPort = "14222"

var (
	resolveOnce sync.Once
	resolvedURL string
	resolveErr  error
)

// URL returns a reachable JetStream server, skipping or failing when none is available.
func URL(t *testing.T) string {
	t.Helper()

	resolveOnce.Do(resolve)
	if resolveErr != nil {
		if os.Getenv("CG_REQUIRE_NATS") == "1" {
			t.Fatalf("CG_REQUIRE_NATS=1 but no NATS server is available: %v", resolveErr)
		}
		t.Skipf("skipping distributed test: no NATS server available (%v)", resolveErr)
	}
	return resolvedURL
}

// Available reports whether a server can be reached, without failing a test.
func Available() bool {
	resolveOnce.Do(resolve)
	return resolveErr == nil
}

func resolve() {
	candidates := []string{}
	if url := os.Getenv("TEST_NATS_URL"); url != "" {
		candidates = append(candidates, url)
	}
	candidates = append(candidates, "nats://127.0.0.1:"+devPort)

	for _, url := range candidates {
		if err := probe(url); err == nil {
			resolvedURL = url
			return
		}
	}
	resolveErr = fmt.Errorf("no JetStream server on %v; run scripts/dev-nats.sh start "+
		"or set TEST_NATS_URL", candidates)
}

// probe checks that a server is reachable and has JetStream enabled.
//
// Both, because a NATS server without JetStream accepts connections perfectly well and then
// fails every stream operation — the same trap as a PostgreSQL server without pgvector.
func probe(url string) error {
	conn, err := nats.Connect(url, nats.Timeout(2*time.Second))
	if err != nil {
		return err
	}
	defer conn.Close()

	stream, err := jetstream.New(conn)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if _, err := stream.AccountInfo(ctx); err != nil {
		return fmt.Errorf("JetStream is not enabled: %w", err)
	}
	return nil
}

// Stream returns a stream name unique to this test.
//
// Necessary because `go test ./...` runs packages in parallel against one server, and a
// work-queue stream is exclusive: two packages sharing a name would delete and recreate
// each other's stream mid-test, which looks like a delivery bug and is really a shared
// fixture. Deriving the name from the test also makes a leftover stream traceable.
func Stream(t *testing.T) string {
	t.Helper()

	cleaned := strings.Map(func(r rune) rune {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			return r
		default:
			return '_'
		}
	}, t.Name())

	// NATS stream names have a length limit well below what a nested subtest name can
	// reach, and the process id keeps two runs of the same test apart.
	if len(cleaned) > 48 {
		cleaned = cleaned[:48]
	}
	name := fmt.Sprintf("TEST_%s_%d", cleaned, os.Getpid())

	resolveOnce.Do(resolve)
	if resolveErr == nil {
		url := resolvedURL
		t.Cleanup(func() { deleteStream(url, name) })
	}
	return name
}

// deleteStream removes a stream once its test is finished.
//
// A work-queue stream that outlived its test would hold undelivered messages on a shared
// server forever, and the next run of the same test would inherit them.
func deleteStream(url, stream string) {
	conn, err := nats.Connect(url, nats.Timeout(2*time.Second))
	if err != nil {
		return
	}
	defer conn.Close()

	js, err := jetstream.New(conn)
	if err != nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = js.DeleteStream(ctx, stream)
}
