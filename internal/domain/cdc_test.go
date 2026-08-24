package domain

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func customerRow() ChangeEvent {
	return ChangeEvent{
		Stream:    "public.customers",
		Operation: ChangeUpdate,
		Key:       map[string]any{"id": float64(42)},
		Before:    map[string]any{"id": float64(42), "tier": "STANDARD", "region": "west"},
		After:     map[string]any{"id": float64(42), "tier": "PREMIUM", "region": "west"},
		Offset:    "0/2000",
		Sequence:  "102",
	}
}

func TestRecordIDIsStableAcrossDeliveries(t *testing.T) {
	// The record id ties every change about one row together. If it varied with map
	// ordering or numeric formatting, an update would look like a new row and a delete
	// would retract nothing.
	first := ChangeEvent{
		Stream: "public.orders",
		Key:    map[string]any{"tenant": "acme", "order_id": float64(7)},
	}
	second := ChangeEvent{
		Stream: "public.orders",
		Key:    map[string]any{"order_id": 7, "tenant": "acme"},
	}

	if first.RecordID() != second.RecordID() {
		t.Fatalf("the same row produced two ids:\n %s\n %s", first.RecordID(), second.RecordID())
	}
	if !strings.Contains(first.RecordID(), "order_id=7") {
		t.Fatalf("a JSON-decoded integer key should not render as a float: %s", first.RecordID())
	}
}

func TestIdempotencyKeyPrefersTheConnectorOffset(t *testing.T) {
	event := customerRow()
	if key := event.IdempotencyKey(); key != "cdc:public.customers:0/2000" {
		t.Fatalf("expected the offset to key the event, got %q", key)
	}

	// A push connector has no offset. The record and its sequence stand in, and two
	// different changes to one row must still differ.
	event.Offset = ""
	withSequence := event.IdempotencyKey()
	event.Sequence = "103"
	if event.IdempotencyKey() == withSequence {
		t.Fatal("two changes to the same row produced the same key")
	}
}

func TestChangedColumnsIgnoresRewritesThatChangeNothing(t *testing.T) {
	event := customerRow()
	if got := event.ChangedColumns(); len(got) != 1 || got[0] != "tier" {
		t.Fatalf("expected only tier to have moved, got %v", got)
	}

	// Some upstreams emit an update for every touched row whether or not a value moved.
	event.After = map[string]any{"id": float64(42), "tier": "STANDARD", "region": "west"}
	if got := event.ChangedColumns(); len(got) != 0 {
		t.Fatalf("a no-op update should report no changes, got %v", got)
	}
}

func TestDeleteDescribesTheRowThatIsGone(t *testing.T) {
	event := ChangeEvent{
		Stream:    "public.customers",
		Operation: ChangeDelete,
		Key:       map[string]any{"id": 42},
		Before:    map[string]any{"id": 42, "tier": "PREMIUM"},
	}
	if err := event.Validate(); err != nil {
		// A delete has no after image, and requiring one would make every tombstone
		// invalid.
		t.Fatalf("a delete with only a before image should be valid: %v", err)
	}
	if event.Image()["tier"] != "PREMIUM" {
		t.Fatal("a delete's image is what was removed")
	}
	if event.Operation.SourceOperation() != SourceOpDelete {
		t.Fatal("a delete must reach the ledger as a delete")
	}
}

func TestChangeEventValidationCatchesUnusableEvents(t *testing.T) {
	cases := map[string]ChangeEvent{
		"no stream": {Operation: ChangeInsert, Key: map[string]any{"id": 1},
			After: map[string]any{"id": 1}},
		"no key": {Stream: "s", Operation: ChangeInsert, After: map[string]any{"id": 1}},
		"insert with no image": {Stream: "s", Operation: ChangeInsert,
			Key: map[string]any{"id": 1}},
		"unknown operation": {Stream: "s", Operation: "merge",
			Key: map[string]any{"id": 1}, After: map[string]any{"id": 1}},
	}
	for name, event := range cases {
		t.Run(name, func(t *testing.T) {
			if err := event.Validate(); err == nil {
				t.Fatal("expected the event to be refused")
			}
		})
	}
}

