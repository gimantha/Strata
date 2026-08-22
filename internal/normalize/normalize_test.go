package normalize

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestDecodeChatProducesOneEpisodePerTurn(t *testing.T) {
	payload := []byte(`{"messages":[
		{"role":"user","content":"What is our refund window?","timestamp":"2026-03-25T10:00:00Z"},
		{"role":"assistant","content":"Thirty days from delivery."},
		{"role":"user","content":"Thanks."}
	]}`)

	doc, err := Decode(MediaTypeJSON, payload, Options{})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(doc.Segments) != 3 {
		t.Fatalf("expected one segment per turn, got %d", len(doc.Segments))
	}

	first := doc.Segments[0]
	if first.Locator.Role != "user" {
		t.Fatalf("role provenance lost: %+v", first.Locator)
	}
	if first.Locator.MessageIndex == nil || *first.Locator.MessageIndex != 0 {
		t.Fatalf("message index provenance lost: %+v", first.Locator)
	}
	if first.Locator.JSONPointer != "/messages/0" {
		t.Fatalf("json pointer provenance lost: %q", first.Locator.JSONPointer)
	}
	if first.EventTime == nil {
		t.Fatal("a timestamp supplied by the source must be preserved, not discarded")
	}
	if !strings.Contains(first.Content, "refund window") {
		t.Fatalf("content lost: %q", first.Content)
	}
	// The turn stays attributable on its own after retrieval.
	if !strings.HasPrefix(first.Content, "user: ") {
		t.Fatalf("speaker should be attached to the turn, got %q", first.Content)
	}

	// A turn without its own timestamp must not inherit one.
	if doc.Segments[1].EventTime != nil {
		t.Fatal("event time must not be invented for a turn that has none")
	}

	assertSegmentOffsets(t, doc)
}

func TestDecodeJSONRecordsProducesOneEpisodePerRecord(t *testing.T) {
	payload := []byte(`[
		{"id":"c-1","name":"Acme","updated_at":"2026-03-01T00:00:00Z"},
		{"id":"c-2","name":"Globex"}
	]`)

	doc, err := Decode(MediaTypeJSON, payload, Options{})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(doc.Segments) != 2 {
		t.Fatalf("expected 2 segments, got %d", len(doc.Segments))
	}
	if doc.Segments[0].Locator.RowKey != "c-1" {
		t.Fatalf("primary key provenance lost: %+v", doc.Segments[0].Locator)
	}
	if doc.Segments[0].Locator.JSONPointer != "/0" || doc.Segments[1].Locator.JSONPointer != "/1" {
		t.Fatal("json pointer provenance lost")
	}
	if doc.Segments[0].EventTime == nil {
		t.Fatal("updated_at supplied by the source must become event time")
	}
	assertSegmentOffsets(t, doc)
}

func TestDecodeJSONIsCanonicalAndStable(t *testing.T) {
	// The same data with different key order must produce identical text, otherwise
	// chunk boundaries move between ingests of equivalent records.
	a, err := Decode(MediaTypeJSON, []byte(`{"b":2,"a":1}`), Options{})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	b, err := Decode(MediaTypeJSON, []byte(`{"a":1,"b":2}`), Options{})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if a.Text != b.Text {
		t.Fatalf("canonical JSON must be key-order independent:\n%q\n%q", a.Text, b.Text)
	}
	if !json.Valid([]byte(a.Text)) {
		t.Fatalf("canonical rendering must still be valid JSON: %q", a.Text)
	}
}

func TestDecodeMarkdownSplitsOnHeadings(t *testing.T) {
	doc, err := Decode(MediaTypeMarkdown, []byte(strings.Join([]string{
		"# Handbook",
		"Intro text.",
		"",
		"## Refunds",
		"Thirty days.",
		"",
		"### Exceptions",
		"Final sale items are excluded.",
	}, "\n")), Options{})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(doc.Segments) != 3 {
		t.Fatalf("expected one segment per section, got %d", len(doc.Segments))
	}

	last := doc.Segments[2]
	if last.Locator.Section != "Exceptions" {
		t.Fatalf("section title lost: %+v", last.Locator)
	}
	want := []string{"Handbook", "Refunds", "Exceptions"}
	if len(last.Locator.HeadingPath) != 3 {
		t.Fatalf("heading path should give a retrieved excerpt its ancestry, got %v", last.Locator.HeadingPath)
	}
	for i, w := range want {
		if last.Locator.HeadingPath[i] != w {
			t.Fatalf("heading path = %v, want %v", last.Locator.HeadingPath, want)
		}
	}
	assertSegmentOffsets(t, doc)
}

func TestDecodeMarkdownWithoutHeadingsIsOneEpisode(t *testing.T) {
	doc, err := Decode(MediaTypeMarkdown, []byte("Just a paragraph.\n\nAnd another."), Options{})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(doc.Segments) != 1 {
		t.Fatalf("expected 1 segment, got %d", len(doc.Segments))
	}
}

