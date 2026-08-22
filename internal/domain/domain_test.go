package domain

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestDeriveKeyIsStableAndSeparated(t *testing.T) {
	a := DeriveKey("ns", "ab", "c")
	if a != DeriveKey("ns", "ab", "c") {
		t.Fatal("DeriveKey must be deterministic across calls")
	}
	// NUL separation: differing part boundaries must not collide.
	if a == DeriveKey("ns", "a", "bc") {
		t.Fatal("part boundaries must affect the derived key")
	}
	// Namespace separation.
	if a == DeriveKey("other", "ab", "c") {
		t.Fatal("namespace must affect the derived key")
	}
	if len(a) != 64 {
		t.Fatalf("expected a 64-char hex digest, got %d chars", len(a))
	}
}

func TestDeriveIdempotencyKeyDetectsChangedContent(t *testing.T) {
	src := SourceID("11111111-1111-7111-8111-111111111111")
	same := DeriveIdempotencyKey(src, "row-1", "v3", "hashA")
	if same != DeriveIdempotencyKey(src, "row-1", "v3", "hashA") {
		t.Fatal("identical inputs must derive the same key")
	}
	if same == DeriveIdempotencyKey(src, "row-1", "v3", "hashB") {
		t.Fatal("changed content must derive a different key")
	}
	if same == DeriveIdempotencyKey(src, "row-1", "v4", "hashA") {
		t.Fatal("changed source version must derive a different key")
	}
}

func TestContentHashMatchesKnownVector(t *testing.T) {
	// sha256("") - guards against an accidental change of hash algorithm, which
	// would silently orphan every archived blob.
	const emptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if got := ContentHash(nil); got != emptySHA256 {
		t.Fatalf("ContentHash(nil) = %s, want %s", got, emptySHA256)
	}
}

func TestParseEnumRejectsUnknownValues(t *testing.T) {
	cases := []struct {
		name string
		fn   func(string) error
	}{
		{"source kind", func(s string) error { _, err := ParseSourceKind(s); return err }},
		{"source operation", func(s string) error { _, err := ParseSourceOperation(s); return err }},
		{"trust level", func(s string) error { _, err := ParseTrustLevel(s); return err }},
		{"classification", func(s string) error { _, err := ParseClassification(s); return err }},
		{"memory kind", func(s string) error { _, err := ParseMemoryKind(s); return err }},
		{"run status", func(s string) error { _, err := ParseRunStatus(s); return err }},
		{"outbox status", func(s string) error { _, err := ParseOutboxStatus(s); return err }},
		{"role", func(s string) error { _, err := ParseRole(s); return err }},
		{"principal kind", func(s string) error { _, err := ParsePrincipalKind(s); return err }},
		{"error class", func(s string) error { _, err := ParseErrorClass(s); return err }},
		{"source event status", func(s string) error { _, err := ParseSourceEventStatus(s); return err }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, bad := range []string{"", "nonsense", "EPISODIC", " chat"} {
				err := tc.fn(bad)
				if err == nil {
					t.Fatalf("expected an error for %q", bad)
				}
				if !IsCode(err, CodeInvalidArgument) {
					t.Fatalf("expected invalid_argument for %q, got %s", bad, CodeOf(err))
				}
			}
		})
	}
}

func TestParseEnumAcceptsEveryDeclaredValue(t *testing.T) {
	for _, k := range sourceKinds {
		if _, err := ParseSourceKind(string(k)); err != nil {
			t.Fatalf("declared source kind %q must parse: %v", k, err)
		}
	}
	for _, s := range outboxStatuses {
		if _, err := ParseOutboxStatus(string(s)); err != nil {
			t.Fatalf("declared outbox status %q must parse: %v", s, err)
		}
	}
}

func TestTemporalCoordinatesValidate(t *testing.T) {
	now := time.Date(2026, 4, 20, 10, 0, 0, 0, time.UTC)
	earlier := now.Add(-time.Hour)

	base := TemporalCoordinates{ObservedAt: now, RecordedAt: now}
	if err := base.Validate(); err != nil {
		t.Fatalf("minimal coordinates must validate: %v", err)
	}

	missing := TemporalCoordinates{RecordedAt: now}
	if err := missing.Validate(); err == nil {
		t.Fatal("observed_at is mandatory: knowledge time cannot be unknown")
	}

	inverted := base
	inverted.ValidFrom = &now
	inverted.ValidTo = &earlier
	if err := inverted.Validate(); err == nil {
		t.Fatal("an interval ending before it starts must be rejected")
	}

	// A valid interval with no end (still true) is legitimate.
	open := base
	open.ValidFrom = &earlier
	if err := open.Validate(); err != nil {
		t.Fatalf("an open-ended validity interval must be accepted: %v", err)
	}
}