func TestMappingTypesColumnsAndSkipsWhatItIsTold(t *testing.T) {
	mapping := ChangeMapping{
		SubjectType:       "organization",
		SubjectNameColumn: "name",
		Columns: []ColumnMapping{
			{Column: "name", Predicate: "legal name"},
			{Column: "tier", Predicate: "TIER", ObjectKind: ObjectSymbol},
			{Column: "credit_limit", Predicate: "CREDIT_LIMIT", ObjectKind: ObjectInteger},
			{Column: "region", Predicate: "REGION", SkipEmpty: true},
			{Column: "owner", Predicate: "OWNED_BY", ObjectEntityType: "person"},
			{Column: "absent", Predicate: "NEVER"},
		},
	}
	event := ChangeEvent{
		Stream:    "public.customers",
		Operation: ChangeInsert,
		Key:       map[string]any{"id": 42},
		After: map[string]any{
			"id": 42, "name": "Acme Corporation", "tier": "PREMIUM",
			"credit_limit": float64(50000), "region": "  ", "owner": "Priya Raman",
		},
	}

	cells := mapping.Cells(event)
	if len(cells) != 4 {
		t.Fatalf("expected four cells (blank region and absent column skipped), got %d: %+v",
			len(cells), cells)
	}

	byPredicate := map[string]CellValue{}
	for _, cell := range cells {
		byPredicate[cell.Predicate] = cell
	}
	if _, ok := byPredicate["LEGAL_NAME"]; !ok {
		t.Fatalf("the predicate name should be normalized: %v", byPredicate)
	}
	if got := byPredicate["CREDIT_LIMIT"].Object; got.Kind != ObjectInteger || got.Integer != 50000 {
		t.Fatalf("a numeric column should land in the integer column, got %+v", got)
	}
	if got := byPredicate["TIER"].Object; got.Kind != ObjectSymbol {
		t.Fatalf("a declared symbol should stay a symbol, got %s", got.Kind)
	}
	if got := byPredicate["OWNED_BY"]; got.EntityName != "Priya Raman" || got.EntityType != "person" {
		t.Fatalf("a relation column should name an entity, got %+v", got)
	}
	if !byPredicate["TIER"].Functional {
		// A column holds one current value. Without this an update accumulates a second
		// value beside the first instead of superseding it.
		t.Fatal("a mapped column should be functional by default")
	}
}

func TestMappingFallsBackRatherThanFailingOnBadValues(t *testing.T) {
	// A column that suddenly contains "n/a" where a number was declared should become a
	// slightly wrong claim, not a failed ingest that blocks the whole stream.
	mapping := ChangeMapping{
		SubjectType: "organization",
		Columns:     []ColumnMapping{{Column: "credit_limit", Predicate: "CREDIT_LIMIT", ObjectKind: ObjectInteger}},
	}
	event := ChangeEvent{
		Stream: "s", Operation: ChangeInsert, Key: map[string]any{"id": 1},
		After: map[string]any{"credit_limit": "n/a"},
	}

	cells := mapping.Cells(event)
	if len(cells) != 1 {
		t.Fatalf("expected the claim to survive, got %d cells", len(cells))
	}
	if cells[0].Object.Kind != ObjectString {
		t.Fatalf("expected a string fallback, got %s", cells[0].Object.Kind)
	}
}

