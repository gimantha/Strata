// Package identity authenticates callers and resolves the scope they may act in.
//
// The rule this package exists to enforce: authenticated identity determines which
// workspaces and graph spaces a caller can reach, and a request can never widen that
// by naming a different workspace (AGENTS.md sections 2.6, 22.1).
//
// Authentication is API-key based for this phase. Secrets live in a
// secret-manager-provided file, never in the database and never in the repository.
// Phase 11 replaces this with real authentication without changing the authorization
// surface below.
package identity

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"strings"
	"sync"

	"github.com/gimantha/strata/internal/domain"
)

// GrantStore is the persistence this package needs. It is declared here, by the
// consumer, rather than in the store package (AGENTS.md section 5).
type GrantStore interface {
	UpsertPrincipal(ctx context.Context, p domain.Principal) error
	GrantFor(ctx context.Context, principal domain.PrincipalID, ws domain.WorkspaceID) (domain.Role, bool, error)
	GrantsFor(ctx context.Context, principal domain.PrincipalID) ([]domain.Grant, error)
	GetPrincipal(ctx context.Context, id domain.PrincipalID) (domain.Principal, error)
	GetGraphSpace(ctx context.Context, id domain.GraphSpaceID) (domain.GraphSpace, error)
}

// Service authenticates API keys and answers authorization questions.
type Service struct {
	keys  map[string]apiKey
	store GrantStore
	// registered caches which principals have been written to the database, so the
	// row that grants and audit records reference is created once per process rather
	// than on every request.
	registered sync.Map
}

type apiKey struct {
	keyID        string
	secretSHA256 []byte
	principal    domain.Principal
}

// keyFile is the on-disk format. Only a hash of each secret is stored, so a leaked
// key file does not directly yield usable credentials.
type keyFile struct {
	Version int           `json:"version"`
	Keys    []keyFileItem `json:"keys"`
}

type keyFileItem struct {
	KeyID        string `json:"key_id"`
	SecretSHA256 string `json:"secret_sha256"`
	PrincipalID  string `json:"principal_id"`
	Kind         string `json:"kind"`
	DisplayName  string `json:"display_name"`
	SystemRole   string `json:"system_role"`
	Comment      string `json:"comment,omitempty"`
}

// Load reads the key file.
//
// It performs no database work: a process must be able to start, and to run migrations,
// against a database whose tables do not exist yet. Principal rows are created lazily
// when a key is first used.
//
// A missing file is not fatal: the process starts with no valid credentials and every
// authenticated route rejects callers. Failing closed keeps health checks and local
// experimentation working without ever defaulting to open access.
func Load(ctx context.Context, path string, store GrantStore, logger *slog.Logger) (*Service, error) {
	const op = "identity.Load"

	svc := &Service{keys: map[string]apiKey{}, store: store}

	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			if logger != nil {
				logger.WarnContext(ctx, "no API key file found; authenticated routes will reject all callers",
					slog.String("path", path))
			}
			return svc, nil
		}
		return nil, domain.Wrap(err, domain.CodeInvalidArgument, op, "cannot read API key file")
	}

	var parsed keyFile
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, domain.Wrap(err, domain.CodeInvalidArgument, op, "cannot parse API key file")
	}
	if parsed.Version != 1 {
		return nil, domain.Errorf(domain.CodeInvalidArgument, op,
			"unsupported API key file version %d", parsed.Version)
	}

	for i, item := range parsed.Keys {
		if item.KeyID == "" || item.SecretSHA256 == "" || item.PrincipalID == "" {
			return nil, domain.Errorf(domain.CodeInvalidArgument, op,
				"key %d needs key_id, secret_sha256, and principal_id", i)
		}
		digest, err := hex.DecodeString(item.SecretSHA256)
		if err != nil || len(digest) != sha256.Size {
			return nil, domain.Errorf(domain.CodeInvalidArgument, op,
				"key %q has a malformed secret_sha256 (expected a hex sha256 digest)", item.KeyID)
		}
		if _, dup := svc.keys[item.KeyID]; dup {
			return nil, domain.Errorf(domain.CodeInvalidArgument, op, "duplicate key_id %q", item.KeyID)
		}

		kind, err := domain.ParsePrincipalKind(orDefault(item.Kind, string(domain.PrincipalUser)))
		if err != nil {
			return nil, err
		}
		role, err := domain.ParseRole(orDefault(item.SystemRole, string(domain.RoleReader)))
		if err != nil {
			return nil, err
		}

		principal := domain.Principal{
			ID:          domain.PrincipalID(item.PrincipalID),
			Kind:        kind,
			DisplayName: orDefault(item.DisplayName, item.PrincipalID),
			SystemRole:  role,
		}
		svc.keys[item.KeyID] = apiKey{keyID: item.KeyID, secretSHA256: digest, principal: principal}
	}

	if logger != nil {
		logger.InfoContext(ctx, "API keys loaded", slog.Int("count", len(svc.keys)))
	}
	return svc, nil
}

