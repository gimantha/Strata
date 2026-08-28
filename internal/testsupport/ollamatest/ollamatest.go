// Package ollamatest provides a real local model for tests that need one.
//
// Real, and deliberately open-weights. Every other test of the planner supplies the model's
// answer, which is the right way to test what the planner does with an answer and no way at
// all to find out what a model actually returns. The two differ: the first real reply this
// package produced labelled every sub-query "decomposed" and omitted the question as asked,
// a case each scripted test had been written to include.
//
// This harness inverts the convention the storage harnesses use, and the difference is
// deliberate. There, a service that happens to be running is a reason to test against it,
// and CG_REQUIRE_X=1 turns an absent one into a failure. Here, tests run only when
// CG_REQUIRE_OLLAMA=1 says so, even if a server is sitting there ready.
//
// The reason is that these tests assert something weaker than the rest of the suite. What a
// model decides is not a property of this repository, so a bare `go test ./...` that quietly
// became dependent on a daemon being up — and on that model's judgment today — would make
// the gate mean less, not more, and would fail for reasons no change to the code caused.
//
// The cost is that `go test ./...` reports these as skipped unless asked for. That is the
// honest report: they did not run.
//
// With CG_REQUIRE_OLLAMA=1, a server is resolved in this order:
//
//  1. TEST_OLLAMA_URL, if set.
//  2. A server already listening on 127.0.0.1:11434 — the ollama default — carrying the
//     model named by TEST_OLLAMA_MODEL, or DefaultModel when that is unset.
//
// A missing server is then a failure rather than a skip. Run it with scripts/dev-ollama.sh.
package ollamatest

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// DefaultModel is small enough to run on a laptop and competent enough at structured output
// to exercise the planner. Any instruct-tuned model with JSON support will do.
const DefaultModel = "qwen2.5:3b"

const defaultURL = "http://127.0.0.1:11434"

var (
	resolveOnce sync.Once
	resolved    Server
	resolveErr  error
)

// Server is a reachable model endpoint.
type Server struct {
	// BaseURL is the OpenAI-compatible base, ready for openai.Config.
	BaseURL string
	Model   string
}

// Resolve returns a reachable server, skipping or failing when none is available.
func Resolve(t testing.TB) Server {
	t.Helper()

	if os.Getenv("CG_REQUIRE_OLLAMA") != "1" {
		t.Skip("live model tests are opt-in: set CG_REQUIRE_OLLAMA=1 " +
			"(see scripts/dev-ollama.sh)")
	}
	resolveOnce.Do(resolve)
	if resolveErr != nil {
		t.Fatalf("CG_REQUIRE_OLLAMA=1 but no model is available: %v "+
			"(try: scripts/dev-ollama.sh start)", resolveErr)
	}
	return resolved
}

func resolve() {
	root := strings.TrimRight(os.Getenv("TEST_OLLAMA_URL"), "/")
	if root == "" {
		root = defaultURL
	}
	model := os.Getenv("TEST_OLLAMA_MODEL")
	if model == "" {
		model = DefaultModel
	}

	host := strings.TrimPrefix(strings.TrimPrefix(root, "http://"), "https://")
	if _, err := net.DialTimeout("tcp", host, 2*time.Second); err != nil {
		resolveErr = fmt.Errorf("nothing listening on %s: %w", host, err)
		return
	}

	// Reachable is not the same as loaded: a server with no model pulled answers every
	// request with a 404 that would otherwise read as a planning failure.
	if err := hasModel(root, model); err != nil {
		resolveErr = err
		return
	}
	resolved = Server{BaseURL: root + "/v1", Model: model}
}

func hasModel(root, model string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, root+"/v1/models", nil)
	if err != nil {
		return err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return fmt.Errorf("listing models: %w", err)
	}
	defer func() { _ = response.Body.Close() }()

	var listing struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(response.Body).Decode(&listing); err != nil {
		return fmt.Errorf("reading the model list: %w", err)
	}
	for _, entry := range listing.Data {
		if entry.ID == model || strings.HasPrefix(entry.ID, model+":") {
			return nil
		}
	}
	return fmt.Errorf("model %q is not pulled (try: ollama pull %s)", model, model)
}
