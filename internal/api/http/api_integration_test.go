package http_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	strataapi "github.com/gimantha/strata/internal/api/http"
	"github.com/gimantha/strata/internal/config"
	"github.com/gimantha/strata/internal/domain"
	"github.com/gimantha/strata/internal/identity"
	"github.com/gimantha/strata/internal/ingest"
	"github.com/gimantha/strata/internal/knowledge"
	"github.com/gimantha/strata/internal/normalize"
	"github.com/gimantha/strata/internal/pipeline"
	"github.com/gimantha/strata/internal/store/blob"
	"github.com/gimantha/strata/internal/testsupport/pgtest"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

type apiHarness struct {
	fixture *pgtest.Fixture
	server  *httptest.Server
	runner  *pipeline.Runner
	// credentials by principal id.
	creds map[string]string
}

// keyEntry describes one credential to configure.
type keyEntry struct {
	principalID string
	secret      string
	systemRole  domain.Role
}

func newAPIHarness(t *testing.T, keys ...keyEntry) *apiHarness {
	t.Helper()
	ctx := context.Background()

	f := pgtest.NewFixture(t)

	if len(keys) == 0 {
		keys = []keyEntry{{principalID: "acme-owner", secret: "owner-secret", systemRole: domain.RoleAdmin}}
	}

	entries := make([]map[string]string, 0, len(keys))
	creds := map[string]string{}
	for _, key := range keys {
		digest := sha256.Sum256([]byte(key.secret))
		keyID := "key-" + key.principalID
		entries = append(entries, map[string]string{
			"key_id":        keyID,
			"secret_sha256": hex.EncodeToString(digest[:]),
			"principal_id":  key.principalID,
			"kind":          string(domain.PrincipalUser),
			"display_name":  key.principalID,
			"system_role":   string(key.systemRole),
		})
		creds[key.principalID] = keyID + "." + key.secret
	}

	body, err := json.Marshal(map[string]any{"version": 1, "keys": entries})
	if err != nil {
		t.Fatalf("encode key file: %v", err)
	}
	keyPath := filepath.Join(t.TempDir(), "api-keys.json")
	if err := os.WriteFile(keyPath, body, 0o600); err != nil {
		t.Fatalf("write key file: %v", err)
	}

	identityService, err := identity.Load(ctx, keyPath, f.Store, nil)
	if err != nil {
		t.Fatalf("load identity: %v", err)
	}
	if err := identityService.SyncPrincipals(ctx); err != nil {
		t.Fatalf("sync principals: %v", err)
	}

	cfg, err := config.LoadFrom(func(key string) string {
		if key == "CG_DATABASE_URL" {
			return "postgres://unused/for-defaults"
		}
		return ""
	})
	if err != nil {
		t.Fatalf("build config: %v", err)
	}

	blobs, err := blob.NewFS(t.TempDir())
	if err != nil {
		t.Fatalf("create blob store: %v", err)
	}
	gateway := ingest.New(f.Store, blobs, ingest.Options{PipelineVersion: 1}, nil, nil, nil)

	server := strataapi.NewServer(strataapi.Deps{
		Config:    cfg,
		Logger:    discardLogger(),
		Identity:  identityService,
		Ledger:    f.Store,
		Gateway:   gateway,
		Knowledge: knowledge.New(f.Store, knowledge.Options{}, nil, nil),
		Blobs:     blobs,
	})

	ts := httptest.NewServer(server.Handler())
	t.Cleanup(ts.Close)

	runner := pipeline.NewRunner(f.Store, 1, pipeline.DefaultStages(f.Store, blobs, pipeline.StageConfig{
		ChunkMaxTokens: 64, ChunkOverlapTokens: 8,
	}), nil, nil, nil)

	return &apiHarness{fixture: f, server: ts, runner: runner, creds: creds}
}