// NewService builds a service directly from principals, for tests.
func NewService(store GrantStore, keys map[string]domain.Principal, secrets map[string]string) *Service {
	svc := &Service{keys: map[string]apiKey{}, store: store}
	for keyID, principal := range keys {
		digest := sha256.Sum256([]byte(secrets[keyID]))
		svc.keys[keyID] = apiKey{keyID: keyID, secretSHA256: digest[:], principal: principal}
	}
	return svc
}

// SyncPrincipals registers every configured principal.
//
// Authentication would register them lazily anyway, but a workspace owner must be able
// to grant access to a colleague who has not logged in yet, and a grant references a
// principal row. Callers run this once the schema is known to exist.
func (s *Service) SyncPrincipals(ctx context.Context) error {
	if s.store == nil {
		return nil
	}
	for _, key := range s.keys {
		if err := s.ensureRegistered(ctx, key.principal); err != nil {
			return err
		}
	}
	return nil
}

// KeyIDs lists configured key identifiers, for CLI selection. Secrets are not
// exposed.
func (s *Service) KeyIDs() []string {
	out := make([]string, 0, len(s.keys))
	for id := range s.keys {
		out = append(out, id)
	}
	return out
}

// PrincipalForKeyID returns the principal behind a key identifier, without
// authenticating. The admin CLI uses it to act as a configured principal while still
// going through normal grant checks.
func (s *Service) PrincipalForKeyID(ctx context.Context, keyID string) (domain.Principal, error) {
	const op = "identity.PrincipalForKeyID"

	key, ok := s.keys[keyID]
	if !ok {
		return domain.Principal{}, domain.Errorf(domain.CodeUnauthenticated, op, "unknown key id")
	}
	return s.withGrants(ctx, key.principal)
}

// PrincipalForID loads a registered principal with its current grants.
//
// For answering questions about somebody other than the caller — "what would this agent be
// allowed to see" — which is a different question from authenticating them, and must not be
// a way to obtain their credentials.
func (s *Service) PrincipalForID(ctx context.Context, id domain.PrincipalID) (domain.Principal, error) {
	const op = "identity.PrincipalForID"

	for _, key := range s.keys {
		if key.principal.ID == id {
			return s.withGrants(ctx, key.principal)
		}
	}

	if s.store == nil {
		return domain.Principal{}, domain.Errorf(domain.CodeNotFound, op,
			"principal %s is not registered", id)
	}
	stored, err := s.store.GetPrincipal(ctx, id)
	if err != nil {
		return domain.Principal{}, err
	}
	return s.withGrants(ctx, stored)
}

// Authenticate resolves an Authorization header into a principal with its grants.
//
// The expected form is "Bearer <key_id>.<secret>". Comparison is constant-time, and
// every failure returns the same opaque error: a caller must not be able to
// distinguish an unknown key from a wrong secret.
func (s *Service) Authenticate(ctx context.Context, authorization string) (domain.Principal, error) {
	const op = "identity.Authenticate"

	unauthenticated := domain.Errorf(domain.CodeUnauthenticated, op, "invalid credentials")

	scheme, token, ok := strings.Cut(strings.TrimSpace(authorization), " ")
	if !ok || !strings.EqualFold(scheme, "bearer") {
		return domain.Principal{}, unauthenticated
	}
	keyID, secret, ok := strings.Cut(strings.TrimSpace(token), ".")
	if !ok || keyID == "" || secret == "" {
		return domain.Principal{}, unauthenticated
	}

	key, found := s.keys[keyID]
	if !found {
		// Hash anyway so the response time does not reveal whether the key exists.
		_ = sha256.Sum256([]byte(secret))
		return domain.Principal{}, unauthenticated
	}
	digest := sha256.Sum256([]byte(secret))
	if subtle.ConstantTimeCompare(digest[:], key.secretSHA256) != 1 {
		return domain.Principal{}, unauthenticated
	}

	return s.withGrants(ctx, key.principal)
}

// withGrants attaches the principal's current workspace grants, read fresh so a
// revocation takes effect on the next request rather than at the next restart.
func (s *Service) withGrants(ctx context.Context, p domain.Principal) (domain.Principal, error) {
	if s.store == nil {
		return p, nil
	}
	if err := s.ensureRegistered(ctx, p); err != nil {
		return domain.Principal{}, err
	}
	grants, err := s.store.GrantsFor(ctx, p.ID)
	if err != nil {
		return domain.Principal{}, err
	}
	p.Grants = grants
	return p, nil
}

