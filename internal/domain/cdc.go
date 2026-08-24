package domain

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ChangeOperation is what an upstream row change did (AGENTS.md section 11.1).
type ChangeOperation string

const (
	ChangeInsert ChangeOperation = "insert"
	ChangeUpdate ChangeOperation = "update"
	ChangeDelete ChangeOperation = "delete"
	// ChangeSnapshot is a row read during an initial table scan rather than from the
	// log. Distinguished from an insert because a snapshot row is a statement about
	// current state, not about something having just happened.
	ChangeSnapshot ChangeOperation = "snapshot"
)

var changeOperations = []ChangeOperation{
	ChangeInsert, ChangeUpdate, ChangeDelete, ChangeSnapshot,
}

func ParseChangeOperation(s string) (ChangeOperation, error) {
	return parseEnum("change operation", s, changeOperations)
}

// SourceOperation maps a row change onto the ingestion vocabulary.
//
// A delete becomes a delete event rather than disappearing: the ledger records that the
// source stopped claiming the record, which is not the same as the record never having been
// true (AGENTS.md section 11.3).
func (o ChangeOperation) SourceOperation() SourceOperation {
	switch o {
	case ChangeDelete:
		return SourceOpDelete
	case ChangeSnapshot:
		return SourceOpSnapshot
	default:
		return SourceOpUpsert
	}
}

// ChangeEvent is one upstream row change, in a form no particular database owns
// (AGENTS.md sections 10.1 and 11.1).
//
// The shape follows what every CDC pipeline already carries — Debezium, logical decoding, a
// Kafka change topic, or a file of exported rows — so an adapter is a translation rather
// than an interpretation. Anything a connector cannot supply is optional; nothing here is
// required except what makes an event identifiable and ordered.
type ChangeEvent struct {
	// Stream names the table or topic: "public.customers". It is the unit that carries a
	// checkpoint and a mapping.
	Stream string `json:"stream"`

	Database string `json:"database,omitempty"`
	Schema   string `json:"schema,omitempty"`
	Table    string `json:"table,omitempty"`

	Operation ChangeOperation `json:"operation"`

	// Key is the primary key, column by column. Kept separate from the images because a
	// delete may carry nothing else, and because the key is what identifies the record
	// across events.
	Key map[string]any `json:"key"`
	// Before and After are the row images, when the upstream permits them. A delete
	// usually has only Before; an insert only After.
	Before map[string]any `json:"before,omitempty"`
	After  map[string]any `json:"after,omitempty"`

	// CommitTime is when the upstream transaction committed — source time, not our time.
	CommitTime *time.Time `json:"commit_time,omitempty"`
	// EventTime is when the described thing happened in the world, if the row says so.
	EventTime *time.Time `json:"event_time,omitempty"`

	// Offset is the connector's position: an LSN, a file offset, a Kafka offset. It is
	// opaque here and is what a checkpoint stores.
	Offset string `json:"offset,omitempty"`
	// Sequence orders events within a stream. Ordering uses this rather than arrival,
	// because arrival order is a property of the network (AGENTS.md section 11.4).
	Sequence string `json:"sequence,omitempty"`
	// Transaction groups changes committed together.
	Transaction string `json:"transaction,omitempty"`
	// SchemaVersion records the upstream schema this row was shaped by, so a later
	// column rename is explicable rather than mysterious.
	SchemaVersion string `json:"schema_version,omitempty"`

	Metadata map[string]any `json:"metadata,omitempty"`
}

func (e ChangeEvent) Validate() error {
	const op = "domain.ChangeEvent.Validate"

	if strings.TrimSpace(e.Stream) == "" {
		return Errorf(CodeInvalidArgument, op, "stream is required")
	}
	if _, err := ParseChangeOperation(string(e.Operation)); err != nil {
		return err
	}
	if len(e.Key) == 0 {
		// Without a key a change cannot be tied to the record it changed, which makes
		// every update look like a new row and every delete unattributable.
		return Errorf(CodeInvalidArgument, op, "a primary key is required")
	}
	if e.Operation != ChangeDelete && len(e.After) == 0 {
		return Errorf(CodeInvalidArgument, op,
			"%s requires an after image", e.Operation)
	}
	return nil
}

