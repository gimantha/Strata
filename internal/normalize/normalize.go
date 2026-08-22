// Package normalize turns raw source bytes into episodes and chunks deterministically.
//
// Nothing here uses a model. Structure that is already present in the source - JSON
// shape, message boundaries, markdown headings, timestamps - is read directly rather
// than rediscovered by an LLM (AGENTS.md section 10.5). Extraction on top of these
// units arrives in phase 3.
//
// Every unit keeps positional provenance, so a quote can always be reproduced from the
// archived artifact (section 6.6).
package normalize

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gimantha/strata/internal/domain"
)

// Media types this package understands. Anything else is treated as plain text, which
// degrades gracefully instead of rejecting the ingest.
const (
	MediaTypeJSON     = "application/json"
	MediaTypeMarkdown = "text/markdown"
	MediaTypePlain    = "text/plain"
)

// MetadataDirectEpisode marks a payload the caller has already segmented: it becomes
// exactly one episode, whatever its content looks like. The /episodes endpoint uses
// this so direct submission shares the source-event path instead of forking it.
const MetadataDirectEpisode = "direct_episode"

// Document is normalized source material.
//
// Text is the canonical text of the whole artifact, and every segment's locator
// offsets index into it. Concatenating segments reproduces Text exactly, which is what
// makes provenance verifiable rather than approximate.
type Document struct {
	MediaType string
	Text      string
	Segments  []Segment
	Metadata  map[string]any
}

// Segment is a candidate episode: one conversation turn, one document section, one
// JSON record.
type Segment struct {
	Sequence    int64
	Content     string
	ContentType string
	EventTime   *time.Time
	Locator     domain.Locator
	Metadata    map[string]any
}

// Options controls decoding.
type Options struct {
	// DirectEpisode forces single-segment output.
	DirectEpisode bool
}

// Decode parses raw bytes into a canonical document with its segments.
func Decode(mediaType string, raw []byte, opts Options) (Document, error) {
	const op = "normalize.Decode"

	base := baseMediaType(mediaType)
	if !utf8.Valid(raw) {
		return Document{}, domain.Errorf(domain.CodeInvalidArgument, op,
			"payload is not valid UTF-8; binary artifacts are archived but not yet segmented")
	}

	if opts.DirectEpisode {
		return singleSegment(string(raw), base), nil
	}

	switch base {
	case MediaTypeJSON:
		return decodeJSON(raw)
	case MediaTypeMarkdown:
		return decodeMarkdown(string(raw)), nil
	default:
		return singleSegment(string(raw), orPlain(base)), nil
	}
}

// singleSegment wraps a whole payload as one episode.
func singleSegment(text, mediaType string) Document {
	text = normalizeNewlines(text)
	return Document{
		MediaType: mediaType,
		Text:      text,
		Segments: []Segment{{
			Sequence:    0,
			Content:     text,
			ContentType: mediaType,
			Locator:     domain.Locator{ArtifactCharStart: 0, ArtifactCharEnd: utf8.RuneCountInString(text)},
		}},
	}
}

// chatPayload is the conversational shape: {"messages":[{"role","content","timestamp"}]}.
type chatPayload struct {
	Messages []chatMessage `json:"messages"`
}

type chatMessage struct {
	Role      string         `json:"role"`
	Content   string         `json:"content"`
	Name      string         `json:"name,omitempty"`
	Timestamp *time.Time     `json:"timestamp,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`
}

// decodeJSON recognizes three shapes: a conversation, an array of records, and a
// single object. Each maps to a different natural episode boundary.
func decodeJSON(raw []byte) (Document, error) {
	const op = "normalize.decodeJSON"

	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return Document{}, domain.Errorf(domain.CodeInvalidArgument, op, "payload is empty")
	}

	// A conversation: one episode per turn, preserving order and per-turn event time.
	if trimmed[0] == '{' {
		var chat chatPayload
		if err := json.Unmarshal(trimmed, &chat); err == nil && len(chat.Messages) > 0 {
			return decodeChat(chat), nil
		}
	}

	// An array: one episode per record.
	if trimmed[0] == '[' {
		var records []json.RawMessage
		if err := json.Unmarshal(trimmed, &records); err != nil {
			return Document{}, domain.Wrap(err, domain.CodeInvalidArgument, op, "malformed JSON array")
		}
		return decodeRecords(records), nil
	}

	// Any other valid JSON: one episode holding its canonical rendering.
	var probe any
	if err := json.Unmarshal(trimmed, &probe); err != nil {
		return Document{}, domain.Wrap(err, domain.CodeInvalidArgument, op, "malformed JSON")
	}
	canonical, err := canonicalJSON(trimmed)
	if err != nil {
		return Document{}, domain.Wrap(err, domain.CodeInvalidArgument, op, "cannot canonicalize JSON")
	}
	doc := singleSegment(canonical, MediaTypeJSON)
	doc.Segments[0].Locator.JSONPointer = ""
	return doc, nil
}

