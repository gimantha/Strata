package identity_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/gimantha/strata/internal/domain"
	"github.com/gimantha/strata/internal/identity"
	"github.com/gimantha/strata/internal/testsupport/pgtest"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

// writeKeyFile creates a key file granting one principal the given system role.
func writeKeyFile(t *testing.T, principalID, secret string, role domain.Role) string {
	t.Helper()

	digest := sha256.Sum256([]byte(secret))
	body, err := json.Marshal(map[string]any{
		"version": 1,
		"keys": []map[string]string{{
			"key_id":        "key-" + principalID,
			"secret_sha256": hex.EncodeToString(digest[:]),
			"principal_id":  principalID,
			"kind":          string(domain.PrincipalUser),
			"display_name":  principalID,
			"system_role":   string(role),
		}},
	})
	if err != nil {
		t.Fatalf("encode key file: %v", err)
	}

	path := filepath.Join(t.TempDir(), "api-keys.json")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write key file: %v", err)
	}
	return path
}

func TestLoadMissingFileFailsClosed(t *testing.T) {
	svc, err := identity.Load(context.Background(),
		filepath.Join(t.TempDir(), "absent.json"), nil, nil)
	if err != nil {
		t.Fatalf("a missing key file must not prevent startup: %v", err)
	}
	if len(svc.KeyIDs()) != 0 {
		t.Fatal("no keys should be configured")
	}
	// No credentials configured means no caller is authenticated - never the reverse.
	if _, err := svc.Authenticate(context.Background(), "Bearer anything.at-all"); !domain.IsCode(err, domain.CodeUnauthenticated) {
		t.Fatalf("expected unauthenticated, got %s", domain.CodeOf(err))
	}
}

func TestLoadRejectsMalformedKeyFiles(t *testing.T) {
	dir := t.TempDir()
	cases := map[string]string{
		"not-json.json":      `{`,
		"wrong-version.json": `{"version": 99, "keys": []}`,
		"no-digest.json":     `{"version": 1, "keys": [{"key_id": "a", "principal_id": "p"}]}`,
		"bad-digest.json":    `{"version": 1, "keys": [{"key_id": "a", "secret_sha256": "xyz", "principal_id": "p"}]}`,
		"bad-role.json":      `{"version": 1, "keys": [{"key_id": "a", "secret_sha256": "` + hex.EncodeToString(make([]byte, 32)) + `", "principal_id": "p", "system_role": "wizard"}]}`,
		"duplicate.json": `{"version": 1, "keys": [
			{"key_id": "a", "secret_sha256": "` + hex.EncodeToString(make([]byte, 32)) + `", "principal_id": "p"},
			{"key_id": "a", "secret_sha256": "` + hex.EncodeToString(make([]byte, 32)) + `", "principal_id": "q"}]}`,
	}

	for name, body := range cases {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		if _, err := identity.Load(context.Background(), path, nil, nil); err == nil {
			t.Fatalf("%s must be rejected rather than partially loaded", name)
		}
	}
}

func TestAuthenticate(t *testing.T) {
	f := pgtest.NewFixture(t)
	path := writeKeyFile(t, "alice", "s3cret", domain.RoleAdmin)

	svc, err := identity.Load(context.Background(), path, f.Store, nil)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	principal, err := svc.Authenticate(context.Background(), "Bearer key-alice.s3cret")
	if err != nil {
		t.Fatalf("valid credentials rejected: %v", err)
	}
	if principal.ID != "alice" || principal.SystemRole != domain.RoleAdmin {
		t.Fatalf("unexpected principal: %+v", principal)
	}

	for _, header := range []string{
		"",
		"key-alice.s3cret",          // no scheme
		"Basic key-alice.s3cret",    // wrong scheme
		"Bearer key-alice",          // no secret
		"Bearer key-alice.",         // empty secret
		"Bearer .s3cret",            // no key id
		"Bearer key-alice.wrong",    // wrong secret
		"Bearer key-unknown.s3cret", // unknown key
	} {
		if _, err := svc.Authenticate(context.Background(), header); !domain.IsCode(err, domain.CodeUnauthenticated) {
			t.Fatalf("header %q must be rejected as unauthenticated, got %s", header, domain.CodeOf(err))
		}
	}

	// The scheme is case-insensitive per RFC 7235.
	if _, err := svc.Authenticate(context.Background(), "bearer key-alice.s3cret"); err != nil {
		t.Fatalf("lowercase scheme must be accepted: %v", err)
	}
}