func TestTemporalCoordinatesNormalizeForcesUTC(t *testing.T) {
	zone := time.FixedZone("UTC+5:30", int(5.5*float64(time.Hour/time.Second)))
	local := time.Date(2026, 3, 25, 12, 0, 0, 0, zone)

	tc := TemporalCoordinates{
		ObservedAt: local, RecordedAt: local,
		EventTime: &local, ValidFrom: &local, ExpiresAt: &local,
	}.Normalize()

	if tc.ObservedAt.Location() != time.UTC {
		t.Fatal("observed_at must be normalized to UTC")
	}
	if tc.EventTime.Location() != time.UTC || tc.ValidFrom.Location() != time.UTC {
		t.Fatal("world-time fields must be normalized to UTC")
	}
	if !tc.EventTime.Equal(local) {
		t.Fatal("normalization must preserve the instant, only change the zone")
	}
	if tc.ValidTo != nil {
		t.Fatal("normalize must leave unset optional fields nil")
	}
}

func TestErrorWrappingPreservesCodeAndCause(t *testing.T) {
	cause := errors.New("connection reset")
	err := Wrap(cause, CodeProviderUnavailable, "ledger.Append", "insert failed")

	if !errors.Is(err, cause) {
		t.Fatal("wrapped cause must remain reachable via errors.Is")
	}
	if CodeOf(err) != CodeProviderUnavailable {
		t.Fatalf("expected provider_unavailable, got %s", CodeOf(err))
	}
	if Wrap(nil, CodeInternal, "op", "msg") != nil {
		t.Fatal("wrapping nil must return nil")
	}
	// An error that never passed through the domain must not look like a client error.
	if CodeOf(errors.New("raw")) != CodeInternal {
		t.Fatal("unclassified errors must default to internal")
	}
	if CodeOf(nil) != "" {
		t.Fatal("CodeOf(nil) must be empty")
	}
	if got := fmt.Sprint(err); got == "" {
		t.Fatal("error message must not be empty")
	}
}

func TestErrorClassRetryability(t *testing.T) {
	retryable := map[ErrorClass]bool{
		ErrorClassTransient:         true,
		ErrorClassRateLimited:       true,
		ErrorClassStorageConflict:   true,
		ErrorClassInternal:          true,
		ErrorClassInvalidSourceData: false,
		ErrorClassSchema:            false,
		ErrorClassPolicy:            false,
		ErrorClassModelValidation:   false,
	}
	for class, want := range retryable {
		if class.Retryable() != want {
			t.Fatalf("%s.Retryable() = %v, want %v", class, class.Retryable(), want)
		}
	}
}

func TestClassifyError(t *testing.T) {
	cases := map[Code]ErrorClass{
		CodeInvalidArgument:     ErrorClassInvalidSourceData,
		CodeOntologyViolation:   ErrorClassSchema,
		CodePermissionDenied:    ErrorClassPolicy,
		CodeRateLimited:         ErrorClassRateLimited,
		CodeProviderUnavailable: ErrorClassTransient,
		CodeSourceEventConflict: ErrorClassStorageConflict,
		CodeInternal:            ErrorClassInternal,
	}
	for code, want := range cases {
		got := ClassifyError(Errorf(code, "op", "boom"))
		if got != want {
			t.Fatalf("ClassifyError(%s) = %s, want %s", code, got, want)
		}
	}
	// Bad input must never be retried forever.
	if ClassifyError(Errorf(CodeInvalidArgument, "op", "bad")).Retryable() {
		t.Fatal("invalid source data must not be retryable")
	}
}

func TestRoleAtLeast(t *testing.T) {
	if !RoleOwner.AtLeast(RoleReader) || !RoleAdmin.AtLeast(RoleWriter) {
		t.Fatal("higher roles must include lower privileges")
	}
	if RoleReader.AtLeast(RoleWriter) {
		t.Fatal("reader must not satisfy writer")
	}
	if Role("bogus").AtLeast(RoleReader) {
		t.Fatal("an unknown role must satisfy nothing")
	}
}

func TestPrincipalGrantLookupIsWorkspaceScoped(t *testing.T) {
	p := Principal{
		ID:   "p1",
		Kind: PrincipalUser,
		Grants: []Grant{
			{WorkspaceID: "ws-a", Role: RoleWriter},
			{WorkspaceID: "ws-b", Role: RoleReader},
		},
	}
	if role, ok := p.GrantFor("ws-a"); !ok || role != RoleWriter {
		t.Fatalf("expected writer on ws-a, got %q (ok=%v)", role, ok)
	}
	if _, ok := p.GrantFor("ws-c"); ok {
		t.Fatal("a principal must not hold a grant it was never given")
	}
}