// RecordID is the stable identity of the changed row within its stream.
//
// Composed from the key columns in sorted order so the same row always produces the same
// identifier regardless of how a connector ordered the map.
func (e ChangeEvent) RecordID() string {
	columns := make([]string, 0, len(e.Key))
	for column := range e.Key {
		columns = append(columns, column)
	}
	sort.Strings(columns)

	parts := make([]string, 0, len(columns))
	for _, column := range columns {
		parts = append(parts, column+"="+scalarText(e.Key[column]))
	}
	return e.Stream + ":" + strings.Join(parts, ",")
}

// Image is the row this event describes: the state after the change, or the state that was
// removed. A delete describes what is gone, and that is the only image it has.
func (e ChangeEvent) Image() map[string]any {
	if e.Operation == ChangeDelete && len(e.After) == 0 {
		return e.Before
	}
	return e.After
}

// IdempotencyKey identifies this change for replay.
//
// Offset first, since a connector's position is exactly the thing that is unique per event
// in a log. Where there is no offset — a push connector, an exported file — the record and
// its sequence stand in. Two deliveries of the same change must produce the same key, and
// two different changes must not.
func (e ChangeEvent) IdempotencyKey() string {
	switch {
	case e.Offset != "":
		return "cdc:" + e.Stream + ":" + e.Offset
	case e.Sequence != "":
		return "cdc:" + e.RecordID() + ":" + e.Sequence
	default:
		return "cdc:" + e.RecordID() + ":" + string(e.Operation)
	}
}

// ChangedColumns reports which columns differ between the images.
//
// This is what makes an update cheap: only the columns that actually moved need to produce
// new claims, and a row rewritten with identical values produces none
// (AGENTS.md section 11.2).
func (e ChangeEvent) ChangedColumns() []string {
	if len(e.Before) == 0 {
		return nil
	}

	changed := make([]string, 0, len(e.After))
	for column, after := range e.After {
		before, existed := e.Before[column]
		if !existed || scalarText(before) != scalarText(after) {
			changed = append(changed, column)
		}
	}
	for column := range e.Before {
		if _, still := e.After[column]; !still {
			changed = append(changed, column)
		}
	}
	sort.Strings(changed)
	return changed
}

// ColumnMapping turns one column into one claim (AGENTS.md section 11.2).
type ColumnMapping struct {
	Column    string `json:"column"`
	Predicate string `json:"predicate"`
	// ObjectKind is the typed column the value lands in. Defaults to string.
	ObjectKind ObjectKind `json:"object_kind,omitempty"`
	// ObjectEntityType makes this column a relation: the value names another entity of
	// this type rather than being a literal.
	ObjectEntityType string `json:"object_entity_type,omitempty"`
	// SkipEmpty omits the claim when the column is null or blank, rather than asserting
	// emptiness. A null column usually means "not recorded", not "recorded as nothing".
	SkipEmpty bool `json:"skip_empty,omitempty"`
	// Functional declares that the column holds one current value, which is what makes an
	// update supersede rather than accumulate. True by default for a row column: that is
	// what a column is.
	Functional *bool `json:"functional,omitempty"`
}

// IsFunctional reports the effective setting.
func (c ColumnMapping) IsFunctional() bool {
	if c.Functional == nil {
		return true
	}
	return *c.Functional
}

// ChangeMapping says how rows in one stream become knowledge.
//
// Deterministic on purpose. A database row is already structured, and asking a model to
// rediscover that structure would be slower, more expensive, and less reliable than reading
// the columns — the same argument the phase 1 stages make about conversation turns and
// markdown headings.
type ChangeMapping struct {
	// SubjectType is the entity type every row in this stream describes.
	SubjectType string `json:"subject_type"`
	// SubjectNameColumn holds the human-readable name. Without one, the key is the name,
	// which is ugly but honest.
	SubjectNameColumn string `json:"subject_name_column,omitempty"`
	// IdentifierNamespace registers the primary key as a stable external identifier, so
	// resolution matches on the key rather than guessing from the name. This is rung one
	// of the resolution ladder and the reason CDC identities are reliable.
	IdentifierNamespace string `json:"identifier_namespace,omitempty"`

	// ValidFromColumn and ValidToColumn read world validity out of the row when it
	// carries it, instead of assuming the fact began when the row was written.
	ValidFromColumn string `json:"valid_from_column,omitempty"`
	ValidToColumn   string `json:"valid_to_column,omitempty"`

	Columns []ColumnMapping `json:"columns"`
}

