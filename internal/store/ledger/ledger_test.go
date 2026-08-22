package ledger_test

import (
	"context"
	"testing"
	"testing/fstest"

	"github.com/gimantha/strata/internal/domain"
	"github.com/gimantha/strata/internal/store/ledger"
	"github.com/gimantha/strata/internal/testsupport/pgtest"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

func TestMigrationsApplyToHead(t *testing.T) {
	store := pgtest.Store(t)
	ctx := context.Background()

	head, err := ledger.EmbeddedHead()
	if err != nil {
		t.Fatalf("read embedded head: %v", err)
	}
	if head < 4 {
		t.Fatalf("expected at least 4 migrations, got head %d", head)
	}

	version, err := store.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("read schema version: %v", err)
	}
	if version != head {
		t.Fatalf("schema version %d does not match embedded head %d", version, head)
	}

	// Migrating an up-to-date database must be a no-op, not a re-application.
	applied, err := store.Migrate(ctx, nil)
	if err != nil {
		t.Fatalf("re-migrate: %v", err)
	}
	if applied != 0 {
		t.Fatalf("expected 0 migrations applied on a current database, got %d", applied)
	}
}

func TestMigrateDetectsEditedMigration(t *testing.T) {
	store := pgtest.Store(t)
	ctx := context.Background()

	// Simulate someone editing an already-released migration file.
	if _, err := store.Pool().Exec(ctx,
		`UPDATE schema_migrations SET checksum = 'tampered' WHERE version = 1`); err != nil {
		t.Fatalf("tamper with checksum: %v", err)
	}

	if _, err := store.Migrate(ctx, nil); err == nil {
		t.Fatal("editing an applied migration must be an error, not a silent divergence")
	}
}

func TestLoadMigrationsRejectsBadNames(t *testing.T) {
	if _, err := ledger.LoadMigrations(mapFS("nope.sql", "select 1;")); err == nil {
		t.Fatal("a migration without a version prefix must be rejected")
	}
	if _, err := ledger.LoadMigrations(mapFS("0001_a.sql", "select 1;", "1_b.sql", "select 2;")); err == nil {
		t.Fatal("duplicate versions must be rejected")
	}
	ms, err := ledger.LoadMigrations(mapFS("0002_b.sql", "select 2;", "0001_a.sql", "select 1;"))
	if err != nil {
		t.Fatalf("valid migrations rejected: %v", err)
	}
	if len(ms) != 2 || ms[0].Version != 1 || ms[1].Version != 2 {
		t.Fatalf("migrations must be ordered by version, got %+v", ms)
	}
	if ms[0].Checksum == ms[1].Checksum {
		t.Fatal("different content must produce different checksums")
	}
}

func TestScopeHierarchyRoundTrip(t *testing.T) {
	store := pgtest.Store(t)
	ctx := context.Background()

	ws, err := store.CreateWorkspace(ctx, domain.Workspace{Slug: "acme", Name: "Acme"}, "")
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	if domain.IsZero(ws.ID) || ws.CreatedAt.IsZero() {
		t.Fatalf("workspace not populated: %+v", ws)
	}

	got, err := store.GetWorkspace(ctx, ws.ID)
	if err != nil {
		t.Fatalf("get workspace: %v", err)
	}
	if got.Slug != "acme" {
		t.Fatalf("round trip lost data: %+v", got)
	}

	if _, err := store.CreateWorkspace(ctx, domain.Workspace{Slug: "acme", Name: "Dup"}, ""); err == nil {
		t.Fatal("duplicate workspace slug must conflict")
	} else if !domain.IsCode(err, domain.CodeConflict) {
		t.Fatalf("expected conflict, got %s", domain.CodeOf(err))
	}

	gs, err := store.CreateGraphSpace(ctx, domain.GraphSpace{
		WorkspaceID: ws.ID, Slug: "main", Name: "Main",
	}, "")
	if err != nil {
		t.Fatalf("create graph space: %v", err)
	}

	// The same slug must be reusable in a different workspace: slugs are per tenant.
	other, err := store.CreateWorkspace(ctx, domain.Workspace{Slug: "globex", Name: "Globex"}, "")
	if err != nil {
		t.Fatalf("create second workspace: %v", err)
	}
	if _, err := store.CreateGraphSpace(ctx, domain.GraphSpace{
		WorkspaceID: other.ID, Slug: "main", Name: "Main",
	}, ""); err != nil {
		t.Fatalf("identical slugs in separate workspaces must be allowed: %v", err)
	}

	resolved, err := store.GetGraphSpace(ctx, gs.ID)
	if err != nil {
		t.Fatalf("get graph space: %v", err)
	}
	if resolved.WorkspaceID != ws.ID {
		t.Fatal("a graph space must resolve to its owning workspace")
	}

	if _, err := store.GetGraphSpace(ctx, domain.NewGraphSpaceID()); !domain.IsCode(err, domain.CodeGraphSpaceNotFound) {
		t.Fatalf("expected graph_space_not_found, got %s", domain.CodeOf(err))
	}
	if _, err := store.GetWorkspace(ctx, domain.NewWorkspaceID()); !domain.IsCode(err, domain.CodeWorkspaceNotFound) {
		t.Fatalf("expected workspace_not_found, got %s", domain.CodeOf(err))
	}
}

