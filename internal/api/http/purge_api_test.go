package http_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

func TestKnowledgePurgeDefaultsToDryRun(t *testing.T) {
	h := newAPIHarness(t)
	before := time.Now().UTC().Add(-90 * 24 * time.Hour)
	status, body := h.do(t, http.MethodPost,
		"/v1/graph-spaces/"+string(h.fixture.Primary.GraphSpace.ID)+"/knowledge-purge",
		"acme-owner", map[string]any{"before": before, "reason": "retention policy"}, nil)
	if status != http.StatusOK {
		t.Fatalf("purge plan status=%d body=%s", status, body)
	}
	var response struct {
		DryRun bool   `json:"dry_run"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatalf("decode purge plan: %v", err)
	}
	if !response.DryRun || response.Status != "completed" {
		t.Fatalf("purge endpoint did not default to a completed dry run: %+v", response)
	}
}
