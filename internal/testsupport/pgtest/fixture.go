package pgtest

import (
	"context"
	"fmt"
	"testing"

	"github.com/gimantha/strata/internal/domain"
	"github.com/gimantha/strata/internal/store/ledger"
)

// Fixture is a migrated database with one or more tenants already set up.
type Fixture struct {
	Store *ledger.Store
	// Primary is the tenant most tests use.
	Primary Tenant
}

// Tenant is a workspace with a graph space, a source, and an owning principal. Tests
// use two of these to prove isolation.
type Tenant struct {
	Workspace  domain.Workspace
	GraphSpace domain.GraphSpace
	Source     domain.Source
	Principal  domain.Principal
}

// Scope is the resolved scope for this tenant's graph space.
// Slug names the tenant, for test messages that need to say which one leaked.
func (t Tenant) Slug() string { return t.Workspace.Slug }

func (t Tenant) Scope() domain.Scope {
	return domain.Scope{WorkspaceID: t.Workspace.ID, GraphSpaceID: t.GraphSpace.ID}
}

// NewFixture builds a database with one tenant.
func NewFixture(t *testing.T) *Fixture {
	t.Helper()

	f := &Fixture{Store: Store(t)}
	f.Primary = f.NewTenant(t, "acme")
	return f
}

// NewTenant adds an isolated tenant sharing the same database, which is what makes
// cross-workspace isolation testable rather than assumed.
func (f *Fixture) NewTenant(t *testing.T, slug string) Tenant {
	t.Helper()
	ctx := context.Background()

	principal := domain.Principal{
		ID:          domain.PrincipalID(slug + "-owner"),
		Kind:        domain.PrincipalUser,
		DisplayName: slug + " owner",
		SystemRole:  domain.RoleAdmin,
	}
	if err := f.Store.UpsertPrincipal(ctx, principal); err != nil {
		t.Fatalf("upsert principal: %v", err)
	}

	ws, err := f.Store.CreateWorkspace(ctx, domain.Workspace{Slug: slug, Name: slug}, principal.ID)
	if err != nil {
		t.Fatalf("create workspace %s: %v", slug, err)
	}
	gs, err := f.Store.CreateGraphSpace(ctx, domain.GraphSpace{
		WorkspaceID: ws.ID, Slug: "main", Name: "Main",
	}, principal.ID)
	if err != nil {
		t.Fatalf("create graph space: %v", err)
	}
	src, err := f.Store.CreateSource(ctx, domain.Source{
		WorkspaceID: ws.ID,
		Kind:        domain.SourceKindChat,
		Name:        "test-source",
		TrustLevel:  domain.TrustStandard,
	}, principal.ID)
	if err != nil {
		t.Fatalf("create source: %v", err)
	}

	grants, err := f.Store.GrantsFor(ctx, principal.ID)
	if err != nil {
		t.Fatalf("read grants: %v", err)
	}
	principal.Grants = grants

	return Tenant{Workspace: ws, GraphSpace: gs, Source: src, Principal: principal}
}

// NewGraphSpace adds a second graph space inside the primary tenant's workspace.
//
// Two spaces in one workspace is the arrangement anything per-space is tested through:
// ontology mode, for instance, is set per graph space precisely so the same source can be
// processed both ways and compared, and that comparison needs the workspace held constant.
func (f *Fixture) NewGraphSpace(t *testing.T, slug string) Tenant {
	t.Helper()

	gs, err := f.Store.CreateGraphSpace(context.Background(), domain.GraphSpace{
		WorkspaceID: f.Primary.Workspace.ID, Slug: slug, Name: slug,
	}, f.Primary.Principal.ID)
	if err != nil {
		t.Fatalf("create graph space %s: %v", slug, err)
	}

	tenant := f.Primary
	tenant.GraphSpace = gs
	return tenant
}