// do issues a request, optionally authenticated as a principal.
func (h *apiHarness) do(t *testing.T, method, path, principalID string, body any, headers map[string]string) (int, []byte) {
	t.Helper()

	var reader io.Reader
	switch payload := body.(type) {
	case nil:
	case string:
		reader = bytes.NewBufferString(payload)
	case []byte:
		reader = bytes.NewReader(payload)
	default:
		encoded, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("encode body: %v", err)
		}
		reader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequest(method, h.server.URL+path, reader)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if principalID != "" {
		cred, ok := h.creds[principalID]
		if !ok {
			t.Fatalf("no credential configured for %s", principalID)
		}
		req.Header.Set("Authorization", "Bearer "+cred)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := h.server.Client().Do(req)
	if err != nil {
		t.Fatalf("request %s %s: %v", method, path, err)
	}
	defer resp.Body.Close()

	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	return resp.StatusCode, responseBody
}

func decode(t *testing.T, body []byte) map[string]any {
	t.Helper()

	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode response %q: %v", body, err)
	}
	return out
}

func errorCode(t *testing.T, body []byte) string {
	t.Helper()

	parsed := decode(t, body)
	envelope, ok := parsed["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected an error envelope, got %s", body)
	}
	code, _ := envelope["code"].(string)
	return code
}

func TestIntegrationHealthAndReadiness(t *testing.T) {
	h := newAPIHarness(t)

	status, body := h.do(t, http.MethodGet, "/healthz", "", nil, nil)
	if status != http.StatusOK {
		t.Fatalf("healthz returned %d: %s", status, body)
	}

	// Readiness must actually check dependencies, not just report ok.
	status, body = h.do(t, http.MethodGet, "/readyz", "", nil, nil)
	if status != http.StatusOK {
		t.Fatalf("readyz returned %d: %s", status, body)
	}
	parsed := decode(t, body)
	checks, ok := parsed["checks"].(map[string]any)
	if !ok {
		t.Fatalf("readiness must report its checks: %s", body)
	}
	for _, name := range []string{"database", "schema", "blob_store"} {
		if checks[name] != "ok" {
			t.Fatalf("check %s = %v, want ok", name, checks[name])
		}
	}
	schema, ok := parsed["schema"].(map[string]any)
	if !ok || schema["applied_version"] != schema["expected_version"] {
		t.Fatalf("readiness must compare the applied schema against this binary's: %s", body)
	}
}

func TestIntegrationAuthenticationIsRequired(t *testing.T) {
	h := newAPIHarness(t)
	gs := string(h.fixture.Primary.GraphSpace.ID)

	status, body := h.do(t, http.MethodPost, "/v1/graph-spaces/"+gs+"/events", "", `{}`, nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("expected 401 without credentials, got %d: %s", status, body)
	}
	if code := errorCode(t, body); code != string(domain.CodeUnauthenticated) {
		t.Fatalf("expected unauthenticated, got %s", code)
	}

	// A wrong secret must be indistinguishable from an unknown key.
	req, err := http.NewRequest(http.MethodPost, h.server.URL+"/v1/graph-spaces/"+gs+"/events", bytes.NewBufferString("{}"))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer key-acme-owner.wrong-secret")
	resp, err := h.server.Client().Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for a bad secret, got %d", resp.StatusCode)
	}
	if resp.Header.Get("WWW-Authenticate") == "" {
		t.Fatal("a 401 should tell the client how to authenticate")
	}
}

