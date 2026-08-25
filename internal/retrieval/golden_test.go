package retrieval_test

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/gimantha/strata/internal/domain"
)

// update rewrites the golden file instead of comparing against it:
//
//	go test ./internal/retrieval -update    # then review the diff
//
// Same convention as internal/normalize/golden_test.go.
var update = flag.Bool("update", false, "rewrite the retrieval golden file")

// Record ids are UUIDs minted per run, so a golden keyed on them would fail every time.
// Labels resolve an id back to something stable: the corpus key a chunk came from, or the
// canonical name of an entity. Scores are rounded, because the last digits of a float are
// not part of the contract and a golden that pinned them would break on an unrelated
// PostgreSQL upgrade.
// goldenQuery records one query's results in an order-insensitive form.
//
// Labels and scores are captured as two sorted lists rather than as pairs, and that is a
// deliberate weakening with a specific reason. Two records that tie inside a retriever are
// ranked in whatever order that retriever returned them; fusion turns those ranks into
// adjacent-but-different scores, so which of the two ends up with the higher score is
// arbitrary. It is now at least stable for a given corpus — the retrievers break ties on
// record id — but record ids are fresh UUIDs in every fixture, so the arbitrary choice
// differs between one run of this test and the next. CI and a laptop disagreed about
// exactly this, which is how the tie-breaking defect was found.
//
// So the pairing between a label and its score is not pinned. Everything else is: a record
// entering or leaving the results, a score changing value, the number of results, and which
// retrievers found each one. What this cannot catch is exactly two records exchanging
// scores, which is the noise it exists to tolerate.
type goldenQuery struct {
	Query string   `json:"query"`
	Modes []string `json:"modes,omitempty"`
	// Labels are the records returned, sorted. Order-insensitive: see above.
	Labels []string `json:"labels"`
	// Scores are the fused scores, sorted descending, paired with nothing.
	Scores []float64 `json:"scores"`
	// FoundBy maps each record to the retrievers that surfaced it. Stable, because it does
	// not depend on rank.
	FoundBy map[string][]string `json:"found_by"`
}

// TestIntegrationRetrievalOutputIsGolden pins what retrieval returns, so a refactor can be
// shown to change nothing rather than merely to compile and pass.
//
// The existing suite asserts properties — hybrid beats every single mode on recall, results
// are stable across runs, filters exclude what they should. Those are the right assertions
// for retrieval quality, and they would all stay green through a change that quietly
// reordered results or dropped a contributing retriever. This file exists for the other
// question: is the output identical?
//
// Written before splitting projection.Store and retrieval.Store into per-index ports, which
// is a refactor whose entire claim is that it changes no behaviour. A claim like that wants
// evidence stronger than a green suite.
func TestIntegrationRetrievalOutputIsGolden(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	keyByRecord := h.loadFixture(t)

	// Every mode combination the evaluation exercises, not only the planner's default: a
	// per-index port split could plausibly change one leg and leave the fusion looking
	// unchanged.
	modeSets := []struct {
		name  string
		modes []domain.RetrievalMode
	}{
		{"hybrid", nil},
		{"lexical", []domain.RetrievalMode{domain.ModeLexical}},
		{"exact", []domain.RetrievalMode{domain.ModeExact}},
		{"vector", []domain.RetrievalMode{domain.ModeVector}},
		{"entity", []domain.RetrievalMode{domain.ModeEntity}},
		{"entity+graph", []domain.RetrievalMode{domain.ModeEntity, domain.ModeGraph}},
	}

	captured := make([]goldenQuery, 0, len(evaluationQueries)*len(modeSets))
	for _, set := range modeSets {
		for _, query := range evaluationQueries {
			result, err := h.retriever.Query(ctx, domain.QueryRequest{
				Scope: h.scope(),
				Query: query.text,
				Modes: set.modes,
				Limit: 10,
			})
			if err != nil {
				t.Fatalf("%s / %s: %v", set.name, query.name, err)
			}

			var (
				labels  []string
				scores  []float64
				foundBy = map[string][]string{}
			)
			for _, item := range result.Items {
				name := string(item.Surface) + ":" + label(item, keyByRecord)
				labels = append(labels, name)
				scores = append(scores, round(item.Score))

				found := make([]string, 0, len(item.FoundBy))
				for _, mode := range item.FoundBy {
					found = append(found, string(mode))
				}
				sort.Strings(found)
				foundBy[name] = found
			}
			sort.Strings(labels)
			sort.Sort(sort.Reverse(sort.Float64Slice(scores)))

			modes := make([]string, 0, len(set.modes))
			for _, mode := range set.modes {
				modes = append(modes, string(mode))
			}
			captured = append(captured, goldenQuery{
				Query:   set.name + " / " + query.name,
				Modes:   modes,
				Labels:  labels,
				Scores:  scores,
				FoundBy: foundBy,
			})
		}
	}

	path := filepath.Join("testdata", "retrieval_golden.json")
	encoded, err := json.MarshalIndent(captured, "", "  ")
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	encoded = append(encoded, '\n')

	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("create testdata: %v", err)
		}
		if err := os.WriteFile(path, encoded, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Logf("rewrote %s with %d query results", path, len(captured))
		return
	}

	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden (run with -update to create it): %v", err)
	}
	if string(encoded) == string(want) {
		return
	}

	// A whole-file diff of a few hundred lines is unreadable, so report the first
	// differing query rather than the first differing byte.
	var previous []goldenQuery
	if err := json.Unmarshal(want, &previous); err != nil {
		t.Fatalf("the golden file is not readable: %v", err)
	}
	t.Errorf("retrieval output changed. Review the diff, and rerun with -update only if the "+
		"change is intended:\n%s", firstDifference(previous, captured))
}

// label renders an item as something that survives a fresh fixture.
func label(item domain.RetrievedItem, keyByRecord map[string]string) string {
	if key, ok := keyByRecord[item.RecordID]; ok {
		return key
	}
	// Entities and assertions have no corpus key. Their projected content is derived
	// deterministically from canonical data, so it is stable across runs; the prefix keeps
	// a long rendered claim from dominating the file.
	content := strings.TrimSpace(item.Content)
	if len(content) > 60 {
		content = content[:60]
	}
	if content == "" {
		return "(" + string(item.Surface) + ", no content)"
	}
	return content
}

// round trims a score to four places. The digits beyond that are arithmetic noise, and
// pinning them would turn a PostgreSQL point release into a failing test.
func round(score float64) float64 {
	const places = 10000
	return float64(int64(score*places+0.5)) / places
}

// firstDifference reports the first query whose results changed.
func firstDifference(want, got []goldenQuery) string {
	var out strings.Builder
	for i := range max(len(want), len(got)) {
		switch {
		case i >= len(want):
			fmt.Fprintf(&out, "new query %q appeared\n", got[i].Query)
			return out.String()
		case i >= len(got):
			fmt.Fprintf(&out, "query %q disappeared\n", want[i].Query)
			return out.String()
		}

		a, b := want[i], got[i]
		if render(a) == render(b) {
			continue
		}
		fmt.Fprintf(&out, "query %q\n  before: %s\n  after:  %s\n", a.Query, render(a), render(b))
		return out.String()
	}
	return "the files differ but no query does; the difference is formatting"
}

func render(q goldenQuery) string {
	if len(q.Labels) == 0 {
		return "(no results)"
	}
	scores := make([]string, 0, len(q.Scores))
	for _, score := range q.Scores {
		scores = append(scores, fmt.Sprintf("%.4f", score))
	}
	return fmt.Sprintf("%s | scores %s",
		strings.Join(q.Labels, ", "), strings.Join(scores, ", "))
}