// NewEntities creates n entities in the primary tenant and returns their identifiers.
//
// Exists for the graph conformance suite, which builds edges between things: a graph index
// writes edges and cannot create the entities they connect, and graph_edges has foreign keys
// to both entities and assertions. A backend without referential integrity can invent
// identifiers; PostgreSQL needs rows.
func (f *Fixture) NewEntities(tb testing.TB, n int) []domain.EntityID {
	tb.Helper()
	ctx := context.Background()

	out := make([]domain.EntityID, 0, n)
	for i := range n {
		entity, err := f.Store.CreateEntity(ctx, domain.Entity{
			WorkspaceID:   f.Primary.Workspace.ID,
			GraphSpaceID:  f.Primary.GraphSpace.ID,
			CanonicalName: fmt.Sprintf("Fixture Entity %d", i),
			EntityType:    "organization",
		})
		if err != nil {
			tb.Fatalf("create entity %d: %v", i, err)
		}
		out = append(out, entity.ID)
	}
	return out
}

// NewAssertions creates n minimal assertions and returns their identifiers.
//
// Written with SQL rather than through the knowledge service on purpose. The service is the
// right path for testing what an assertion means; here an assertion is only the thing a
// graph edge cites, and dragging ingestion, extraction and reconciliation into a graph
// fixture would make a traversal test fail for reasons that have nothing to do with
// traversal.
func (f *Fixture) NewAssertions(tb testing.TB, n int) []domain.AssertionID {
	tb.Helper()
	ctx := context.Background()

	artifact := domain.NewUUIDString()
	if _, err := f.Store.Pool().Exec(ctx, `
		INSERT INTO artifacts (id, workspace_id, content_hash, media_type, size_bytes,
		                       blob_key, storage)
		VALUES ($1, $2, 'fixture', 'text/plain', 0, 'fixture', 'fs')`,
		artifact, f.Primary.Workspace.ID); err != nil {
		tb.Fatalf("create fixture artifact: %v", err)
	}

	event := domain.NewUUIDString()
	if _, err := f.Store.Pool().Exec(ctx, `
		INSERT INTO source_events (id, workspace_id, graph_space_id, source_id, operation,
		                           content_hash, idempotency_key, observed_at, recorded_at,
		                           raw_artifact_id, status, classification, media_type)
		VALUES ($1, $2, $3, $4, 'upsert', 'fixture', $6, now(), now(), $5, 'processed',
		        'internal', 'text/plain')`,
		event, f.Primary.Workspace.ID, f.Primary.GraphSpace.ID, f.Primary.Source.ID,
		artifact, event); err != nil {
		tb.Fatalf("create fixture source event: %v", err)
	}

	predicate := domain.NewUUIDString()
	if _, err := f.Store.Pool().Exec(ctx, `
		INSERT INTO predicates (id, workspace_id, name, version, temporal_policy,
		                        conflict_policy, default_memory_kind, sensitivity, status)
		VALUES ($1, $2, 'FIXTURE_LINK', 1, 'point_in_time', 'coexist', 'semantic',
		        'internal', 'active')`,
		predicate, f.Primary.Workspace.ID); err != nil {
		tb.Fatalf("create fixture predicate: %v", err)
	}

	subject := f.NewEntities(tb, 1)[0]
	out := make([]domain.AssertionID, 0, n)
	for range n {
		id := domain.NewUUIDString()
		if _, err := f.Store.Pool().Exec(ctx, `
			INSERT INTO assertions (id, workspace_id, graph_space_id, subject_id,
			                        predicate_id, predicate_name, predicate_version,
			                        object_kind, object_text, object_key, memory_kind,
			                        observed_at, recorded_at, status, provenance_mode,
			                        source_event_id, fingerprint, classification)
			VALUES ($1, $2, $3, $4, $5, 'FIXTURE_LINK', 1, 'string', 'fixture', $7,
			        'semantic', now(), now(), 'active', 'user_asserted', $6, $7,
			        'internal')`,
			id, f.Primary.Workspace.ID, f.Primary.GraphSpace.ID, subject, predicate,
			event, id); err != nil {
			tb.Fatalf("create fixture assertion: %v", err)
		}
		out = append(out, domain.AssertionID(id))
	}
	return out
}