func TestIntegrationIngestAndStatusFlow(t *testing.T) {
	h := newAPIHarness(t)
	ctx := context.Background()
	gs := string(h.fixture.Primary.GraphSpace.ID)
	owner := string(h.fixture.Primary.Principal.ID)

	// Ingest one event.
	status, body := h.do(t, http.MethodPost, "/v1/graph-spaces/"+gs+"/events", owner, map[string]any{
		"source_name": h.fixture.Primary.Source.Name,
		"event_type":  "chat.session",
		"media_type":  normalize.MediaTypeJSON,
		"content_json": json.RawMessage(`{"messages":[
			{"role":"user","content":"When does the Acme contract renew?"},
			{"role":"assistant","content":"On April 2nd, thirty days after the March 3rd invoice."}]}`),
	}, map[string]string{"Idempotency-Key": "api-1"})
	if status != http.StatusAccepted {
		t.Fatalf("expected 202 for a new event, got %d: %s", status, body)
	}
	receipt := decode(t, body)
	eventID, _ := receipt["source_event_id"].(string)
	if eventID == "" {
		t.Fatalf("no event id in receipt: %s", body)
	}
	if receipt["duplicate"] != false {
		t.Fatal("a new event must not be reported as a duplicate")
	}

	// Replay returns the original event with 200 rather than creating a second one.
	status, body = h.do(t, http.MethodPost, "/v1/graph-spaces/"+gs+"/events", owner, map[string]any{
		"source_name": h.fixture.Primary.Source.Name,
		"event_type":  "chat.session",
		"media_type":  normalize.MediaTypeJSON,
		"content_json": json.RawMessage(`{"messages":[
			{"role":"user","content":"When does the Acme contract renew?"},
			{"role":"assistant","content":"On April 2nd, thirty days after the March 3rd invoice."}]}`),
	}, map[string]string{"Idempotency-Key": "api-1"})
	if status != http.StatusOK {
		t.Fatalf("expected 200 for a replay, got %d: %s", status, body)
	}
	replay := decode(t, body)
	if replay["duplicate"] != true || replay["source_event_id"] != eventID {
		t.Fatalf("a replay must resolve to the original event: %s", body)
	}

	// The same key with different content is a conflict.
	status, body = h.do(t, http.MethodPost, "/v1/graph-spaces/"+gs+"/events", owner, map[string]any{
		"source_name": h.fixture.Primary.Source.Name,
		"content":     "completely different content",
	}, map[string]string{"Idempotency-Key": "api-1"})
	if status != http.StatusConflict {
		t.Fatalf("expected 409 for a reused key with new content, got %d: %s", status, body)
	}
	if code := errorCode(t, body); code != string(domain.CodeSourceEventConflict) {
		t.Fatalf("expected source_event_conflict, got %s", code)
	}

	// Status before processing.
	status, body = h.do(t, http.MethodGet, "/v1/events/"+eventID+"/status", owner, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("status returned %d: %s", status, body)
	}
	before := decode(t, body)
	if before["status"] != string(domain.SourceEventAccepted) {
		t.Fatalf("expected an accepted event, got %v", before["status"])
	}
	if before["episodes"].(float64) != 0 {
		t.Fatal("nothing should be derived before processing")
	}
	if work, ok := before["work"].([]any); !ok || len(work) != 1 {
		t.Fatalf("acknowledged ingestion must have queued work: %s", body)
	}

	// Process, then check status reflects the derived units.
	if _, err := h.runner.Process(ctx, h.fixture.Primary.Workspace.ID, domain.SourceEventID(eventID), false); err != nil {
		t.Fatalf("process: %v", err)
	}
	status, body = h.do(t, http.MethodGet, "/v1/events/"+eventID+"/status", owner, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("status returned %d: %s", status, body)
	}
	after := decode(t, body)
	if after["status"] != string(domain.SourceEventProcessed) {
		t.Fatalf("expected a processed event, got %v", after["status"])
	}
	if after["episodes"].(float64) != 2 {
		t.Fatalf("expected one episode per conversation turn, got %v", after["episodes"])
	}
	pipelineStatus, ok := after["pipeline"].(map[string]any)
	if !ok {
		t.Fatalf("status must report the pipeline: %s", body)
	}
	stages, ok := pipelineStatus["stages"].([]any)
	if !ok || len(stages) != 3 {
		t.Fatalf("expected 3 stages in the status response: %s", body)
	}
}

