package pgtest

import (
	"context"
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
