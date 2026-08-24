package mcp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/gimantha/strata/internal/api/mcp"
	"github.com/gimantha/strata/internal/contextblock"
	"github.com/gimantha/strata/internal/domain"
	"github.com/gimantha/strata/internal/embedding/hashing"
	"github.com/gimantha/strata/internal/ingest"
	"github.com/gimantha/strata/internal/knowledge"
	"github.com/gimantha/strata/internal/normalize"
	"github.com/gimantha/strata/internal/pipeline"
	"github.com/gimantha/strata/internal/policy"
	"github.com/gimantha/strata/internal/projection"
	"github.com/gimantha/strata/internal/retrieval"
	"github.com/gimantha/strata/internal/store/blob"
	"github.com/gimantha/strata/internal/testsupport/pgtest"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

// client drives a server the way an MCP client does: one JSON-RPC message per line, replies
// read back in order.
type client struct {
	t      *testing.T
	server *mcp.Server
	nextID int
}

type harness struct {
	fixture   *pgtest.Fixture
	gateway   *ingest.Gateway
	runner    *pipeline.Runner
	service   *knowledge.Service
	projector *projection.Projector
	client    *client
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	f := pgtest.NewFixture(t)
	blobs, err := blob.NewFS(t.TempDir())
	if err != nil {
		t.Fatalf("blob store: %v", err)
	}

	embedder := hashing.New()
	projector := projection.New(f.Store, embedder, projection.Options{}, nil, nil)
	service := knowledge.New(f.Store, knowledge.Options{}, nil, nil)
	gateway := ingest.New(f.Store, blobs, ingest.Options{PipelineVersion: 1}, nil, nil, nil)
	retriever := retrieval.New(f.Store, embedder, retrieval.Options{Traces: f.Store}, nil, nil)

	stages := pipeline.DefaultStages(f.Store, blobs, pipeline.StageConfig{
		ChunkMaxTokens: 256, ChunkOverlapTokens: 16,
		Tokenizer: normalize.DefaultTokenizer, Projector: projector,
	})

	tools := mcp.NewTools(mcp.Deps{
		Gateway:   gateway,
		Retriever: retriever,
		Assembler: contextblock.New(retriever, f.Store, contextblock.Options{}, nil, nil),
		Policy:    policy.New(f.Store, policy.DiscardAuditor{}, policy.Options{}, nil),
		Ledger:    f.Store,
	}, f.Primary.Principal, f.Primary.Scope(), f.Primary.Source.Name)

	return &harness{
		fixture:   f,
		gateway:   gateway,
		runner:    pipeline.NewRunner(f.Store, 1, stages, nil, nil, nil),
		service:   service,
		projector: projector,
		client:    &client{t: t, server: mcp.NewServer(tools, nil)},
	}
}

// call sends one request and returns the decoded reply.
func (c *client) call(method string, params any) map[string]any {
	c.t.Helper()

	c.nextID++
	request := map[string]any{"jsonrpc": "2.0", "id": c.nextID, "method": method}
	if params != nil {
		request["params"] = params
	}

	encoded, err := json.Marshal(request)
	if err != nil {
		c.t.Fatalf("encode request: %v", err)
	}

	var out bytes.Buffer
	if err := c.server.Serve(context.Background(),
		bytes.NewReader(append(encoded, '\n')), &out); err != nil && err != io.EOF {
		c.t.Fatalf("serve: %v", err)
	}

	line := strings.TrimSpace(out.String())
	if line == "" {
		c.t.Fatalf("%s produced no reply", method)
	}

	var response map[string]any
	if err := json.Unmarshal([]byte(line), &response); err != nil {
		c.t.Fatalf("decode reply to %s: %v (%q)", method, err, line)
	}
	if response["jsonrpc"] != "2.0" {
		c.t.Fatalf("reply is not JSON-RPC 2.0: %q", line)
	}
	return response
}

// callTool invokes one tool and returns the decoded JSON payload it produced.
func (c *client) callTool(name string, args map[string]any) map[string]any {
	c.t.Helper()

	response := c.call("tools/call", map[string]any{"name": name, "arguments": args})
	if errObj, failed := response["error"]; failed {
		c.t.Fatalf("%s failed at the protocol level: %v", name, errObj)
	}

	result, _ := response["result"].(map[string]any)
	content, _ := result["content"].([]any)
	if len(content) == 0 {
		c.t.Fatalf("%s returned no content", name)
	}
	block, _ := content[0].(map[string]any)
	text, _ := block["text"].(string)

	if isError, _ := result["isError"].(bool); isError {
		c.t.Fatalf("%s reported an error: %s", name, text)
	}

	var payload map[string]any
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		c.t.Fatalf("%s did not return JSON: %v (%q)", name, err, text)
	}
	return payload
}