func TestMappingValidationRefusesConfigurationsThatRecordNothing(t *testing.T) {
	cases := map[string]ChangeMapping{
		"no subject type": {Columns: []ColumnMapping{{Column: "a", Predicate: "A"}}},
		"no columns":      {SubjectType: "organization"},
		"column without a predicate": {SubjectType: "organization",
			Columns: []ColumnMapping{{Column: "a"}}},
		"duplicate column": {SubjectType: "organization", Columns: []ColumnMapping{
			{Column: "a", Predicate: "A"}, {Column: "a", Predicate: "B"},
		}},
		"unknown object kind": {SubjectType: "organization", Columns: []ColumnMapping{
			{Column: "a", Predicate: "A", ObjectKind: "quaternion"},
		}},
	}
	for name, mapping := range cases {
		t.Run(name, func(t *testing.T) {
			if err := mapping.Validate(); err == nil {
				t.Fatal("expected the mapping to be refused")
			}
		})
	}
}

func TestMappedPredicatesAreFunctionalWithAuthorityResolution(t *testing.T) {
	mapping := ChangeMapping{
		SubjectType: "organization",
		Columns:     []ColumnMapping{{Column: "tier", Predicate: "TIER", ObjectKind: ObjectSymbol}},
	}

	definitions := mapping.PredicateDefinitions(WorkspaceID("w"))
	if len(definitions) != 1 {
		t.Fatalf("expected one definition, got %d", len(definitions))
	}
	if !definitions[0].Functional {
		t.Fatal("a column holds one current value")
	}
	if definitions[0].ConflictPolicy != ConflictPolicyHighestAuthority {
		// Same-source corrections are settled by source ordering. What is left is two
		// systems disagreeing, which a timestamp should not settle.
		t.Fatalf("expected authority resolution, got %s", definitions[0].ConflictPolicy)
	}
	if definitions[0].Status != PredicateApproved {
		t.Fatalf("a declared mapping is not a discovered candidate, got %s", definitions[0].Status)
	}
}

func TestMappingReadsValidityAndIdentityFromTheRow(t *testing.T) {
	mapping := ChangeMapping{
		SubjectType:         "organization",
		SubjectNameColumn:   "name",
		IdentifierNamespace: "erp.customer_id",
		ValidFromColumn:     "effective_from",
		Columns:             []ColumnMapping{{Column: "tier", Predicate: "TIER"}},
	}
	event := ChangeEvent{
		Stream: "public.customers", Operation: ChangeInsert,
		Key: map[string]any{"id": 42},
		After: map[string]any{
			"id": 42, "name": "Acme Corporation", "tier": "PREMIUM",
			"effective_from": "2026-03-01T00:00:00Z",
		},
	}

	if got := mapping.SubjectName(event); got != "Acme Corporation" {
		t.Fatalf("expected the name column, got %q", got)
	}
	namespace, value, ok := mapping.ExternalIdentifier(event)
	if !ok || namespace != "erp.customer_id" || value != "42" {
		t.Fatalf("expected the primary key as an identifier, got %q/%q ok=%v", namespace, value, ok)
	}
	validFrom := mapping.TimeAt(event, mapping.ValidFromColumn)
	if validFrom == nil || !validFrom.Equal(time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("expected the row's own validity, got %v", validFrom)
	}
}

func TestChangeEventSurvivesAJSONRoundTrip(t *testing.T) {
	// The whole event is archived as JSON and decoded again by the pipeline stage. A field
	// that does not survive that trip is a field that silently stops working.
	original := customerRow()
	commit := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	original.CommitTime = &commit
	original.Transaction = "tx-9"
	original.SchemaVersion = "3"

	encoded, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	var decoded ChangeEvent
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}

	if decoded.RecordID() != original.RecordID() {
		t.Fatalf("record id changed across the round trip: %s then %s",
			original.RecordID(), decoded.RecordID())
	}
	if decoded.IdempotencyKey() != original.IdempotencyKey() {
		t.Fatal("idempotency key changed across the round trip")
	}
	if decoded.CommitTime == nil || !decoded.CommitTime.Equal(commit) {
		t.Fatal("commit time did not survive")
	}
	if decoded.Transaction != "tx-9" || decoded.SchemaVersion != "3" {
		t.Fatal("transaction or schema version did not survive")
	}
}