func (m ChangeMapping) Validate() error {
	const op = "domain.ChangeMapping.Validate"

	if strings.TrimSpace(m.SubjectType) == "" {
		return Errorf(CodeInvalidArgument, op, "subject_type is required")
	}
	if len(m.Columns) == 0 {
		return Errorf(CodeInvalidArgument, op,
			"a mapping with no columns would ingest rows and record nothing")
	}

	seen := map[string]bool{}
	for _, column := range m.Columns {
		if strings.TrimSpace(column.Column) == "" {
			return Errorf(CodeInvalidArgument, op, "a column mapping needs a column name")
		}
		if strings.TrimSpace(column.Predicate) == "" {
			return Errorf(CodeInvalidArgument, op,
				"column %q needs a predicate", column.Column)
		}
		if seen[column.Column] {
			return Errorf(CodeInvalidArgument, op,
				"column %q is mapped twice", column.Column)
		}
		seen[column.Column] = true

		if column.ObjectKind != "" {
			if _, err := ParseObjectKind(string(column.ObjectKind)); err != nil {
				return err
			}
		}
	}
	return nil
}

// PredicateDefinitions renders a mapping's columns as registry entries.
//
// A column holds one current value, so its predicate is functional by default — that is what
// makes an update supersede the old value rather than accumulate beside it. Cross-source
// disagreement still goes to authority weighting rather than latest-wins: the same source
// correcting itself is handled by source ordering, and two different systems disagreeing
// about the same field is a real conflict that a timestamp should not settle.
func (m ChangeMapping) PredicateDefinitions(ws WorkspaceID) []PredicateDefinition {
	out := make([]PredicateDefinition, 0, len(m.Columns))
	for _, column := range m.Columns {
		definition := PredicateDefinition{
			WorkspaceID:       ws,
			Name:              NormalizePredicateName(column.Predicate),
			Description:       "mapped from column " + column.Column,
			Functional:        column.IsFunctional(),
			TemporalPolicy:    TemporalPolicyStateful,
			ConflictPolicy:    ConflictPolicyHighestAuthority,
			DefaultMemoryKind: MemorySemantic,
			Sensitivity:       ClassificationInternal,
			Status:            PredicateApproved,
		}
		if m.SubjectType != "" {
			definition.SubjectTypes = []string{NormalizeEntityType(m.SubjectType)}
		}
		if column.ObjectEntityType != "" {
			definition.ObjectTypes = []string{NormalizeEntityType(column.ObjectEntityType)}
			definition.ObjectKinds = []ObjectKind{ObjectEntity}
		} else if column.ObjectKind != "" {
			definition.ObjectKinds = []ObjectKind{column.ObjectKind}
		}
		out = append(out, definition)
	}
	return out
}

// SubjectName reads the display name for a row, falling back to the key.
func (m ChangeMapping) SubjectName(event ChangeEvent) string {
	image := event.Image()
	if m.SubjectNameColumn != "" {
		if value, ok := image[m.SubjectNameColumn]; ok {
			if text := strings.TrimSpace(scalarText(value)); text != "" {
				return text
			}
		}
	}
	return event.RecordID()
}

// ExternalIdentifier renders the primary key as a resolution identifier.
func (m ChangeMapping) ExternalIdentifier(event ChangeEvent) (namespace, value string, ok bool) {
	if m.IdentifierNamespace == "" || len(event.Key) == 0 {
		return "", "", false
	}

	columns := make([]string, 0, len(event.Key))
	for column := range event.Key {
		columns = append(columns, column)
	}
	sort.Strings(columns)

	parts := make([]string, 0, len(columns))
	for _, column := range columns {
		parts = append(parts, scalarText(event.Key[column]))
	}
	return m.IdentifierNamespace, strings.Join(parts, ":"), true
}

// CellValue is one column's contribution to knowledge, already typed.
type CellValue struct {
	Column     string
	Predicate  string
	Object     AssertionObject
	EntityName string
	EntityType string
	Functional bool
}