func decodeChat(chat chatPayload) Document {
	b := newTextBuilder()
	segments := make([]Segment, 0, len(chat.Messages))

	for i, msg := range chat.Messages {
		content := normalizeNewlines(msg.Content)
		// The rendered form keeps the speaker attached to the words, so a retrieved
		// excerpt is still attributable on its own.
		rendered := content
		if msg.Role != "" {
			rendered = msg.Role + ": " + content
		}
		start, end := b.append(rendered)

		index := i
		segments = append(segments, Segment{
			Sequence:    int64(i),
			Content:     rendered,
			ContentType: MediaTypePlain,
			EventTime:   msg.Timestamp,
			Locator: domain.Locator{
				MessageIndex:      &index,
				Role:              msg.Role,
				JSONPointer:       fmt.Sprintf("/messages/%d", i),
				ArtifactCharStart: start,
				ArtifactCharEnd:   end,
			},
			Metadata: chatSegmentMetadata(msg),
		})
	}

	return Document{
		MediaType: MediaTypeJSON,
		Text:      b.string(),
		Segments:  segments,
		Metadata:  map[string]any{"shape": "chat", "message_count": len(chat.Messages)},
	}
}

func chatSegmentMetadata(msg chatMessage) map[string]any {
	if msg.Name == "" && len(msg.Metadata) == 0 {
		return nil
	}
	out := map[string]any{}
	for k, v := range msg.Metadata {
		out[k] = v
	}
	if msg.Name != "" {
		out["speaker_name"] = msg.Name
	}
	return out
}

func decodeRecords(records []json.RawMessage) Document {
	b := newTextBuilder()
	segments := make([]Segment, 0, len(records))

	for i, rec := range records {
		content := renderRecord(rec)
		start, end := b.append(content)
		segments = append(segments, Segment{
			Sequence:    int64(i),
			Content:     content,
			ContentType: MediaTypeJSON,
			EventTime:   recordEventTime(rec),
			Locator: domain.Locator{
				JSONPointer:       fmt.Sprintf("/%d", i),
				RowKey:            recordKey(rec),
				ArtifactCharStart: start,
				ArtifactCharEnd:   end,
			},
		})
	}

	return Document{
		MediaType: MediaTypeJSON,
		Text:      b.string(),
		Segments:  segments,
		Metadata:  map[string]any{"shape": "records", "record_count": len(records)},
	}
}

// renderRecord produces the canonical text for one record. Strings are used verbatim;
// anything else is rendered as stable, indented JSON.
func renderRecord(rec json.RawMessage) string {
	var asString string
	if err := json.Unmarshal(rec, &asString); err == nil {
		return normalizeNewlines(asString)
	}
	canonical, err := canonicalJSON(rec)
	if err != nil {
		return normalizeNewlines(string(rec))
	}
	return canonical
}

// recordKey picks a stable primary key from a record when one is obvious, so CDC rows
// stay identifiable in provenance without any configuration.
func recordKey(rec json.RawMessage) string {
	var obj map[string]any
	if err := json.Unmarshal(rec, &obj); err != nil {
		return ""
	}
	for _, field := range []string{"id", "ID", "_id", "uuid", "key", "pk", "primary_key"} {
		if v, ok := obj[field]; ok {
			return fmt.Sprint(v)
		}
	}
	return ""
}

// recordEventTime reads a world-time hint the source already provided. Time supplied
// by a source is preferred over anything inferred later (section 10.5).
func recordEventTime(rec json.RawMessage) *time.Time {
	var obj map[string]any
	if err := json.Unmarshal(rec, &obj); err != nil {
		return nil
	}
	for _, field := range []string{"event_time", "timestamp", "occurred_at", "created_at", "updated_at"} {
		v, ok := obj[field]
		if !ok {
			continue
		}
		s, ok := v.(string)
		if !ok {
			continue
		}
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02"} {
			if parsed, err := time.Parse(layout, s); err == nil {
				utc := parsed.UTC()
				return &utc
			}
		}
	}
	return nil
}

