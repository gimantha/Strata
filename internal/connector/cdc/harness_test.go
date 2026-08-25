package cdc_test

import (
	"context"
	"testing"
	"time"

	"github.com/gimantha/strata/internal/connector/cdc"
	"github.com/gimantha/strata/internal/domain"
	"github.com/gimantha/strata/internal/embedding/hashing"
	"github.com/gimantha/strata/internal/ingest"
	"github.com/gimantha/strata/internal/knowledge"
	"github.com/gimantha/strata/internal/pipeline"
	"github.com/gimantha/strata/internal/projection"
	"github.com/gimantha/strata/internal/store/blob"
	"github.com/gimantha/strata/internal/testsupport/pgtest"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

type harness struct {
	fixture *pgtest.Fixture
	runner  *cdc.Runner
	process *pipeline.Runner
	service *knowledge.Service
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	f := pgtest.NewFixture(t)
	blobs, err := blob.NewFS(t.TempDir())
	if err != nil {
		t.Fatalf("blob store: %v", err)
	}

	gateway := ingest.New(f.Store, blobs, ingest.Options{PipelineVersion: 1}, nil, nil, nil)
	service := knowledge.New(f.Store, knowledge.Options{}, nil, nil)
	projector := projection.New(f.Store, f.Store, f.Store.Indexes(), hashing.New(), projection.Options{}, nil, nil)

	stages := pipeline.DefaultStages(f.Store, blobs, pipeline.StageConfig{
		ChunkMaxTokens: 256, ChunkOverlapTokens: 16,
		Changes:   f.Store,
		Committer: service,
		Projector: projector,
	})

	return &harness{
		fixture: f,
		runner:  cdc.New(gateway, f.Store, cdc.Options{CheckpointEvery: 2}, nil, nil),
		process: pipeline.NewRunner(f.Store, 1, stages, nil, nil, nil),
		service: service,
	}
}

func (h *harness) scope() domain.Scope { return h.fixture.Primary.Scope() }

// customerMapping is the reference mapping used throughout: a customers table where the id
// is the identity, the name is the label, and three columns are facts.
func customerMapping() domain.ChangeMapping {
	return domain.ChangeMapping{
		SubjectType:         "organization",
		SubjectNameColumn:   "name",
		IdentifierNamespace: "erp.customer_id",
		Columns: []domain.ColumnMapping{
			{Column: "name", Predicate: "LEGAL_NAME"},
			{Column: "tier", Predicate: "TIER", ObjectKind: domain.ObjectSymbol},
			{Column: "region", Predicate: "REGION", SkipEmpty: true},
			{Column: "credit_limit", Predicate: "CREDIT_LIMIT", ObjectKind: domain.ObjectInteger},
		},
	}
}

func (h *harness) registerStream(t *testing.T, stream string, mapping domain.ChangeMapping) {
	t.Helper()

	if _, err := h.fixture.Store.UpsertCDCStream(context.Background(), domain.CDCStream{
		WorkspaceID: h.scope().WorkspaceID,
		SourceID:    h.fixture.Primary.Source.ID,
		Stream:      stream,
		Mapping:     mapping,
	}, h.fixture.Primary.Principal.ID); err != nil {
		t.Fatalf("register stream: %v", err)
	}
}

// consume runs a change log through the connector and processes every event it accepted.
func (h *harness) consume(t *testing.T, stream string, resume bool, events ...domain.ChangeEvent) cdc.Result {
	t.Helper()
	ctx := context.Background()

	result, err := h.runner.Run(ctx, cdc.Request{
		Scope:                h.scope(),
		Principal:            h.fixture.Primary.Principal.Ref(),
		SourceID:             h.fixture.Primary.Source.ID,
		Stream:               stream,
		ResumeFromCheckpoint: resume,
	}, cdc.NewReplayEvents("test", events))
	if err != nil {
		t.Fatalf("run connector: %v", err)
	}

	for _, id := range result.Events {
		if _, err := h.process.Process(ctx, h.scope().WorkspaceID, id, false); err != nil {
			t.Fatalf("process %s: %v", id, err)
		}
	}
	return result
}

// current returns the workspace's current belief, predicate to rendered object.
//
// It fails on a duplicate rather than letting the map hide it. Two active claims for one
// functional predicate is exactly the bug this package is most likely to reintroduce — an
// unchanged column that fingerprints differently on every update accumulates silently, and a
// map keyed by predicate would show the last one and look fine.
func (h *harness) current(t *testing.T) map[string]string {
	t.Helper()

	assertions, err := h.fixture.Store.QueryAssertions(context.Background(), domain.AssertionQuery{
		Scope:    h.scope(),
		Statuses: []domain.AssertionStatus{domain.AssertionActive},
		Limit:    domain.MaxAssertionLimit,
	})
	if err != nil {
		t.Fatalf("query: %v", err)
	}

	out := map[string]string{}
	for _, assertion := range assertions {
		if existing, clash := out[assertion.Predicate.Name]; clash {
			t.Fatalf("%s has two active claims at once: %q and %q",
				assertion.Predicate.Name, existing, assertion.Object.Display())
		}
		out[assertion.Predicate.Name] = assertion.Object.Display()
	}
	return out
}

// ids returns the assertion id backing each current predicate, which is what "no rebuild"
// is measured against.
func (h *harness) ids(t *testing.T) map[string]domain.AssertionID {
	t.Helper()

	assertions, err := h.fixture.Store.QueryAssertions(context.Background(), domain.AssertionQuery{
		Scope:    h.scope(),
		Statuses: []domain.AssertionStatus{domain.AssertionActive},
		Limit:    domain.MaxAssertionLimit,
	})
	if err != nil {
		t.Fatalf("query: %v", err)
	}

	out := map[string]domain.AssertionID{}
	for _, assertion := range assertions {
		if _, clash := out[assertion.Predicate.Name]; clash {
			t.Fatalf("%s has two active claims at once", assertion.Predicate.Name)
		}
		out[assertion.Predicate.Name] = assertion.ID
	}
	return out
}

func at(t *testing.T, value string) *time.Time {
	t.Helper()

	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatalf("parse %s: %v", value, err)
	}
	return &parsed
}

// row builds one change event for the customers stream.
func row(op domain.ChangeOperation, offset, sequence string, commit *time.Time, image map[string]any) domain.ChangeEvent {
	event := domain.ChangeEvent{
		Stream:     "public.customers",
		Schema:     "public",
		Table:      "customers",
		Operation:  op,
		Key:        map[string]any{"id": 42},
		Offset:     offset,
		Sequence:   sequence,
		CommitTime: commit,
	}
	if op == domain.ChangeDelete {
		event.Before = image
		return event
	}
	event.After = image
	return event
}
