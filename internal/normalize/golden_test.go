package normalize_test

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/gimantha/strata/internal/normalize"
)

// update rewrites the golden files instead of comparing against them:
//
//	go test ./internal/normalize -update
//
// Segmentation and chunk boundaries decide what every later phase retrieves, so a
// change to them should be visible in a diff rather than absorbed silently
// (AGENTS.md section 32.4).
var update = flag.Bool("update", false, "rewrite golden files")

type goldenDocument struct {
	MediaType string          `json:"media_type"`
	TextRunes int             `json:"text_runes"`
	Segments  []goldenSegment `json:"segments"`
}

type goldenSegment struct {
	Sequence    int64         `json:"sequence"`
	ContentType string        `json:"content_type"`
	EventTime   string        `json:"event_time,omitempty"`
	Role        string        `json:"role,omitempty"`
	Section     string        `json:"section,omitempty"`
	HeadingPath []string      `json:"heading_path,omitempty"`
	JSONPointer string        `json:"json_pointer,omitempty"`
	RowKey      string        `json:"row_key,omitempty"`
	CharStart   int           `json:"artifact_char_start"`
	CharEnd     int           `json:"artifact_char_end"`
	Content     string        `json:"content"`
	Chunks      []goldenChunk `json:"chunks"`
}

type goldenChunk struct {
	Sequence   int64  `json:"sequence"`
	TokenCount int    `json:"token_count"`
	CharStart  int    `json:"char_start"`
	CharEnd    int    `json:"char_end"`
	Content    string `json:"content"`
}

func TestGoldenFixtures(t *testing.T) {
	cases := []struct {
		name      string
		path      string
		mediaType string
	}{
		{"chat-session", "chat/session-01.json", normalize.MediaTypeJSON},
		{"markdown-handbook", "doc/handbook.md", normalize.MediaTypeMarkdown},
		{"json-records", "json/customers.json", normalize.MediaTypeJSON},
	}

	opts := normalize.ChunkOptions{MaxTokens: 48, OverlapTokens: 8}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", tc.path))
			if err != nil {
				t.Fatalf("read fixture: %v", err)
			}

			doc, err := normalize.Decode(tc.mediaType, raw, normalize.Options{})
			if err != nil {
				t.Fatalf("decode: %v", err)
			}

			got := goldenDocument{MediaType: doc.MediaType, TextRunes: len([]rune(doc.Text))}
			for _, seg := range doc.Segments {
				entry := goldenSegment{
					Sequence:    seg.Sequence,
					ContentType: seg.ContentType,
					Role:        seg.Locator.Role,
					Section:     seg.Locator.Section,
					HeadingPath: seg.Locator.HeadingPath,
					JSONPointer: seg.Locator.JSONPointer,
					RowKey:      seg.Locator.RowKey,
					CharStart:   seg.Locator.ArtifactCharStart,
					CharEnd:     seg.Locator.ArtifactCharEnd,
					Content:     seg.Content,
				}
				if seg.EventTime != nil {
					entry.EventTime = seg.EventTime.UTC().Format("2006-01-02T15:04:05Z")
				}
				for _, spec := range normalize.Chunk(seg.Content, opts) {
					entry.Chunks = append(entry.Chunks, goldenChunk{
						Sequence:   spec.Sequence,
						TokenCount: spec.TokenCount,
						CharStart:  spec.CharStart,
						CharEnd:    spec.CharEnd,
						Content:    spec.Content,
					})
				}
				got.Segments = append(got.Segments, entry)
			}

			encoded, err := json.MarshalIndent(got, "", "  ")
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			encoded = append(encoded, '\n')

			goldenPath := filepath.Join("..", "..", "testdata", "golden", tc.name+".json")
			if *update {
				if err := os.MkdirAll(filepath.Dir(goldenPath), 0o755); err != nil {
					t.Fatalf("create golden directory: %v", err)
				}
				if err := os.WriteFile(goldenPath, encoded, 0o644); err != nil {
					t.Fatalf("write golden file: %v", err)
				}
				t.Logf("updated %s", goldenPath)
				return
			}

			want, err := os.ReadFile(goldenPath)
			if err != nil {
				t.Fatalf("read golden file (run with -update to create it): %v", err)
			}
			if string(encoded) != string(want) {
				t.Fatalf("segmentation or chunking changed for %s.\n"+
					"If the change is intended, re-run with -update and review the diff.\n"+
					"got:\n%s", tc.name, encoded)
			}
		})
	}
}