func TestIntegrationBatchIngestReportsPerItemOutcomes(t *testing.T) {
	h := newAPIHarness(t)
	gs := string(h.fixture.Primary.GraphSpace.ID)
	owner := string(h.fixture.Primary.Principal.ID)

	status, body := h.do(t, http.MethodPost, "/v1/graph-spaces/"+gs+"/events", owner, []map[string]any{
		{"source_name": h.fixture.Primary.Source.Name, "content": "first record"},
		{"source_name": "no-such-source", "content": "second record"},
		{"source_name": h.fixture.Primary.Source.Name, "content": "third record"},
	}, nil)

	// 207: some items were durably accepted and one was not. Reporting a single overall
	// status would hide one or the other.
	if status != http.StatusMultiStatus {
		t.Fatalf("expected 207 for a partially successful batch, got %d: %s", status, body)
	}
	parsed := decode(t, body)
	if parsed["accepted"].(float64) != 2 || parsed["failed"].(float64) != 1 {
		t.Fatalf("unexpected batch tally: %s", body)
	}
	results, ok := parsed["results"].([]any)
	if !ok || len(results) != 3 {
		t.Fatalf("expected a result per item: %s", body)
	}
	failed := results[1].(map[string]any)
	if failed["error"] == nil {
		t.Fatalf("the failing item must report its error: %s", body)
	}
	for _, index := range []int{0, 2} {
		item := results[index].(map[string]any)
		if item["receipt"] == nil {
			t.Fatalf("item %d must carry a receipt: %s", index, body)
		}
	}
}

func TestIntegrationDocumentAndEpisodeEndpoints(t *testing.T) {
	h := newAPIHarness(t)
	gs := string(h.fixture.Primary.GraphSpace.ID)
	owner := string(h.fixture.Primary.Principal.ID)
	source := h.fixture.Primary.Source.Name

	// A raw document body, no JSON wrapper.
	status, body := h.do(t, http.MethodPost,
		"/v1/graph-spaces/"+gs+"/documents?source_name="+source+"&external_id=handbook.md",
		owner, []byte("# Handbook\n\nRefunds run thirty days.\n"),
		map[string]string{"Content-Type": normalize.MediaTypeMarkdown, "Idempotency-Key": "doc-1"})
	if status != http.StatusAccepted {
		t.Fatalf("document ingest returned %d: %s", status, body)
	}

	// A caller-segmented episode.
	status, body = h.do(t, http.MethodPost, "/v1/graph-spaces/"+gs+"/episodes", owner, map[string]any{
		"source_name": source,
		"content":     "Alice Chen confirmed the renewal.",
		"event_type":  "chat.turn",
	}, map[string]string{"Idempotency-Key": "ep-1"})
	if status != http.StatusAccepted {
		t.Fatalf("episode ingest returned %d: %s", status, body)
	}

	// Both are ordinary source events, so both are processable through the same path.
	receipt := decode(t, body)
	eventID := domain.SourceEventID(receipt["source_event_id"].(string))
	if _, err := h.runner.Process(context.Background(), h.fixture.Primary.Workspace.ID, eventID, false); err != nil {
		t.Fatalf("process: %v", err)
	}
	episodes, err := h.fixture.Store.ListEpisodes(context.Background(), h.fixture.Primary.Workspace.ID, eventID)
	if err != nil {
		t.Fatalf("list episodes: %v", err)
	}
	if len(episodes) != 1 {
		t.Fatalf("a caller-supplied episode must stay one episode, got %d", len(episodes))
	}
}