// ensureRegistered writes the principal row that grants and audit records reference.
// It runs on first use rather than at startup so the schema can be migrated by the same
// binary that later authenticates against it.
func (s *Service) ensureRegistered(ctx context.Context, p domain.Principal) error {
	if _, done := s.registered.Load(p.ID); done {
		return nil
	}
	if err := s.store.UpsertPrincipal(ctx, p); err != nil {
		return err
	}
	s.registered.Store(p.ID, struct{}{})
	return nil
}

// AuthorizeWorkspace checks that a principal holds at least the required role in a
// workspace. The grant table is authoritative: a high system role does not imply
// access to a tenant's data.
func (s *Service) AuthorizeWorkspace(ctx context.Context, p domain.Principal, ws domain.WorkspaceID, need domain.Role) error {
	const op = "identity.AuthorizeWorkspace"

	if p.ID == "" {
		return domain.Errorf(domain.CodeUnauthenticated, op, "no authenticated principal")
	}
	if domain.IsZero(ws) {
		return domain.Errorf(domain.CodeInvalidArgument, op, "workspace is required")
	}

	role, ok, err := s.store.GrantFor(ctx, p.ID, ws)
	if err != nil {
		return err
	}
	// Not-found and forbidden are the same answer on purpose: existence of another
	// tenant's workspace is itself information.
	if !ok || !role.AtLeast(need) {
		return domain.Errorf(domain.CodePermissionDenied, op, "principal may not %s this workspace", need)
	}
	return nil
}

// ResolveGraphSpace turns a graph space identifier from a request path into an
// authorized scope.
//
// The workspace comes from the graph space row, never from the request, so a caller
// cannot reach another tenant's data by supplying its workspace id (section 22.1).
func (s *Service) ResolveGraphSpace(ctx context.Context, p domain.Principal, id domain.GraphSpaceID, need domain.Role) (domain.Scope, error) {
	const op = "identity.ResolveGraphSpace"

	if domain.IsZero(id) || !domain.ValidUUID(id) {
		return domain.Scope{}, domain.Errorf(domain.CodeInvalidArgument, op, "malformed graph space id")
	}

	gs, err := s.store.GetGraphSpace(ctx, id)
	if err != nil {
		return domain.Scope{}, err
	}
	if err := s.AuthorizeWorkspace(ctx, p, gs.WorkspaceID, need); err != nil {
		// Report a missing graph space rather than a denial, so probing for
		// identifiers reveals nothing about other tenants.
		return domain.Scope{}, domain.Errorf(domain.CodeGraphSpaceNotFound, op, "graph space not found")
	}
	return domain.Scope{WorkspaceID: gs.WorkspaceID, GraphSpaceID: gs.ID}, nil
}

// AuthorizeSystem checks a principal's system role, which gates operations that exist
// outside any workspace, such as creating one.
func (s *Service) AuthorizeSystem(p domain.Principal, need domain.Role) error {
	const op = "identity.AuthorizeSystem"

	if p.ID == "" {
		return domain.Errorf(domain.CodeUnauthenticated, op, "no authenticated principal")
	}
	if !p.SystemRole.AtLeast(need) {
		return domain.Errorf(domain.CodePermissionDenied, op, "principal lacks the %s system role", need)
	}
	return nil
}

// GenerateKey mints a key identifier and secret, returning the file entry to store.
// The plaintext secret is returned once, for the operator to save; only its digest is
// ever persisted.
func GenerateKey(principalID, displayName string, kind domain.PrincipalKind, role domain.Role) (keyID, secret string, entry map[string]string, err error) {
	const op = "identity.GenerateKey"

	idBytes := make([]byte, 6)
	secretBytes := make([]byte, 32)
	if _, err := rand.Read(idBytes); err != nil {
		return "", "", nil, domain.Wrap(err, domain.CodeInternal, op, "cannot generate key id")
	}
	if _, err := rand.Read(secretBytes); err != nil {
		return "", "", nil, domain.Wrap(err, domain.CodeInternal, op, "cannot generate secret")
	}

	keyID = "k_" + hex.EncodeToString(idBytes)
	secret = hex.EncodeToString(secretBytes)
	digest := sha256.Sum256([]byte(secret))

	return keyID, secret, map[string]string{
		"key_id":        keyID,
		"secret_sha256": hex.EncodeToString(digest[:]),
		"principal_id":  principalID,
		"kind":          string(kind),
		"display_name":  displayName,
		"system_role":   string(role),
	}, nil
}

// FormatCredential renders the value a client sends in the Authorization header.
func FormatCredential(keyID, secret string) string { return fmt.Sprintf("%s.%s", keyID, secret) }

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