func TestCreateWorkspaceGrantsCreatorOwnership(t *testing.T) {
	store := pgtest.Store(t)
	ctx := context.Background()

	if err := store.UpsertPrincipal(ctx, domain.Principal{
		ID: "alice", Kind: domain.PrincipalUser, DisplayName: "Alice", SystemRole: domain.RoleAdmin,
	}); err != nil {
		t.Fatalf("upsert principal: %v", err)
	}

	ws, err := store.CreateWorkspace(ctx, domain.Workspace{Slug: "acme", Name: "Acme"}, "alice")
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}

	role, ok, err := store.GrantFor(ctx, "alice", ws.ID)
	if err != nil {
		t.Fatalf("read grant: %v", err)
	}
	if !ok || role != domain.RoleOwner {
		t.Fatalf("creator must own the workspace, got role %q (ok=%v)", role, ok)
	}

	// A workspace nobody may administer would be unusable and unauditable.
	list, err := store.ListWorkspaces(ctx, "alice")
	if err != nil {
		t.Fatalf("list workspaces: %v", err)
	}
	if len(list) != 1 || list[0].ID != ws.ID {
		t.Fatalf("expected exactly the granted workspace, got %+v", list)
	}

	// A principal with no grant sees nothing, even though the workspace exists.
	if err := store.UpsertPrincipal(ctx, domain.Principal{
		ID: "bob", Kind: domain.PrincipalUser, DisplayName: "Bob", SystemRole: domain.RoleReader,
	}); err != nil {
		t.Fatalf("upsert principal: %v", err)
	}
	list, err = store.ListWorkspaces(ctx, "bob")
	if err != nil {
		t.Fatalf("list workspaces: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("a principal without a grant must see no workspaces, got %+v", list)
	}
}

func TestSourceRegistrationDefaultsAreConservative(t *testing.T) {
	store := pgtest.Store(t)
	ctx := context.Background()

	ws, err := store.CreateWorkspace(ctx, domain.Workspace{Slug: "acme", Name: "Acme"}, "")
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}

	src, err := store.CreateSource(ctx, domain.Source{
		WorkspaceID: ws.ID, Kind: domain.SourceKindChat, Name: "support-chat",
	}, "")
	if err != nil {
		t.Fatalf("create source: %v", err)
	}
	if src.TrustLevel != domain.TrustStandard {
		t.Fatalf("unspecified trust must not become high trust, got %q", src.TrustLevel)
	}
	if src.Classification != domain.ClassificationInternal {
		t.Fatalf("unspecified classification must not become public, got %q", src.Classification)
	}

	byName, err := store.GetSourceByName(ctx, ws.ID, "support-chat")
	if err != nil {
		t.Fatalf("get source by name: %v", err)
	}
	if byName.ID != src.ID {
		t.Fatal("source lookup by name returned the wrong row")
	}

	// Cross-workspace dereference of a valid identifier must fail.
	other, err := store.CreateWorkspace(ctx, domain.Workspace{Slug: "globex", Name: "Globex"}, "")
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	if _, err := store.GetSource(ctx, other.ID, src.ID); !domain.IsCode(err, domain.CodeNotFound) {
		t.Fatalf("a source must not be readable from another workspace, got %s", domain.CodeOf(err))
	}
}

func TestAdminMutationsAreAudited(t *testing.T) {
	store := pgtest.Store(t)
	ctx := context.Background()

	if err := store.UpsertPrincipal(ctx, domain.Principal{
		ID: "alice", Kind: domain.PrincipalUser, DisplayName: "Alice", SystemRole: domain.RoleAdmin,
	}); err != nil {
		t.Fatalf("upsert principal: %v", err)
	}
	ws, err := store.CreateWorkspace(ctx, domain.Workspace{Slug: "acme", Name: "Acme"}, "alice")
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	if _, err := store.CreateSource(ctx, domain.Source{
		WorkspaceID: ws.ID, Kind: domain.SourceKindFile, Name: "uploads",
	}, "alice"); err != nil {
		t.Fatalf("create source: %v", err)
	}

	for _, action := range []string{"workspace.create", "source.create"} {
		n, err := store.CountAuditEvents(ctx, ws.ID, action)
		if err != nil {
			t.Fatalf("count audit events: %v", err)
		}
		if n != 1 {
			t.Fatalf("expected 1 audit row for %s, got %d", action, n)
		}
	}
}

// mapFS builds an in-memory migration filesystem from name/content pairs.
func mapFS(pairs ...string) fstest.MapFS {
	fsys := fstest.MapFS{}
	for i := 0; i+1 < len(pairs); i += 2 {
		fsys[pairs[i]] = &fstest.MapFile{Data: []byte(pairs[i+1])}
	}
	return fsys
}