// decodeMarkdown splits on ATX headings so each section becomes one episode. Sections
// are the natural retrieval unit for documents, and the heading path gives a retrieved
// excerpt its context.
func decodeMarkdown(text string) Document {
	text = normalizeNewlines(text)
	lines := strings.Split(text, "\n")

	type section struct {
		level   int
		title   string
		path    []string
		content []string
	}

	var (
		sections []section
		current  = section{level: 0}
		headings []string
	)

	flush := func() {
		if len(current.content) == 0 && current.title == "" {
			return
		}
		if strings.TrimSpace(strings.Join(current.content, "\n")) == "" && current.title == "" {
			return
		}
		sections = append(sections, current)
	}

	for _, line := range lines {
		level, title, isHeading := parseHeading(line)
		if !isHeading {
			current.content = append(current.content, line)
			continue
		}

		flush()
		// Maintain the heading stack so nested sections know their ancestry.
		if level-1 < len(headings) {
			headings = headings[:level-1]
		}
		for len(headings) < level-1 {
			headings = append(headings, "")
		}
		headings = append(headings, title)

		path := make([]string, len(headings))
		copy(path, headings)
		current = section{level: level, title: title, path: path, content: []string{line}}
	}
	flush()

	if len(sections) == 0 {
		return singleSegment(text, MediaTypeMarkdown)
	}

	b := newTextBuilder()
	segments := make([]Segment, 0, len(sections))
	for i, sec := range sections {
		content := strings.TrimRight(strings.Join(sec.content, "\n"), "\n")
		start, end := b.append(content)
		level := sec.level
		segments = append(segments, Segment{
			Sequence:    int64(i),
			Content:     content,
			ContentType: MediaTypeMarkdown,
			Locator: domain.Locator{
				Section:           sec.title,
				HeadingPath:       sec.path,
				LineStart:         nil,
				ArtifactCharStart: start,
				ArtifactCharEnd:   end,
				Extra:             map[string]any{"heading_level": level},
			},
		})
	}

	return Document{
		MediaType: MediaTypeMarkdown,
		Text:      b.string(),
		Segments:  segments,
		Metadata:  map[string]any{"shape": "markdown", "section_count": len(sections)},
	}
}

// parseHeading recognizes an ATX markdown heading.
func parseHeading(line string) (level int, title string, ok bool) {
	trimmed := strings.TrimLeft(line, " ")
	if !strings.HasPrefix(trimmed, "#") {
		return 0, "", false
	}
	hashes := 0
	for hashes < len(trimmed) && trimmed[hashes] == '#' {
		hashes++
	}
	if hashes > 6 || hashes >= len(trimmed) || trimmed[hashes] != ' ' {
		return 0, "", false
	}
	return hashes, strings.TrimSpace(trimmed[hashes+1:]), true
}

// textBuilder assembles canonical text while recording each part's rune offsets, so
// segment locators always index back into the text exactly.
type textBuilder struct {
	sb    strings.Builder
	runes int
}

const segmentSeparator = "\n\n"

func newTextBuilder() *textBuilder { return &textBuilder{} }

func (b *textBuilder) append(part string) (start, end int) {
	if b.runes > 0 {
		b.sb.WriteString(segmentSeparator)
		b.runes += utf8.RuneCountInString(segmentSeparator)
	}
	start = b.runes
	b.sb.WriteString(part)
	b.runes += utf8.RuneCountInString(part)
	return start, b.runes
}

func (b *textBuilder) string() string { return b.sb.String() }

// canonicalJSON re-renders JSON with stable key order and indentation, so identical
// data produces byte-identical text and chunk boundaries do not shift between runs.
func canonicalJSON(raw []byte) (string, error) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", err
	}
	// encoding/json sorts map keys, which is what makes the output stable.
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// normalizeNewlines makes offsets platform-independent: a CRLF source and an LF source
// with the same words must produce the same chunk boundaries.
func normalizeNewlines(s string) string {
	if !strings.ContainsRune(s, '\r') {
		return s
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\r", "\n")
}

func baseMediaType(mediaType string) string {
	base, _, _ := strings.Cut(mediaType, ";")
	return strings.ToLower(strings.TrimSpace(base))
}

func orPlain(mediaType string) string {
	if mediaType == "" {
		return MediaTypePlain
	}
	return mediaType
}