func TestDecodePlainTextAndUnknownTypes(t *testing.T) {
	for _, mediaType := range []string{MediaTypePlain, "", "application/x-unknown", "text/plain; charset=utf-8"} {
		doc, err := Decode(mediaType, []byte("hello world"), Options{})
		if err != nil {
			t.Fatalf("media type %q must degrade to plain text, got %v", mediaType, err)
		}
		if len(doc.Segments) != 1 || doc.Segments[0].Content != "hello world" {
			t.Fatalf("unexpected segmentation for %q: %+v", mediaType, doc.Segments)
		}
	}
}

func TestDecodeDirectEpisodeForcesSingleSegment(t *testing.T) {
	// Direct submission shares the source-event path; it must not be re-segmented.
	payload := []byte(`{"messages":[{"role":"user","content":"one"},{"role":"user","content":"two"}]}`)
	doc, err := Decode(MediaTypeJSON, payload, Options{DirectEpisode: true})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(doc.Segments) != 1 {
		t.Fatalf("a caller-supplied episode must stay one episode, got %d", len(doc.Segments))
	}
}

func TestDecodeRejectsMalformedAndNonUTF8(t *testing.T) {
	if _, err := Decode(MediaTypeJSON, []byte(`{"messages":`), Options{}); err == nil {
		t.Fatal("malformed JSON must be rejected rather than stored as text")
	}
	if _, err := Decode(MediaTypeJSON, []byte(``), Options{}); err == nil {
		t.Fatal("an empty payload must be rejected")
	}
	if _, err := Decode(MediaTypePlain, []byte{0xff, 0xfe, 0x00}, Options{}); err == nil {
		t.Fatal("invalid UTF-8 must be rejected by segmentation")
	}
}

func TestNormalizeNewlinesMakesOffsetsPlatformIndependent(t *testing.T) {
	crlf, err := Decode(MediaTypePlain, []byte("line one\r\nline two"), Options{})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	lf, err := Decode(MediaTypePlain, []byte("line one\nline two"), Options{})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if crlf.Text != lf.Text {
		t.Fatalf("CRLF and LF sources must normalize identically: %q vs %q", crlf.Text, lf.Text)
	}
}

// assertSegmentOffsets checks the provenance contract: every segment's locator offsets
// must index back into the document text and reproduce the segment exactly.
func assertSegmentOffsets(t *testing.T, doc Document) {
	t.Helper()

	runes := []rune(doc.Text)
	for _, seg := range doc.Segments {
		start, end := seg.Locator.ArtifactCharStart, seg.Locator.ArtifactCharEnd
		if start < 0 || end > len(runes) || end < start {
			t.Fatalf("segment %d has offsets outside the document: [%d,%d) of %d",
				seg.Sequence, start, end, len(runes))
		}
		if got := string(runes[start:end]); got != seg.Content {
			t.Fatalf("segment %d offsets do not reproduce its content:\nfrom text: %q\nsegment:   %q",
				seg.Sequence, got, seg.Content)
		}
	}
}

func TestChunkOffsetsReproduceSourceExactly(t *testing.T) {
	text := strings.Repeat("Alice signed the contract on March 3rd. ", 40)

	specs := Chunk(text, ChunkOptions{MaxTokens: 32, OverlapTokens: 4})
	if len(specs) < 2 {
		t.Fatalf("expected the text to split into several chunks, got %d", len(specs))
	}

	runes := []rune(text)
	for _, spec := range specs {
		if got := text[spec.ByteStart:spec.ByteEnd]; got != spec.Content {
			t.Fatalf("chunk %d byte offsets do not reproduce its content", spec.Sequence)
		}
		if got := string(runes[spec.CharStart:spec.CharEnd]); got != spec.Content {
			t.Fatalf("chunk %d char offsets do not reproduce its content", spec.Sequence)
		}
		if spec.TokenCount <= 0 {
			t.Fatalf("chunk %d has no token estimate", spec.Sequence)
		}
	}
}

func TestChunkCoversAllContentAndOverlaps(t *testing.T) {
	text := strings.Repeat("sentence about a fact. ", 60)
	specs := Chunk(text, ChunkOptions{MaxTokens: 24, OverlapTokens: 6})

	if specs[0].ByteStart != 0 {
		t.Fatal("chunking must start at the beginning of the text")
	}
	if specs[len(specs)-1].ByteEnd != len(text) {
		t.Fatal("chunking must reach the end of the text: dropped tail content is lost knowledge")
	}
	for i := 1; i < len(specs); i++ {
		if specs[i].ByteStart >= specs[i].ByteEnd {
			t.Fatalf("chunk %d is empty", i)
		}
		if specs[i].ByteStart <= specs[i-1].ByteStart {
			t.Fatalf("chunk starts must advance: %d then %d", specs[i-1].ByteStart, specs[i].ByteStart)
		}
		if specs[i].ByteStart >= specs[i-1].ByteEnd {
			t.Fatalf("chunk %d does not overlap its predecessor, so a fact spanning the "+
				"boundary would be unretrievable", i)
		}
	}
}

