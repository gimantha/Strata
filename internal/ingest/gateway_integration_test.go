package ingest_test

import (
	"context"
	"testing"
	"time"

	"github.com/gimantha/strata/internal/domain"
	"github.com/gimantha/strata/internal/ingest"
	"github.com/gimantha/strata/internal/store/blob"
	"github.com/gimantha/strata/internal/testsupport/pgtest"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

// newGateway builds a gateway over a real database and a temporary blob store.
func newGateway(t *testing.T, f *pgtest.Fixture) *ingest.Gateway {
	t.Helper()

	blobs, err := blob.NewFS(t.TempDir())
	if err != nil {
		t.Fatalf("create blob store: %v", err)
	}
	return ingest.New(f.Store, blobs, ingest.Options{PipelineVersion: 1, MaxAttempts: 5}, nil, nil, nil)
}

func baseRequest(tenant pgtest.Tenant, content string) ingest.Request {
	return ingest.Request{
		Scope:     tenant.Scope(),
		Principal: tenant.Principal.Ref(),
		SourceID:  tenant.Source.ID,
		EventType: "chat.turn",
		MediaType: "text/plain",
		Payload:   []byte(content),
		Operation: domain.SourceOpUpsert,
	}
}

func TestIntegrationSameIdempotencyKeyCannotDuplicateAnEvent(t *testing.T) {
	f := pgtest.NewFixture(t)
	gateway := newGateway(t, f)
	ctx := context.Background()

	req := baseRequest(f.Primary, "Alice signed the contract on March 3rd.")
	req.IdempotencyKey = "caller-key-1"

	first, err := gateway.Accept(ctx, req)
	if err != nil {
		t.Fatalf("first accept: %v", err)
	}
	if first.Duplicate {
		t.Fatal("the first submission must not be reported as a duplicate")
	}

	second, err := gateway.Accept(ctx, req)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !second.Duplicate {
		t.Fatal("a replayed submission must be recognized, not ingested again")
	}
	if second.SourceEventID != first.SourceEventID {
		t.Fatalf("replay returned a different event: %s then %s", first.SourceEventID, second.SourceEventID)
	}

	assertCount(t, f, "source_events", 1)
	assertCount(t, f, "artifacts", 1)
	// One work item, not two: redelivering the same submission must not queue the same
	// processing twice.
	assertCount(t, f, "outbox_events", 1)
	assertCount(t, f, "pipeline_runs", 1)
}

func TestIntegrationDerivedIdempotencyKeyDeduplicatesIdenticalContent(t *testing.T) {
	f := pgtest.NewFixture(t)
	gateway := newGateway(t, f)
	ctx := context.Background()

	// No caller-supplied key: the derived key must still recognize a resend of the
	// same upstream record with the same content.
	req := baseRequest(f.Primary, "Acme is on the enterprise tier.")
	req.ExternalID = "customer-42"
	req.SourceVersion = "v7"

	first, err := gateway.Accept(ctx, req)
	if err != nil {
		t.Fatalf("first accept: %v", err)
	}
	second, err := gateway.Accept(ctx, req)
	if err != nil {
		t.Fatalf("resend: %v", err)
	}
	if !second.Duplicate || second.SourceEventID != first.SourceEventID {
		t.Fatal("an identical resend must deduplicate without a caller-supplied key")
	}
	assertCount(t, f, "source_events", 1)
}

func TestIntegrationSameKeyDifferentContentIsAConflict(t *testing.T) {
	f := pgtest.NewFixture(t)
	gateway := newGateway(t, f)
	ctx := context.Background()

	req := baseRequest(f.Primary, "original content")
	req.IdempotencyKey = "caller-key-1"
	if _, err := gateway.Accept(ctx, req); err != nil {
		t.Fatalf("first accept: %v", err)
	}

	changed := req
	changed.Payload = []byte("different content")
	_, err := gateway.Accept(ctx, changed)
	if err == nil {
		t.Fatal("reusing a key for different content must not silently discard either payload")
	}
	if !domain.IsCode(err, domain.CodeSourceEventConflict) {
		t.Fatalf("expected source_event_conflict, got %s: %v", domain.CodeOf(err), err)
	}
	assertCount(t, f, "source_events", 1)
}

func TestIntegrationSameUpstreamVersionWithChangedContentIsAConflict(t *testing.T) {
	f := pgtest.NewFixture(t)
	gateway := newGateway(t, f)
	ctx := context.Background()

	req := baseRequest(f.Primary, "row at version 7")
	req.ExternalID = "customer-42"
	req.SourceVersion = "v7"
	if _, err := gateway.Accept(ctx, req); err != nil {
		t.Fatalf("first accept: %v", err)
	}

	// A source that reuses a version number for changed data is a real upstream fault,
	// and silently accepting it would make version-based temporal queries wrong.
	changed := req
	changed.Payload = []byte("row at version 7, but different")
	if _, err := gateway.Accept(ctx, changed); !domain.IsCode(err, domain.CodeSourceEventConflict) {
		t.Fatalf("expected source_event_conflict, got %s: %v", domain.CodeOf(err), err)
	}
}

func TestIntegrationUpdatesWithoutVersionsAppendNewEvents(t *testing.T) {
	f := pgtest.NewFixture(t)
	gateway := newGateway(t, f)
	ctx := context.Background()

	// A CDC stream reporting successive updates to one row, with no version numbers,
	// must be able to append: an update is new knowledge, not a duplicate
	// (AGENTS.md section 11.2).
	for i, content := range []string{"tier=standard", "tier=enterprise", "tier=strategic"} {
		req := baseRequest(f.Primary, content)
		req.ExternalID = "customer-42"
		receipt, err := gateway.Accept(ctx, req)
		if err != nil {
			t.Fatalf("update %d: %v", i, err)
		}
		if receipt.Duplicate {
			t.Fatalf("update %d was treated as a duplicate; upstream changes must append", i)
		}
	}
	assertCount(t, f, "source_events", 3)
}

func TestIntegrationTransactionAtomicity(t *testing.T) {
	f := pgtest.NewFixture(t)
	ctx := context.Background()

	// Force the outbox insert to fail after the source event has been written, by
	// pointing the work item at a graph space that does not exist. If the commit were
	// not atomic, the event would survive with no queued work: accepted knowledge that
	// is never processed, which is exactly the failure the outbox pattern prevents.
	artifact := domain.Artifact{
		WorkspaceID: f.Primary.Workspace.ID,
		ContentHash: domain.ContentHash([]byte("payload")),
		MediaType:   "text/plain",
		SizeBytes:   7,
		BlobKey:     "key",
		Storage:     "test",
	}
	event := domain.SourceEvent{
		WorkspaceID:    f.Primary.Workspace.ID,
		GraphSpaceID:   f.Primary.GraphSpace.ID,
		SourceID:       f.Primary.Source.ID,
		Operation:      domain.SourceOpUpsert,
		ContentHash:    artifact.ContentHash,
		IdempotencyKey: "atomicity-key",
		Status:         domain.SourceEventAccepted,
		Classification: domain.ClassificationInternal,
		ObservedAt:     nowUTC(),
		RecordedAt:     nowUTC(),
	}
	work := domain.OutboxEvent{
		WorkspaceID:   f.Primary.Workspace.ID,
		GraphSpaceID:  domain.NewGraphSpaceID(), // does not exist
		Topic:         domain.TopicIngestPipeline,
		EventType:     domain.EventTypeSourceEventAccepted,
		SchemaVersion: domain.OutboxSchemaVersion,
		DedupeKey:     "atomicity-dedupe",
		Status:        domain.OutboxPending,
		MaxAttempts:   5,
	}

	if _, err := f.Store.AppendSourceEvent(ctx, domain.SourceEventAppend{
		Artifact: artifact, Event: event, Outbox: []domain.OutboxEvent{work}, PipelineVersion: 1,
	}); err == nil {
		t.Fatal("expected the append to fail on the invalid work item")
	}

	assertCount(t, f, "source_events", 0)
	assertCount(t, f, "outbox_events", 0)
	assertCount(t, f, "pipeline_runs", 0)
	// The audit row is written in the same transaction and must roll back with it.
	if n, err := f.Store.CountAuditEvents(ctx, f.Primary.Workspace.ID, "ingest.accept"); err != nil {
		t.Fatalf("count audit events: %v", err)
	} else if n != 0 {
		t.Fatalf("a rolled-back ingest must leave no audit record, found %d", n)
	}
}

func TestIntegrationBatchUsesTheSameSourceEventPath(t *testing.T) {
	f := pgtest.NewFixture(t)
	gateway := newGateway(t, f)
	ctx := context.Background()

	contents := []string{"first turn", "second turn", "third turn"}

	// Submit as a batch into one tenant.
	batch := make([]ingest.Request, 0, len(contents))
	for i, content := range contents {
		req := baseRequest(f.Primary, content)
		req.IdempotencyKey = "batch-" + string(rune('a'+i))
		batch = append(batch, req)
	}
	receipts, errs := gateway.AcceptBatch(ctx, batch)
	for i, err := range errs {
		if err != nil {
			t.Fatalf("batch item %d failed: %v", i, err)
		}
	}

	// Submit the identical payloads one at a time into a second tenant.
	other := f.NewTenant(t, "globex")
	for i, content := range contents {
		req := baseRequest(other, content)
		req.IdempotencyKey = "batch-" + string(rune('a'+i))
		if _, err := gateway.Accept(ctx, req); err != nil {
			t.Fatalf("single item %d failed: %v", i, err)
		}
	}

	// The canonical rows must be indistinguishable apart from tenancy and identifiers.
	batchRows := canonicalRows(t, f, f.Primary.Workspace.ID)
	singleRows := canonicalRows(t, f, other.Workspace.ID)
	if len(batchRows) != len(contents) || len(singleRows) != len(contents) {
		t.Fatalf("expected %d rows each, got %d and %d", len(contents), len(batchRows), len(singleRows))
	}
	for i := range batchRows {
		if batchRows[i] != singleRows[i] {
			t.Fatalf("batch and single-event ingestion diverged:\n batch:  %s\n single: %s",
				batchRows[i], singleRows[i])
		}
	}
	if len(receipts) != len(contents) {
		t.Fatalf("expected %d receipts, got %d", len(contents), len(receipts))
	}
}

func TestIntegrationBatchItemFailureDoesNotDiscardTheRest(t *testing.T) {
	f := pgtest.NewFixture(t)
	gateway := newGateway(t, f)
	ctx := context.Background()

	good := baseRequest(f.Primary, "valid content")
	good.IdempotencyKey = "good-1"
	bad := baseRequest(f.Primary, "")
	bad.IdempotencyKey = "bad-1"
	alsoGood := baseRequest(f.Primary, "more valid content")
	alsoGood.IdempotencyKey = "good-2"

	receipts, errs := gateway.AcceptBatch(ctx, []ingest.Request{good, bad, alsoGood})

	if errs[0] != nil || errs[2] != nil {
		t.Fatalf("valid items must succeed independently: %v, %v", errs[0], errs[2])
	}
	if errs[1] == nil {
		t.Fatal("the empty payload must be rejected")
	}
	if domain.IsZero(receipts[0].SourceEventID) || domain.IsZero(receipts[2].SourceEventID) {
		t.Fatal("valid items must be durably accepted")
	}
	assertCount(t, f, "source_events", 2)
}

func TestIntegrationWorkspaceIsolation(t *testing.T) {
	f := pgtest.NewFixture(t)
	gateway := newGateway(t, f)
	ctx := context.Background()

	other := f.NewTenant(t, "globex")

	// Identical names exist in both tenants; a source from one must be unusable in the
	// other even with a valid identifier (AGENTS.md scenario F).
	crossTenant := baseRequest(f.Primary, "content")
	crossTenant.SourceID = other.Source.ID
	if _, err := gateway.Accept(ctx, crossTenant); err == nil {
		t.Fatal("a source from another workspace must not be usable")
	} else if !domain.IsCode(err, domain.CodeNotFound) {
		t.Fatalf("expected not_found, got %s", domain.CodeOf(err))
	}

	// The same idempotency key in two tenants is two distinct events, not a collision.
	a := baseRequest(f.Primary, "tenant a content")
	a.IdempotencyKey = "shared-key"
	b := baseRequest(other, "tenant b content")
	b.IdempotencyKey = "shared-key"

	ra, err := gateway.Accept(ctx, a)
	if err != nil {
		t.Fatalf("tenant a: %v", err)
	}
	rb, err := gateway.Accept(ctx, b)
	if err != nil {
		t.Fatalf("tenant b: %v", err)
	}
	if rb.Duplicate || ra.SourceEventID == rb.SourceEventID {
		t.Fatal("idempotency keys must be scoped per tenant")
	}

	// Neither event may be read from the other tenant's workspace.
	if _, err := f.Store.GetSourceEvent(ctx, other.Workspace.ID, ra.SourceEventID); !domain.IsCode(err, domain.CodeNotFound) {
		t.Fatalf("cross-workspace read must fail, got %s", domain.CodeOf(err))
	}
}

func TestIntegrationClassificationIsInheritedAndNeverDowngraded(t *testing.T) {
	f := pgtest.NewFixture(t)
	gateway := newGateway(t, f)
	ctx := context.Background()

	confidential, err := f.Store.CreateSource(ctx, domain.Source{
		WorkspaceID:    f.Primary.Workspace.ID,
		Kind:           domain.SourceKindDatabase,
		Name:           "hr-records",
		TrustLevel:     domain.TrustHigh,
		Classification: domain.ClassificationConfidential,
	}, f.Primary.Principal.ID)
	if err != nil {
		t.Fatalf("create source: %v", err)
	}

	// A request asking for a weaker label must not lower the source's classification.
	req := baseRequest(f.Primary, "salary band information")
	req.SourceID = confidential.ID
	req.Classification = domain.ClassificationPublic
	req.IdempotencyKey = "hr-1"

	receipt, err := gateway.Accept(ctx, req)
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	stored, err := f.Store.GetSourceEvent(ctx, f.Primary.Workspace.ID, receipt.SourceEventID)
	if err != nil {
		t.Fatalf("load event: %v", err)
	}
	if stored.Classification != domain.ClassificationConfidential {
		t.Fatalf("classification was downgraded to %q by a request", stored.Classification)
	}

	// Raising it is allowed.
	raised := req
	raised.Classification = domain.ClassificationRestricted
	raised.IdempotencyKey = "hr-2"
	receipt, err = gateway.Accept(ctx, raised)
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	stored, err = f.Store.GetSourceEvent(ctx, f.Primary.Workspace.ID, receipt.SourceEventID)
	if err != nil {
		t.Fatalf("load event: %v", err)
	}
	if stored.Classification != domain.ClassificationRestricted {
		t.Fatalf("a request must be able to raise sensitivity, got %q", stored.Classification)
	}
}

func TestIntegrationRejectsUnscopedAndOversizedSubmissions(t *testing.T) {
	f := pgtest.NewFixture(t)
	ctx := context.Background()

	blobs, err := blob.NewFS(t.TempDir())
	if err != nil {
		t.Fatalf("create blob store: %v", err)
	}
	gateway := ingest.New(f.Store, blobs, ingest.Options{PipelineVersion: 1, MaxPayloadBytes: 32}, nil, nil, nil)

	oversized := baseRequest(f.Primary, "this payload is definitely longer than thirty-two bytes")
	if _, err := gateway.Accept(ctx, oversized); !domain.IsCode(err, domain.CodeInvalidArgument) {
		t.Fatalf("expected invalid_argument for an oversized payload, got %s", domain.CodeOf(err))
	}

	// A gateway must never be reachable without resolved scope: that would mean
	// authorization was skipped.
	unscoped := baseRequest(f.Primary, "hi")
	unscoped.Scope = domain.Scope{}
	if _, err := gateway.Accept(ctx, unscoped); !domain.IsCode(err, domain.CodeInternal) {
		t.Fatalf("expected an internal error for unresolved scope, got %s", domain.CodeOf(err))
	}

	noSource := baseRequest(f.Primary, "hi")
	noSource.SourceID = ""
	if _, err := gateway.Accept(ctx, noSource); !domain.IsCode(err, domain.CodeInvalidArgument) {
		t.Fatalf("expected invalid_argument without a source, got %s", domain.CodeOf(err))
	}

	assertCount(t, f, "source_events", 0)
}

func TestIntegrationRawPayloadIsArchivedAndRetrievable(t *testing.T) {
	f := pgtest.NewFixture(t)
	ctx := context.Background()

	dir := t.TempDir()
	blobs, err := blob.NewFS(dir)
	if err != nil {
		t.Fatalf("create blob store: %v", err)
	}
	gateway := ingest.New(f.Store, blobs, ingest.Options{PipelineVersion: 1}, nil, nil, nil)

	payload := "Alice Chen signed for Acme on March 3rd."
	req := baseRequest(f.Primary, payload)
	receipt, err := gateway.Accept(ctx, req)
	if err != nil {
		t.Fatalf("accept: %v", err)
	}

	// Assertion-to-source traceability starts here: the archived bytes must be
	// byte-identical to what was submitted (AGENTS.md section 2.2).
	artifact, err := f.Store.GetArtifact(ctx, f.Primary.Workspace.ID, receipt.ArtifactID)
	if err != nil {
		t.Fatalf("load artifact: %v", err)
	}
	stored, err := blobs.Get(ctx, artifact.BlobKey)
	if err != nil {
		t.Fatalf("read archived payload: %v", err)
	}
	if string(stored) != payload {
		t.Fatalf("archived payload differs from what was submitted: %q", stored)
	}
	if artifact.ContentHash != domain.ContentHash([]byte(payload)) {
		t.Fatal("artifact content hash does not match the archived bytes")
	}
	if artifact.SizeBytes != int64(len(payload)) {
		t.Fatalf("artifact size %d does not match payload size %d", artifact.SizeBytes, len(payload))
	}
}

func TestIntegrationIdenticalContentSharesOneArchivedArtifact(t *testing.T) {
	f := pgtest.NewFixture(t)
	gateway := newGateway(t, f)
	ctx := context.Background()

	// Two distinct events carrying identical bytes must reference one archived blob:
	// deduplication is by content address, not by event.
	first := baseRequest(f.Primary, "identical bytes")
	first.IdempotencyKey = "dedupe-a"
	second := baseRequest(f.Primary, "identical bytes")
	second.IdempotencyKey = "dedupe-b"

	ra, err := gateway.Accept(ctx, first)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	rb, err := gateway.Accept(ctx, second)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if ra.SourceEventID == rb.SourceEventID {
		t.Fatal("distinct idempotency keys must produce distinct events")
	}
	if ra.ArtifactID != rb.ArtifactID {
		t.Fatal("identical content must reuse one archived artifact")
	}
	assertCount(t, f, "source_events", 2)
	assertCount(t, f, "artifacts", 1)
}

// assertCount checks a table's row count.
func assertCount(t *testing.T, f *pgtest.Fixture, table string, want int) {
	t.Helper()

	var got int
	if err := f.Store.Pool().QueryRow(context.Background(), "SELECT count(*) FROM "+table).Scan(&got); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	if got != want {
		t.Fatalf("expected %d row(s) in %s, found %d", want, table, got)
	}
}

// canonicalRows returns the comparable fields of a workspace's source events.
func canonicalRows(t *testing.T, f *pgtest.Fixture, ws domain.WorkspaceID) []string {
	t.Helper()

	rows, err := f.Store.Pool().Query(context.Background(), `
		SELECT event_type || '|' || operation || '|' || content_hash || '|' || media_type ||
		       '|' || status || '|' || classification || '|' || idempotency_key
		FROM source_events WHERE workspace_id = $1 ORDER BY idempotency_key`, ws)
	if err != nil {
		t.Fatalf("read canonical rows: %v", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var row string
		if err := rows.Scan(&row); err != nil {
			t.Fatalf("scan canonical row: %v", err)
		}
		out = append(out, row)
	}
	return out
}

func nowUTC() time.Time { return time.Now().UTC() }