func (h *harness) seed(t *testing.T, text string, claim knowledge.Claim, key string) domain.Assertion {
	t.Helper()
	ctx := context.Background()
	tenant := h.fixture.Primary

	receipt, err := h.gateway.Accept(ctx, ingest.Request{
		Scope: tenant.Scope(), Principal: tenant.Principal.Ref(), SourceID: tenant.Source.ID,
		MediaType: normalize.MediaTypePlain, Payload: []byte(text), IdempotencyKey: key,
		// The document carries the claim's sensitivity. Ingesting a restricted fact from
		// an internal document is a legitimate configuration, but it is not the case this
		// test is about, and seeding it that way would test the passage rather than the
		// claim.
		Classification: claim.Classification,
	})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if _, err := h.runner.Process(ctx, tenant.Workspace.ID, receipt.SourceEventID, false); err != nil {
		t.Fatalf("process: %v", err)
	}

	episodes, err := h.fixture.Store.ListEpisodes(ctx, tenant.Workspace.ID, receipt.SourceEventID)
	if err != nil || len(episodes) == 0 {
		t.Fatalf("episodes: %v", err)
	}
	claim.Evidence = []knowledge.EvidenceInput{{EpisodeID: episodes[0].ID, ExtractedText: text}}

	result, err := h.service.Assert(ctx, knowledge.AssertRequest{
		Scope: tenant.Scope(), Principal: tenant.Principal.Ref(),
		SourceEventID: receipt.SourceEventID, Claims: []knowledge.Claim{claim},
	})
	if err != nil {
		t.Fatalf("assert: %v", err)
	}
	if _, err := h.projector.ProjectEvent(ctx, tenant.Scope(), receipt.SourceEventID); err != nil {
		t.Fatalf("project: %v", err)
	}
	if _, err := h.projector.ProjectEntities(ctx, tenant.Scope()); err != nil {
		t.Fatalf("project entities: %v", err)
	}
	return result.Assertions[0]
}