func TestChunkPrefersNaturalBoundaries(t *testing.T) {
	text := "First paragraph about refunds.\n\nSecond paragraph about shipping and delivery windows " +
		strings.Repeat("with extra words ", 20)

	specs := Chunk(text, ChunkOptions{MaxTokens: 12, OverlapTokens: 0})
	if len(specs) < 2 {
		t.Fatalf("expected multiple chunks, got %d", len(specs))
	}
	// A chunk that ends mid-word retrieves badly; boundaries should land on whitespace
	// or punctuation.
	for _, spec := range specs[:len(specs)-1] {
		last, _ := utf8.DecodeLastRuneInString(spec.Content)
		if !strings.ContainsRune(" \n.!?;,", last) {
			t.Fatalf("chunk %d ends mid-word at %q", spec.Sequence, spec.Content)
		}
	}
}

func TestChunkShortTextIsOneChunk(t *testing.T) {
	specs := Chunk("A short fact.", ChunkOptions{MaxTokens: 320, OverlapTokens: 48})
	if len(specs) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(specs))
	}
	if specs[0].Content != "A short fact." {
		t.Fatalf("content altered: %q", specs[0].Content)
	}
}

func TestChunkEmptyAndWhitespaceOnly(t *testing.T) {
	if got := Chunk("", ChunkOptions{}); got != nil {
		t.Fatalf("empty text must produce no chunks, got %d", len(got))
	}
	if got := Chunk("   \n\n  ", ChunkOptions{}); len(got) != 0 {
		t.Fatalf("whitespace-only text must produce no chunks, got %d", len(got))
	}
}

func TestChunkHandlesMultibyteText(t *testing.T) {
	text := strings.Repeat("契約は三月三日に締結された。", 40)
	specs := Chunk(text, ChunkOptions{MaxTokens: 20, OverlapTokens: 4})

	runes := []rune(text)
	for _, spec := range specs {
		if !utf8.ValidString(spec.Content) {
			t.Fatalf("chunk %d split a multibyte character", spec.Sequence)
		}
		if got := string(runes[spec.CharStart:spec.CharEnd]); got != spec.Content {
			t.Fatalf("chunk %d char offsets wrong for multibyte text", spec.Sequence)
		}
	}
}

func TestHeuristicTokenizer(t *testing.T) {
	tk := HeuristicTokenizer{}
	if tk.CountTokens("") != 0 {
		t.Fatal("empty text has no tokens")
	}
	if tk.CountTokens("a") != 1 {
		t.Fatal("any non-empty text must count as at least one token")
	}
	long, short := tk.CountTokens(strings.Repeat("word ", 100)), tk.CountTokens("word")
	if long <= short {
		t.Fatal("token count must grow with text length")
	}
	if custom := (HeuristicTokenizer{CharsPerToken: 2}).CountTokens("abcd"); custom != 2 {
		t.Fatalf("custom ratio ignored: got %d, want 2", custom)
	}
}

func TestLocatorForChunkTranslatesToArtifactCoordinates(t *testing.T) {
	doc, err := Decode(MediaTypeMarkdown, []byte("# A\ntext a\n\n## B\ntext b"), Options{})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	episode := doc.Segments[1]
	spec := Chunk(episode.Content, ChunkOptions{MaxTokens: 320})[0]

	loc := LocatorForChunk(episode.Locator, spec)
	if loc.ArtifactCharStart != episode.Locator.ArtifactCharStart+spec.CharStart {
		t.Fatal("chunk locator must be translated into artifact coordinates")
	}
	if loc.Section != "B" {
		t.Fatal("chunk locator must inherit its episode's section")
	}

	// Mutating the derived locator must not corrupt the episode's.
	loc.HeadingPath[0] = "mutated"
	if episode.Locator.HeadingPath[0] == "mutated" {
		t.Fatal("chunk locators must not share mutable state with their episode")
	}
}

func FuzzChunkOffsetContract(f *testing.F) {
	f.Add("hello world, this is a fact about Alice.")
	f.Add("# Heading\n\nbody text\n\n## Another\n\nmore body")
	f.Add(strings.Repeat("multibyte 契約 ", 20))

	f.Fuzz(func(t *testing.T, text string) {
		if !utf8.ValidString(text) {
			t.Skip("segmentation rejects invalid UTF-8 before chunking")
		}
		specs := Chunk(text, ChunkOptions{MaxTokens: 24, OverlapTokens: 4})

		runes := []rune(text)
		for i, spec := range specs {
			if spec.ByteStart < 0 || spec.ByteEnd > len(text) || spec.ByteEnd < spec.ByteStart {
				t.Fatalf("chunk %d has out-of-range byte offsets", i)
			}
			if spec.CharStart < 0 || spec.CharEnd > len(runes) || spec.CharEnd < spec.CharStart {
				t.Fatalf("chunk %d has out-of-range char offsets", i)
			}
			if text[spec.ByteStart:spec.ByteEnd] != spec.Content {
				t.Fatalf("chunk %d byte offsets do not reproduce content", i)
			}
			if string(runes[spec.CharStart:spec.CharEnd]) != spec.Content {
				t.Fatalf("chunk %d char offsets do not reproduce content", i)
			}
			if i > 0 && spec.ByteStart <= specs[i-1].ByteStart {
				t.Fatalf("chunk %d did not advance", i)
			}
		}
	})
}