func TestIntegrationCrossWorkspaceAccessIsDenied(t *testing.T) {
	h := newAPIHarness(t,
		keyEntry{principalID: "acme-owner", secret: "a", systemRole: domain.RoleAdmin},
		keyEntry{principalID: "globex-owner", secret: "b", systemRole: domain.RoleAdmin},
	)
	owner := string(h.fixture.Primary.Principal.ID)

	// Ingest into tenant A.
	gsA := string(h.fixture.Primary.GraphSpace.ID)
	status, body := h.do(t, http.MethodPost, "/v1/graph-spaces/"+gsA+"/events", owner, map[string]any{
		"source_name": h.fixture.Primary.Source.Name,
		"content":     "tenant A knowledge",
	}, map[string]string{"Idempotency-Key": "a-1"})
	if status != http.StatusAccepted {
		t.Fatalf("ingest returned %d: %s", status, body)
	}
	eventID := decode(t, body)["source_event_id"].(string)

	// Tenant B holds a valid credential but no grant on tenant A. Every route must
	// report absence rather than confirming the resource exists (AGENTS.md scenario F).
	status, body = h.do(t, http.MethodPost, "/v1/graph-spaces/"+gsA+"/events", "globex-owner", map[string]any{
		"source_name": h.fixture.Primary.Source.Name,
		"content":     "should never land",
	}, nil)
	if status != http.StatusNotFound {
		t.Fatalf("expected 404 for another tenant's graph space, got %d: %s", status, body)
	}
	if code := errorCode(t, body); code != string(domain.CodeGraphSpaceNotFound) {
		t.Fatalf("expected graph_space_not_found, got %s", code)
	}

	status, body = h.do(t, http.MethodGet, "/v1/events/"+eventID+"/status", "globex-owner", nil, nil)
	if status != http.StatusNotFound {
		t.Fatalf("expected 404 for another tenant's event, got %d: %s", status, body)
	}

	status, body = h.do(t, http.MethodGet,
		"/v1/workspaces/"+string(h.fixture.Primary.Workspace.ID)+"/sources", "globex-owner", nil, nil)
	if status != http.StatusNotFound {
		t.Fatalf("expected 404 for another tenant's workspace, got %d: %s", status, body)
	}

	// Listing is scoped by grants, so tenant B sees nothing of tenant A.
	status, body = h.do(t, http.MethodGet, "/v1/workspaces", "globex-owner", nil, nil)
	if status != http.StatusOK {
		t.Fatalf("list workspaces returned %d: %s", status, body)
	}
	if workspaces, ok := decode(t, body)["workspaces"].([]any); !ok || len(workspaces) != 0 {
		t.Fatalf("a principal without grants must see no workspaces: %s", body)
	}
}

func TestIntegrationWorkspaceCreationRequiresASystemRole(t *testing.T) {
	h := newAPIHarness(t,
		keyEntry{principalID: "acme-owner", secret: "a", systemRole: domain.RoleAdmin},
		keyEntry{principalID: "watcher", secret: "w", systemRole: domain.RoleReader},
	)

	status, body := h.do(t, http.MethodPost, "/v1/workspaces", "watcher",
		map[string]any{"slug": "sneaky", "name": "Sneaky"}, nil)
	if status != http.StatusForbidden {
		t.Fatalf("expected 403 for a reader, got %d: %s", status, body)
	}

	status, body = h.do(t, http.MethodPost, "/v1/workspaces", "acme-owner",
		map[string]any{"slug": "newtenant", "name": "New Tenant"}, nil)
	if status != http.StatusCreated {
		t.Fatalf("expected 201 for an admin, got %d: %s", status, body)
	}
	created := decode(t, body)

	// The creator is granted ownership, so the new workspace is immediately usable.
	status, body = h.do(t, http.MethodPost,
		"/v1/workspaces/"+created["id"].(string)+"/graph-spaces", "acme-owner",
		map[string]any{"slug": "main", "name": "Main"}, nil)
	if status != http.StatusCreated {
		t.Fatalf("the creator must be able to administer the workspace, got %d: %s", status, body)
	}
}