// TestIntegrationAnMCPClientCanIngestQueryAndExplain is phase 13's first acceptance criterion.
//
// It drives the server the way a client does — handshake, tool discovery, then calls — rather
// than invoking the tool functions directly, because the protocol layer is where an MCP
// integration actually breaks: a reply to a notification, a missing id, a tool schema a client
// cannot parse.
func TestIntegrationAnMCPClientCanIngestQueryAndExplain(t *testing.T) {
	h := newHarness(t)

	t.Run("handshake", func(t *testing.T) {
		response := h.client.call("initialize", map[string]any{
			"protocolVersion": mcp.ProtocolVersion,
			"clientInfo":      map[string]any{"name": "test-client", "version": "1"},
		})
		result, _ := response["result"].(map[string]any)
		if result["protocolVersion"] != mcp.ProtocolVersion {
			t.Fatalf("unexpected protocol version: %v", result["protocolVersion"])
		}
		capabilities, _ := result["capabilities"].(map[string]any)
		if _, offered := capabilities["tools"]; !offered {
			t.Fatal("the server must advertise tool support")
		}
	})

	t.Run("tool discovery", func(t *testing.T) {
		response := h.client.call("tools/list", nil)
		result, _ := response["result"].(map[string]any)
		tools, _ := result["tools"].([]any)
		if len(tools) == 0 {
			t.Fatal("no tools were advertised")
		}

		named := map[string]bool{}
		for _, entry := range tools {
			tool, _ := entry.(map[string]any)
			name, _ := tool["name"].(string)
			named[name] = true

			// A tool without a usable schema is a tool an agent cannot call correctly.
			schema, _ := tool["inputSchema"].(map[string]any)
			if schema["type"] != "object" {
				t.Fatalf("%s has no object schema", name)
			}
			if _, has := schema["properties"]; !has {
				t.Fatalf("%s declares no properties", name)
			}
			if description, _ := tool["description"].(string); description == "" {
				t.Fatalf("%s has no description", name)
			}
		}

		// The surface AGENTS.md section 26 asks for.
		for _, required := range []string{
			"context_graph_ingest", "context_graph_search", "context_graph_get_context",
			"context_graph_get_entity", "context_graph_get_assertion",
			"context_graph_explain", "context_graph_temporal_query",
		} {
			if !named[required] {
				t.Fatalf("%s is missing from the tool surface", required)
			}
		}
	})

	t.Run("notifications get no reply", func(t *testing.T) {
		// MCP clients send notifications/initialized on every connection. Answering one is
		// a protocol violation, and some clients treat it as fatal — so this breaks every
		// integration or none.
		var out bytes.Buffer
		notification := `{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n"
		if err := h.client.server.Serve(context.Background(),
			strings.NewReader(notification), &out); err != nil {
			t.Fatalf("serve: %v", err)
		}
		if strings.TrimSpace(out.String()) != "" {
			t.Fatalf("the server replied to a notification: %q", out.String())
		}
	})

	t.Run("ingest", func(t *testing.T) {
		payload := h.client.callTool("context_graph_ingest", map[string]any{
			"content":         "Thornbury Works supplies the Kelvinbridge assembly line.",
			"idempotency_key": "mcp-1",
		})
		if payload["source_event_id"] == "" {
			t.Fatal("ingest returned no event id to follow")
		}
		if payload["note"] == nil {
			// An agent that assumes its content is searchable on the next call will draw
			// the wrong conclusion from an empty result.
			t.Fatal("ingest should say that extraction is asynchronous")
		}
	})

	// Knowledge to query, recorded through the ordinary path.
	claim := h.seed(t, "Thornbury Works is on the premium tier.", knowledge.Claim{
		Subject:   knowledge.EntityRef{Name: "Thornbury Works", Type: "organization"},
		Predicate: "TIER",
		Object:    domain.ObjectOfSymbol("PREMIUM"),
	}, "mcp-seed")

	t.Run("search", func(t *testing.T) {
		payload := h.client.callTool("context_graph_search", map[string]any{
			"query": "Thornbury Works", "limit": 5, "explain": true,
		})
		results, _ := payload["results"].([]any)
		if len(results) == 0 {
			t.Fatal("search found nothing")
		}

		first, _ := results[0].(map[string]any)
		// Canonical ids, so the agent can follow a reference instead of asking for a
		// bigger payload (AGENTS.md section 26).
		if first["record_id"] == "" || first["surface"] == "" {
			t.Fatalf("a result carries no canonical reference: %v", first)
		}
		if payload["trace_id"] == nil {
			t.Fatal("search should return a trace id so the agent can ask why later")
		}
	})

	t.Run("context", func(t *testing.T) {
		payload := h.client.callTool("context_graph_get_context", map[string]any{
			"query": "Thornbury Works tier", "token_budget": 800,
		})
		block, _ := payload["context"].(string)
		if !strings.Contains(block, "CONTEXT BLOCK") {
			t.Fatalf("expected a rendered block, got %q", block)
		}

		budget, _ := payload["budget"].(map[string]any)
		used, _ := budget["used"].(float64)
		limit, _ := budget["limit"].(float64)
		if used > limit {
			t.Fatalf("the block exceeded its budget: %v of %v", used, limit)
		}
	})

	t.Run("get assertion and explain", func(t *testing.T) {
		payload := h.client.callTool("context_graph_get_assertion", map[string]any{
			"assertion_id": string(claim.ID),
		})
		if payload["predicate"] != "TIER" {
			t.Fatalf("unexpected claim: %v", payload)
		}

		explained := h.client.callTool("context_graph_explain", map[string]any{
			"assertion_id": string(claim.ID),
		})
		provenance, _ := explained["provenance"].([]any)
		if len(provenance) == 0 {
			t.Fatal("explain returned no provenance, so a fact cannot be checked")
		}
		link, _ := provenance[0].(map[string]any)
		if quote, _ := link["quote"].(string); !strings.Contains(quote, "Thornbury") {
			t.Fatalf("the evidence quote is missing: %v", link)
		}
		if link["source"] == "" {
			t.Fatal("explain should name the source")
		}
	})

	t.Run("temporal query", func(t *testing.T) {
		payload := h.client.callTool("context_graph_temporal_query", map[string]any{
			"subject": string(claim.SubjectID), "predicate": "TIER",
		})
		assertions, _ := payload["assertions"].([]any)
		if len(assertions) == 0 {
			t.Fatal("the temporal query found nothing")
		}
	})
}

// TestIntegrationMCPRefusesWhatItShould covers the protocol's error paths, which are what a
// client hits when an agent improvises.
func TestIntegrationMCPRefusesWhatItShould(t *testing.T) {
	h := newHarness(t)

	t.Run("unknown method", func(t *testing.T) {
		response := h.client.call("tools/invoke", nil)
		errObj, failed := response["error"].(map[string]any)
		if !failed {
			t.Fatal("an unknown method should be a protocol error")
		}
		if code, _ := errObj["code"].(float64); int(code) != -32601 {
			t.Fatalf("expected method-not-found, got %v", errObj)
		}
	})

	t.Run("unknown tool", func(t *testing.T) {
		response := h.client.call("tools/call", map[string]any{
			"name": "context_graph_delete_everything",
		})
		if _, failed := response["error"]; !failed {
			t.Fatal("an unknown tool should be refused")
		}
	})

	t.Run("bad arguments are a tool error, not a transport error", func(t *testing.T) {
		// The agent asked a reasonable question badly. Telling it so lets it retry;
		// failing the transport tends to make clients drop the conversation.
		response := h.client.call("tools/call", map[string]any{
			"name":      "context_graph_search",
			"arguments": map[string]any{"query": "anything", "valid_at": "last tuesday"},
		})
		if _, failed := response["error"]; failed {
			t.Fatal("a bad argument should not be a JSON-RPC error")
		}
		result, _ := response["result"].(map[string]any)
		if isError, _ := result["isError"].(bool); !isError {
			t.Fatalf("the tool should report the failure: %v", result)
		}
	})

	t.Run("malformed json", func(t *testing.T) {
		var out bytes.Buffer
		if err := h.client.server.Serve(context.Background(),
			strings.NewReader("{not json\n"), &out); err != nil {
			t.Fatalf("serve: %v", err)
		}

		var response map[string]any
		if err := json.Unmarshal([]byte(strings.TrimSpace(out.String())), &response); err != nil {
			t.Fatalf("the server should still reply with valid JSON: %v", err)
		}
		errObj, _ := response["error"].(map[string]any)
		if code, _ := errObj["code"].(float64); int(code) != -32700 {
			t.Fatalf("expected a parse error, got %v", response)
		}
	})
}

// TestIntegrationMCPHonoursPolicy checks that the transport is not a way around access
// control (AGENTS.md section 26).
func TestIntegrationMCPHonoursPolicy(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()

	restricted := h.seed(t, "Thornbury Works is under Quillon investigation.", knowledge.Claim{
		Subject:        knowledge.EntityRef{Name: "Thornbury Works", Type: "organization"},
		Predicate:      "NOTES",
		Object:         domain.ObjectOfString("under Quillon investigation"),
		Classification: domain.ClassificationRestricted,
	}, "mcp-restricted")

	// A policy that caps this principal below the claim's classification.
	policies := policy.New(h.fixture.Store, policy.DiscardAuditor{}, policy.Options{}, nil)
	if _, err := policies.Define(ctx, policy.DefineRequest{
		Scope: h.fixture.Primary.Scope(), Principal: h.fixture.Primary.Principal.Ref(),
		Name: "capped", DefaultClearance: domain.ClassificationInternal, Activate: true,
	}); err != nil {
		t.Fatalf("define policy: %v", err)
	}

	payload := h.client.callTool("context_graph_search", map[string]any{
		"query": "Quillon", "limit": 10,
	})
	results, _ := payload["results"].([]any)
	for _, entry := range results {
		result, _ := entry.(map[string]any)
		if content, _ := result["content"].(string); strings.Contains(content, "Quillon") {
			t.Fatalf("MCP returned material the principal is not cleared for: %v", result)
		}
	}

	// And by id, which is the path that skips search filtering entirely.
	response := h.client.call("tools/call", map[string]any{
		"name":      "context_graph_get_assertion",
		"arguments": map[string]any{"assertion_id": string(restricted.ID)},
	})
	result, _ := response["result"].(map[string]any)
	if isError, _ := result["isError"].(bool); !isError {
		t.Fatal("fetching a restricted claim by id should be refused")
	}
}
