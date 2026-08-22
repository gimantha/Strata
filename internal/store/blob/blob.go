// Package blob stores raw source material outside the relational ledger.
//
// Raw source is preserved for every ingested event: an assertion must always be
// traceable back to the bytes it came from (AGENTS.md sections 2.2, 6.4). The
// interface is deliberately small so an S3-compatible adapter can replace the
// filesystem implementation without touching ingestion.
package blob

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Info describes a stored object.
type Info struct {
	Key  string
	Size int64
}

// ErrNotFound is returned when a key is absent.
var ErrNotFound = errors.New("blob not found")

// Store is the blob port. Implementations must be safe for concurrent use.
type Store interface {
	// Put stores data under key. It is idempotent: writing identical content to the
	// same key twice is not an error, which matters because keys are content
	// addresses and ingestion replays are normal.
	Put(ctx context.Context, key string, data []byte) (Info, error)
	Get(ctx context.Context, key string) ([]byte, error)
	Stat(ctx context.Context, key string) (Info, error)
	Delete(ctx context.Context, key string) error
	// Healthy reports whether the backend is usable, for the readiness endpoint.
	Healthy(ctx context.Context) error
	// Name identifies the backend, recorded on artifact rows so a later migration
	// can tell where existing bytes live.
	Name() string
}

// Key builds the storage key for content in a workspace.
//
// Keys are content-addressed and workspace-prefixed: identical bytes in one
// workspace are stored once, and no key can ever span tenants. The two-level fan-out
// keeps directory sizes manageable on filesystem backends.
func Key(workspaceID, contentHash string) (string, error) {
	const op = "blob.Key"

	if err := validateSegment(workspaceID); err != nil {
		return "", fmt.Errorf("%s: workspace: %w", op, err)
	}
	if err := validateHash(contentHash); err != nil {
		return "", fmt.Errorf("%s: content hash: %w", op, err)
	}
	return fmt.Sprintf("%s/sha256/%s/%s/%s", workspaceID, contentHash[0:2], contentHash[2:4], contentHash), nil
}

func validateHash(h string) error {
	if len(h) != 64 {
		return fmt.Errorf("must be a 64-character sha256 digest, got %d characters", len(h))
	}
	for _, r := range h {
		if !(r >= '0' && r <= '9') && !(r >= 'a' && r <= 'f') {
			return errors.New("must be lowercase hexadecimal")
		}
	}
	return nil
}

// validateSegment rejects anything that could escape the storage root or confuse a
// backend's namespace. Path traversal through a key is a tenancy breach, not a bug.
func validateSegment(s string) error {
	if s == "" {
		return errors.New("must not be empty")
	}
	if strings.ContainsAny(s, `/\`) || strings.Contains(s, "..") {
		return errors.New("must not contain path separators or parent references")
	}
	for _, r := range s {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_'
		if !ok {
			return errors.New("must contain only letters, digits, hyphen, or underscore")
		}
	}
	return nil
}

// FS is a filesystem-backed blob store, the development and single-node default.
type FS struct {
	root string
}

// NewFS creates the storage root if it does not exist.
func NewFS(root string) (*FS, error) {
	if root == "" {
		return nil, errors.New("blob: storage root is required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("blob: resolve storage root: %w", err)
	}
	if err := os.MkdirAll(abs, 0o750); err != nil {
		return nil, fmt.Errorf("blob: create storage root: %w", err)
	}
	return &FS{root: abs}, nil
}

func (f *FS) Name() string { return "filesystem" }

// path resolves a key inside the root, refusing anything that escapes it.
func (f *FS) path(key string) (string, error) {
	if key == "" {
		return "", errors.New("blob: key is required")
	}
	if strings.Contains(key, "..") {
		return "", errors.New("blob: key must not contain parent references")
	}
	full := filepath.Join(f.root, filepath.FromSlash(key))
	if !strings.HasPrefix(full, f.root+string(os.PathSeparator)) {
		return "", errors.New("blob: key escapes the storage root")
	}
	return full, nil
}

// Put writes bytes atomically: content goes to a temporary file in the destination
// directory and is renamed into place, so a crash mid-write never leaves a partial
// object that a later read would mistake for complete source material.
func (f *FS) Put(ctx context.Context, key string, data []byte) (Info, error) {
	if err := ctx.Err(); err != nil {
		return Info{}, err
	}
	full, err := f.path(key)
	if err != nil {
		return Info{}, err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
		return Info{}, fmt.Errorf("blob: create directory: %w", err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(full), ".tmp-*")
	if err != nil {
		return Info{}, fmt.Errorf("blob: create temporary file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return Info{}, fmt.Errorf("blob: write: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return Info{}, fmt.Errorf("blob: sync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return Info{}, fmt.Errorf("blob: close: %w", err)
	}
	if err := os.Chmod(tmpName, 0o640); err != nil {
		return Info{}, fmt.Errorf("blob: chmod: %w", err)
	}
	if err := os.Rename(tmpName, full); err != nil {
		return Info{}, fmt.Errorf("blob: rename into place: %w", err)
	}
	return Info{Key: key, Size: int64(len(data))}, nil
}

func (f *FS) Get(ctx context.Context, key string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	full, err := f.path(key)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(full)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, key)
		}
		return nil, fmt.Errorf("blob: read: %w", err)
	}
	return data, nil
}

func (f *FS) Stat(ctx context.Context, key string) (Info, error) {
	if err := ctx.Err(); err != nil {
		return Info{}, err
	}
	full, err := f.path(key)
	if err != nil {
		return Info{}, err
	}
	st, err := os.Stat(full)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Info{}, fmt.Errorf("%w: %s", ErrNotFound, key)
		}
		return Info{}, fmt.Errorf("blob: stat: %w", err)
	}
	return Info{Key: key, Size: st.Size()}, nil
}

// Delete removes an object. Absence is not an error, so privacy erasure and cleanup
// jobs are safe to re-run (AGENTS.md section 23).
func (f *FS) Delete(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	full, err := f.path(key)
	if err != nil {
		return err
	}
	if err := os.Remove(full); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("blob: delete: %w", err)
	}
	return nil
}

// Healthy verifies the root is writable, which readiness reports before the process
// accepts ingestion it could not archive.
func (f *FS) Healthy(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	probe, err := os.CreateTemp(f.root, ".health-*")
	if err != nil {
		return fmt.Errorf("blob: storage root is not writable: %w", err)
	}
	name := probe.Name()
	probe.Close()
	return os.Remove(name)
}