func TestIntegrationRequestValidation(t *testing.T) {
	h := newAPIHarness(t)
	gs := string(h.fixture.Primary.GraphSpace.ID)
	owner := string(h.fixture.Primary.Principal.ID)

	cases := []struct {
		name     string
		path     string
		body     any
		wantCode int
	}{
		{"unknown field", "/v1/graph-spaces/" + gs + "/episodes",
			`{"source_name":"test-source","content":"x","typo":1}`, http.StatusBadRequest},
		{"empty body", "/v1/graph-spaces/" + gs + "/episodes", ``, http.StatusBadRequest},
		{"two documents", "/v1/graph-spaces/" + gs + "/episodes",
			`{"source_name":"test-source","content":"x"}{"source_name":"test-source","content":"y"}`, http.StatusBadRequest},
		{"no content", "/v1/graph-spaces/" + gs + "/events",
			`{"source_name":"test-source"}`, http.StatusBadRequest},
		{"both content forms", "/v1/graph-spaces/" + gs + "/events",
			`{"source_name":"test-source","content":"x","content_json":{"a":1}}`, http.StatusBadRequest},
		{"bad operation", "/v1/graph-spaces/" + gs + "/events",
			`{"source_name":"test-source","content":"x","operation":"obliterate"}`, http.StatusBadRequest},
		{"bad classification", "/v1/graph-spaces/" + gs + "/events",
			`{"source_name":"test-source","content":"x","classification":"cosmic"}`, http.StatusBadRequest},
		{"empty batch", "/v1/graph-spaces/" + gs + "/events", `[]`, http.StatusBadRequest},
		{"malformed graph space id", "/v1/graph-spaces/not-a-uuid/events",
			`{"source_name":"test-source","content":"x"}`, http.StatusBadRequest},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, body := h.do(t, http.MethodPost, tc.path, owner, tc.body, nil)
			if status != tc.wantCode {
				t.Fatalf("expected %d, got %d: %s", tc.wantCode, status, body)
			}
			if errorCode(t, body) == "" {
				t.Fatalf("error responses must carry a machine-readable code: %s", body)
			}
		})
	}

	// A malformed event id must be rejected before it reaches a query.
	status, body := h.do(t, http.MethodGet, "/v1/events/not-a-uuid/status", owner, nil, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("expected 400 for a malformed event id, got %d: %s", status, body)
	}
}

func TestIntegrationErrorsCarryRequestIDs(t *testing.T) {
	h := newAPIHarness(t)
	owner := string(h.fixture.Primary.Principal.ID)

	status, body := h.do(t, http.MethodGet, "/v1/events/not-a-uuid/status", owner, nil, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("unexpected status %d", status)
	}
	envelope := decode(t, body)["error"].(map[string]any)
	if envelope["request_id"] == "" || envelope["request_id"] == nil {
		t.Fatalf("errors must be correlatable to a request: %s", body)
	}
}

func TestIntegrationSourceRegistrationThroughAPI(t *testing.T) {
	h := newAPIHarness(t)
	owner := string(h.fixture.Primary.Principal.ID)
	ws := string(h.fixture.Primary.Workspace.ID)

	status, body := h.do(t, http.MethodPost, "/v1/workspaces/"+ws+"/sources", owner, map[string]any{
		"kind": "database", "name": "crm", "uri": "postgres://crm/customers",
		"trust_level": "high", "classification": "confidential",
	}, nil)
	if status != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", status, body)
	}
	created := decode(t, body)
	if created["trust_level"] != "high" || created["classification"] != "confidential" {
		t.Fatalf("source attributes were not persisted: %s", body)
	}

	// A duplicate name in the same workspace conflicts.
	status, body = h.do(t, http.MethodPost, "/v1/workspaces/"+ws+"/sources", owner, map[string]any{
		"kind": "database", "name": "crm",
	}, nil)
	if status != http.StatusConflict {
		t.Fatalf("expected 409 for a duplicate source name, got %d: %s", status, body)
	}

	status, body = h.do(t, http.MethodGet, "/v1/workspaces/"+ws+"/sources", owner, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("list sources returned %d: %s", status, body)
	}
	if sources, ok := decode(t, body)["sources"].([]any); !ok || len(sources) != 2 {
		t.Fatalf("expected the fixture source plus the new one: %s", body)
	}
}

// discardLogger keeps test output readable; the code under test still exercises every
// logging path.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}