func TestRecordValidation(t *testing.T) {
	now := time.Now().UTC()

	if err := (Workspace{Slug: "Bad Slug", Name: "x"}).Validate(); err == nil {
		t.Fatal("slugs with spaces and capitals must be rejected")
	}
	if err := (Workspace{Slug: "acme", Name: "Acme"}).Validate(); err != nil {
		t.Fatalf("valid workspace rejected: %v", err)
	}
	if err := (GraphSpace{Slug: "main", Name: "Main"}).Validate(); err == nil {
		t.Fatal("a graph space without a workspace must be rejected")
	}

	event := SourceEvent{
		WorkspaceID: "ws", GraphSpaceID: "gs", SourceID: "src",
		ContentHash: "abc", IdempotencyKey: "key",
		Operation: SourceOpUpsert, Status: SourceEventAccepted,
		ObservedAt: now, RecordedAt: now,
	}
	if err := event.Validate(); err != nil {
		t.Fatalf("valid source event rejected: %v", err)
	}
	noScope := event
	noScope.GraphSpaceID = ""
	if err := noScope.Validate(); err == nil {
		t.Fatal("graph space scope is mandatory on a source event")
	}
	noKey := event
	noKey.IdempotencyKey = ""
	if err := noKey.Validate(); err == nil {
		t.Fatal("an event without an idempotency key cannot be replay-safe")
	}

	chunk := Chunk{WorkspaceID: "ws", GraphSpaceID: "gs", EpisodeID: "ep", CharStart: 10, CharEnd: 5}
	if err := chunk.Validate(); err == nil {
		t.Fatal("inverted chunk offsets must be rejected")
	}
}

func TestOutboxEventValidation(t *testing.T) {
	ev := OutboxEvent{
		WorkspaceID: "ws", Topic: TopicIngestPipeline, EventType: EventTypeSourceEventAccepted,
		SchemaVersion: OutboxSchemaVersion, DedupeKey: "k", Status: OutboxPending, MaxAttempts: 5,
	}
	if err := ev.Validate(); err != nil {
		t.Fatalf("valid outbox event rejected: %v", err)
	}
	noDedupe := ev
	noDedupe.DedupeKey = ""
	if err := noDedupe.Validate(); err == nil {
		t.Fatal("an outbox event without a dedupe key cannot be idempotent")
	}
	unversioned := ev
	unversioned.SchemaVersion = 0
	if err := unversioned.Validate(); err == nil {
		t.Fatal("durable event payloads must be versioned")
	}
}

func TestPipelineDedupeKeyIsStableAcrossReplays(t *testing.T) {
	src := SourceID("11111111-1111-7111-8111-111111111111")

	// A replay of the same submission must map to the same work item, so redelivery
	// cannot enqueue a second copy of work that is already queued.
	if PipelineDedupeKey(src, "key-1", 1) != PipelineDedupeKey(src, "key-1", 1) {
		t.Fatal("dedupe keys must be stable for identical submissions")
	}
	if PipelineDedupeKey(src, "key-1", 1) == PipelineDedupeKey(src, "key-2", 1) {
		t.Fatal("distinct submissions must map to distinct work items")
	}
	// A new pipeline version must be able to reprocess the same event.
	if PipelineDedupeKey(src, "key-1", 1) == PipelineDedupeKey(src, "key-1", 2) {
		t.Fatal("a new pipeline version must produce a new work item")
	}
}

func TestStageKeyString(t *testing.T) {
	k := StageKey{WorkspaceID: "ws", SourceEventID: "ev", PipelineVersion: 1, StageName: "chunk", StageVersion: 2}
	if got, want := k.String(), "ws/ev/p1/chunk/v2"; got != want {
		t.Fatalf("StageKey.String() = %q, want %q", got, want)
	}
}

func TestIDHelpers(t *testing.T) {
	id := NewSourceEventID()
	if IsZero(id) {
		t.Fatal("a generated id must not be zero")
	}
	if !ValidUUID(id) {
		t.Fatalf("generated id %q must be a valid UUID", id)
	}
	if ValidUUID(SourceEventID("not-a-uuid")) {
		t.Fatal("malformed identifiers must be rejected")
	}
	// UUIDv7 is time-sortable, which the ledger's index locality depends on.
	first, second := NewSourceEventID(), NewSourceEventID()
	if first >= second {
		t.Fatalf("UUIDv7 ids must sort by creation order: %q >= %q", first, second)
	}
}

func FuzzDeriveKeyDeterminism(f *testing.F) {
	f.Add("ns", "a", "b")
	f.Fuzz(func(t *testing.T, ns, a, b string) {
		if DeriveKey(ns, a, b) != DeriveKey(ns, a, b) {
			t.Fatal("DeriveKey must be deterministic for identical inputs")
		}
		if len(DeriveKey(ns, a, b)) != 64 {
			t.Fatal("DeriveKey must always produce a 64-char digest")
		}
	})
}