// Cells turns a row image into the claims it states, in mapping order.
//
// Ordering is the mapping's, not the map's, so the same row always produces the same claims
// in the same order — which matters because fingerprints and replay both depend on it.
func (m ChangeMapping) Cells(event ChangeEvent) []CellValue {
	image := event.Image()
	out := make([]CellValue, 0, len(m.Columns))

	for _, column := range m.Columns {
		raw, present := image[column.Column]
		text := strings.TrimSpace(scalarText(raw))
		if !present || (column.SkipEmpty && text == "") {
			continue
		}

		cell := CellValue{
			Column:     column.Column,
			Predicate:  NormalizePredicateName(column.Predicate),
			Functional: column.IsFunctional(),
		}
		if column.ObjectEntityType != "" {
			if text == "" {
				continue
			}
			cell.EntityName = text
			cell.EntityType = column.ObjectEntityType
			out = append(out, cell)
			continue
		}
		cell.Object = objectOf(column.ObjectKind, raw, text)
		out = append(out, cell)
	}
	return out
}

// objectOf types a column value, falling back to a string when a declared kind cannot hold
// the value it was given. A row that suddenly contains "n/a" in a numeric column should
// become a slightly wrong claim, not a failed ingest.
func objectOf(kind ObjectKind, raw any, text string) AssertionObject {
	switch kind {
	case ObjectInteger:
		if n, err := strconv.ParseInt(text, 10, 64); err == nil {
			return ObjectOfInteger(n)
		}
	case ObjectDecimal:
		if _, err := strconv.ParseFloat(text, 64); err == nil {
			return ObjectOfDecimal(text)
		}
	case ObjectBoolean:
		if b, err := strconv.ParseBool(text); err == nil {
			return ObjectOfBool(b)
		}
	case ObjectTimestamp:
		if t, err := time.Parse(time.RFC3339, text); err == nil {
			return ObjectOfTimestamp(t)
		}
	case ObjectDate:
		if t, err := time.Parse(DateLayout, text); err == nil {
			return ObjectOfDate(t)
		}
	case ObjectSymbol:
		return ObjectOfSymbol(text)
	case ObjectURI:
		return ObjectOfURI(text)
	}
	_ = raw
	return ObjectOfString(text)
}

// TimeAt reads a timestamp column, tolerating the formats connectors actually emit.
func (m ChangeMapping) TimeAt(event ChangeEvent, column string) *time.Time {
	if column == "" {
		return nil
	}
	raw, ok := event.Image()[column]
	if !ok {
		return nil
	}
	text := strings.TrimSpace(scalarText(raw))
	if text == "" {
		return nil
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, DateLayout} {
		if parsed, err := time.Parse(layout, text); err == nil {
			utc := parsed.UTC()
			return &utc
		}
	}
	return nil
}

// StreamCheckpoint is a connector's durable position in one stream (AGENTS.md section 11.1).
type StreamCheckpoint struct {
	WorkspaceID WorkspaceID
	SourceID    SourceID
	Stream      string

	LastOffset     string
	LastSequence   string
	LastCommitTime *time.Time
	EventsConsumed int64
	UpdatedAt      time.Time
}

// CDCStream binds a stream to the mapping that interprets it and the checkpoint that
// resumes it. They live together because they are the same unit of configuration: a stream
// nobody can interpret is not worth checkpointing.
type CDCStream struct {
	ID          CDCStreamID
	WorkspaceID WorkspaceID
	SourceID    SourceID
	Stream      string
	Mapping     ChangeMapping
	Checkpoint  StreamCheckpoint
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// MediaTypeChangeRow marks a payload that is one CDC row rather than prose.
const MediaTypeChangeRow = "application/vnd.strata.change-row+json"

// EventTypeChangeRow labels the source event a change produces.
const EventTypeChangeRow = "cdc.row"

// scalarText renders a column value as text without inventing precision.
//
// JSON decoding turns every number into a float64, so an integer key would otherwise render
// as "1.0000000000" or "1e+06" and stop matching itself between deliveries.
func scalarText(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return typed
	case bool:
		return strconv.FormatBool(typed)
	case float64:
		if typed == float64(int64(typed)) {
			return strconv.FormatInt(int64(typed), 10)
		}
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case float32:
		return scalarText(float64(typed))
	case int:
		return strconv.Itoa(typed)
	case int64:
		return strconv.FormatInt(typed, 10)
	case json.Number:
		return typed.String()
	case time.Time:
		return typed.UTC().Format(time.RFC3339Nano)
	default:
		encoded, err := json.Marshal(typed)
		if err != nil {
			return fmt.Sprintf("%v", typed)
		}
		return string(encoded)
	}
}
