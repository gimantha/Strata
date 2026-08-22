package blob

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestKeyIsContentAddressedAndWorkspacePrefixed(t *testing.T) {
	const hash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

	key, err := Key("ws-1", hash)
	if err != nil {
		t.Fatalf("valid key rejected: %v", err)
	}
	want := "ws-1/sha256/e3/b0/" + hash
	if key != want {
		t.Fatalf("Key() = %q, want %q", key, want)
	}

	// Identical content in different workspaces must never share a key.
	other, err := Key("ws-2", hash)
	if err != nil {
		t.Fatalf("valid key rejected: %v", err)
	}
	if strings.HasPrefix(other, "ws-1") {
		t.Fatal("keys must be workspace-prefixed")
	}
}

func TestKeyRejectsTraversalAndBadHashes(t *testing.T) {
	const hash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

	for _, ws := range []string{"", "..", "a/b", `a\b`, "ws 1", "../../etc"} {
		if _, err := Key(ws, hash); err == nil {
			t.Fatalf("workspace %q must be rejected: a key that escapes its prefix is a tenancy breach", ws)
		}
	}
	for _, h := range []string{"", "short", strings.Repeat("z", 64), strings.ToUpper(hash)} {
		if _, err := Key("ws-1", h); err == nil {
			t.Fatalf("content hash %q must be rejected", h)
		}
	}
}

func TestFSRoundTripAndIdempotentPut(t *testing.T) {
	store, err := NewFS(filepath.Join(t.TempDir(), "blobs"))
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	ctx := context.Background()
	payload := []byte("the quick brown fox")

	info, err := store.Put(ctx, "ws/sha256/aa/bb/object", payload)
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if info.Size != int64(len(payload)) {
		t.Fatalf("reported size %d, want %d", info.Size, len(payload))
	}

	// Replay: writing the same content again must succeed, because ingestion replays
	// are routine and the key is a content address.
	if _, err := store.Put(ctx, "ws/sha256/aa/bb/object", payload); err != nil {
		t.Fatalf("idempotent put: %v", err)
	}

	got, err := store.Get(ctx, "ws/sha256/aa/bb/object")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("round trip changed content: %q", got)
	}

	st, err := store.Stat(ctx, "ws/sha256/aa/bb/object")
	if err != nil || st.Size != int64(len(payload)) {
		t.Fatalf("stat: %+v, %v", st, err)
	}
}

func TestFSMissingKeyIsNotFound(t *testing.T) {
	store, err := NewFS(t.TempDir())
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	ctx := context.Background()

	if _, err := store.Get(ctx, "ws/missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
	if _, err := store.Stat(ctx, "ws/missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
	// Deleting something absent must succeed so erasure jobs are safe to re-run.
	if err := store.Delete(ctx, "ws/missing"); err != nil {
		t.Fatalf("delete of a missing key must be a no-op, got %v", err)
	}
}

func TestFSRejectsKeysEscapingRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "blobs")
	store, err := NewFS(root)
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	ctx := context.Background()

	for _, key := range []string{"../escape", "ws/../../escape", ""} {
		if _, err := store.Put(ctx, key, []byte("x")); err == nil {
			t.Fatalf("key %q must not be writable outside the storage root", key)
		}
	}
}

func TestFSHealthy(t *testing.T) {
	store, err := NewFS(t.TempDir())
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	if err := store.Healthy(context.Background()); err != nil {
		t.Fatalf("a fresh store must be healthy: %v", err)
	}
	if store.Name() != "filesystem" {
		t.Fatalf("unexpected backend name %q", store.Name())
	}
}

func TestFSDeleteRemovesContent(t *testing.T) {
	store, err := NewFS(t.TempDir())
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	ctx := context.Background()

	if _, err := store.Put(ctx, "ws/object", []byte("secret")); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := store.Delete(ctx, "ws/object"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := store.Get(ctx, "ws/object"); !errors.Is(err, ErrNotFound) {
		t.Fatal("deleted content must be unreadable: a surviving blob is a leak")
	}
}