func TestAuthenticateReflectsCurrentGrants(t *testing.T) {
	f := pgtest.NewFixture(t)
	ctx := context.Background()
	path := writeKeyFile(t, "alice", "s3cret", domain.RoleAdmin)

	svc, err := identity.Load(ctx, path, f.Store, nil)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	principal, err := svc.Authenticate(ctx, "Bearer key-alice.s3cret")
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if len(principal.Grants) != 0 {
		t.Fatal("a new principal must start with no workspace access")
	}

	ws := f.Primary.Workspace.ID
	if err := f.Store.Grant(ctx, "alice", ws, domain.RoleWriter, f.Primary.Principal.ID); err != nil {
		t.Fatalf("grant: %v", err)
	}

	// Grants are read per request, so access changes take effect immediately rather
	// than at the next restart.
	principal, err = svc.Authenticate(ctx, "Bearer key-alice.s3cret")
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if role, ok := principal.GrantFor(ws); !ok || role != domain.RoleWriter {
		t.Fatalf("expected a writer grant, got %q (ok=%v)", role, ok)
	}

	if err := f.Store.Revoke(ctx, "alice", ws, f.Primary.Principal.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	principal, err = svc.Authenticate(ctx, "Bearer key-alice.s3cret")
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if _, ok := principal.GrantFor(ws); ok {
		t.Fatal("a revoked grant must stop granting access immediately")
	}
}

func TestAuthorizeWorkspaceRequiresAGrant(t *testing.T) {
	f := pgtest.NewFixture(t)
	ctx := context.Background()
	path := writeKeyFile(t, "alice", "s3cret", domain.RoleOwner)

	svc, err := identity.Load(ctx, path, f.Store, nil)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	principal, err := svc.Authenticate(ctx, "Bearer key-alice.s3cret")
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}

	// A high system role does not imply access to a tenant's data.
	if err := svc.AuthorizeWorkspace(ctx, principal, f.Primary.Workspace.ID, domain.RoleReader); !domain.IsCode(err, domain.CodePermissionDenied) {
		t.Fatalf("expected permission_denied without a grant, got %s", domain.CodeOf(err))
	}

	if err := f.Store.Grant(ctx, "alice", f.Primary.Workspace.ID, domain.RoleReader, f.Primary.Principal.ID); err != nil {
		t.Fatalf("grant: %v", err)
	}
	principal, _ = svc.Authenticate(ctx, "Bearer key-alice.s3cret")

	if err := svc.AuthorizeWorkspace(ctx, principal, f.Primary.Workspace.ID, domain.RoleReader); err != nil {
		t.Fatalf("a reader grant must satisfy a reader requirement: %v", err)
	}
	// A reader must not be able to write.
	if err := svc.AuthorizeWorkspace(ctx, principal, f.Primary.Workspace.ID, domain.RoleWriter); !domain.IsCode(err, domain.CodePermissionDenied) {
		t.Fatalf("expected permission_denied for a writer requirement, got %s", domain.CodeOf(err))
	}
}

func TestResolveGraphSpaceDerivesWorkspaceFromTheGraphSpace(t *testing.T) {
	f := pgtest.NewFixture(t)
	ctx := context.Background()
	other := f.NewTenant(t, "globex")
	path := writeKeyFile(t, "alice", "s3cret", domain.RoleAdmin)

	svc, err := identity.Load(ctx, path, f.Store, nil)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	// Grant before Alice has ever authenticated: an owner must be able to prepare
	// access for someone who has not logged in yet.
	if err := svc.SyncPrincipals(ctx); err != nil {
		t.Fatalf("sync principals: %v", err)
	}
	if err := f.Store.Grant(ctx, "alice", f.Primary.Workspace.ID, domain.RoleWriter, f.Primary.Principal.ID); err != nil {
		t.Fatalf("grant: %v", err)
	}
	principal, err := svc.Authenticate(ctx, "Bearer key-alice.s3cret")
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}

	scope, err := svc.ResolveGraphSpace(ctx, principal, f.Primary.GraphSpace.ID, domain.RoleWriter)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if scope.WorkspaceID != f.Primary.Workspace.ID {
		t.Fatal("scope must be derived from the graph space's own workspace")
	}

	// The other tenant's graph space is reported as missing, not forbidden: whether it
	// exists is itself information (AGENTS.md section 22.1).
	if _, err := svc.ResolveGraphSpace(ctx, principal, other.GraphSpace.ID, domain.RoleReader); !domain.IsCode(err, domain.CodeGraphSpaceNotFound) {
		t.Fatalf("expected graph_space_not_found for another tenant, got %s", domain.CodeOf(err))
	}
	// Malformed and absent identifiers must not reach a query as-is.
	if _, err := svc.ResolveGraphSpace(ctx, principal, "not-a-uuid", domain.RoleReader); !domain.IsCode(err, domain.CodeInvalidArgument) {
		t.Fatalf("expected invalid_argument, got %s", domain.CodeOf(err))
	}
	if _, err := svc.ResolveGraphSpace(ctx, principal, domain.NewGraphSpaceID(), domain.RoleReader); !domain.IsCode(err, domain.CodeGraphSpaceNotFound) {
		t.Fatalf("expected graph_space_not_found, got %s", domain.CodeOf(err))
	}
}

func TestAuthorizeSystem(t *testing.T) {
	svc := identity.NewService(nil,
		map[string]domain.Principal{
			"admin-key":  {ID: "admin", Kind: domain.PrincipalUser, SystemRole: domain.RoleAdmin},
			"reader-key": {ID: "reader", Kind: domain.PrincipalUser, SystemRole: domain.RoleReader},
		},
		map[string]string{"admin-key": "a", "reader-key": "b"})

	admin, err := svc.Authenticate(context.Background(), "Bearer admin-key.a")
	if err != nil {
		t.Fatalf("authenticate admin: %v", err)
	}
	if err := svc.AuthorizeSystem(admin, domain.RoleAdmin); err != nil {
		t.Fatalf("admin must pass an admin requirement: %v", err)
	}

	reader, err := svc.Authenticate(context.Background(), "Bearer reader-key.b")
	if err != nil {
		t.Fatalf("authenticate reader: %v", err)
	}
	if err := svc.AuthorizeSystem(reader, domain.RoleAdmin); !domain.IsCode(err, domain.CodePermissionDenied) {
		t.Fatalf("expected permission_denied, got %s", domain.CodeOf(err))
	}
	// An unauthenticated caller is never authorized.
	if err := svc.AuthorizeSystem(domain.Principal{}, domain.RoleReader); !domain.IsCode(err, domain.CodeUnauthenticated) {
		t.Fatalf("expected unauthenticated, got %s", domain.CodeOf(err))
	}
}

func TestGenerateKeyProducesUsableCredentials(t *testing.T) {
	keyID, secret, entry, err := identity.GenerateKey("ops", "Ops", domain.PrincipalService, domain.RoleAdmin)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if keyID == "" || secret == "" {
		t.Fatal("both a key id and a secret are required")
	}
	if entry["secret_sha256"] == secret {
		t.Fatal("the stored entry must hold a digest, never the secret itself")
	}

	digest := sha256.Sum256([]byte(secret))
	if entry["secret_sha256"] != hex.EncodeToString(digest[:]) {
		t.Fatal("the stored digest must match the generated secret")
	}

	// Two calls must not collide.
	otherID, otherSecret, _, err := identity.GenerateKey("ops", "Ops", domain.PrincipalService, domain.RoleAdmin)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if otherID == keyID || otherSecret == secret {
		t.Fatal("generated credentials must be unique")
	}

	if got := identity.FormatCredential(keyID, secret); got != keyID+"."+secret {
		t.Fatalf("unexpected credential format: %s", got)
	}
}
