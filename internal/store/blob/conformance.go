package blob

import (
	"errors"
	"strings"
	"testing"
)

// RunConformance exercises the behaviour every blob backend must share.
//
// A port with two implementations is only useful if callers can stop caring which one is
// configured, and that is a claim about behaviour rather than about method signatures. The
// compiler checks the signatures. This checks the parts that actually differ between a
// filesystem and an object store: how absence is reported, whether an overwrite is an error,
// what an empty object does, and whether a key with slashes in it survives a round trip.
//
// Exported and in the non-test file so both backends' test packages can call it, and so a
// third backend added later inherits the same bar rather than a subset somebody reimplements.
func RunConformance(t *testing.T, name string, store Store) {
	t.Helper()

	t.Run(name+"/round trip", func(t *testing.T) {
		ctx := t.Context()
		key := "01a00000-0000-7000-8000-000000000001/sha256/ab/cd/" +
			strings.Repeat("ab", 32)
		payload := []byte("Ada Lovelace worked on the Analytical Engine.")

		info, err := store.Put(ctx, key, payload)
		if err != nil {
			t.Fatalf("put: %v", err)
		}
		if info.Size != int64(len(payload)) {
			t.Fatalf("put reported %d bytes for a %d-byte payload", info.Size, len(payload))
		}

		got, err := store.Get(ctx, key)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if string(got) != string(payload) {
			t.Fatalf("content changed in storage: %q", got)
		}

		stat, err := store.Stat(ctx, key)
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		if stat.Size != int64(len(payload)) {
			t.Fatalf("stat reported %d bytes, stored %d", stat.Size, len(payload))
		}
	})

	t.Run(name+"/put is idempotent", func(t *testing.T) {
		ctx := t.Context()
		key := "01a00000-0000-7000-8000-000000000001/sha256/11/22/" +
			strings.Repeat("11", 32)
		payload := []byte("written twice")

		if _, err := store.Put(ctx, key, payload); err != nil {
			t.Fatalf("first put: %v", err)
		}
		// Keys are content addresses, so an ingestion replay writes identical bytes to
		// the same key. A backend that treated that as a conflict would turn a normal
		// retry into a failed ingest.
		if _, err := store.Put(ctx, key, payload); err != nil {
			t.Fatalf("second put of identical content: %v", err)
		}

		got, err := store.Get(ctx, key)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if string(got) != string(payload) {
			t.Fatalf("content changed after a repeated put: %q", got)
		}
	})

	t.Run(name+"/absence is ErrNotFound", func(t *testing.T) {
		ctx := t.Context()
		key := "01a00000-0000-7000-8000-000000000001/sha256/ff/ff/" +
			strings.Repeat("ff", 32)

		// Both paths, because object stores report them differently: GetObject returns a
		// typed error and HeadObject has no body to carry one, so a backend can easily
		// translate one and not the other.
		if _, err := store.Get(ctx, key); !errors.Is(err, ErrNotFound) {
			t.Fatalf("get of a missing key returned %v, want ErrNotFound", err)
		}
		if _, err := store.Stat(ctx, key); !errors.Is(err, ErrNotFound) {
			t.Fatalf("stat of a missing key returned %v, want ErrNotFound", err)
		}
	})

	t.Run(name+"/delete is idempotent", func(t *testing.T) {
		ctx := t.Context()
		key := "01a00000-0000-7000-8000-000000000001/sha256/33/44/" +
			strings.Repeat("33", 32)

		if _, err := store.Put(ctx, key, []byte("temporary")); err != nil {
			t.Fatalf("put: %v", err)
		}
		if err := store.Delete(ctx, key); err != nil {
			t.Fatalf("delete: %v", err)
		}
		if _, err := store.Get(ctx, key); !errors.Is(err, ErrNotFound) {
			t.Fatalf("the object survived deletion: %v", err)
		}
		// Deleting what is already gone is success. A caller cleaning up should not have
		// to distinguish "gone now" from "gone already".
		if err := store.Delete(ctx, key); err != nil {
			t.Fatalf("second delete: %v", err)
		}
	})

	t.Run(name+"/empty content round trips", func(t *testing.T) {
		ctx := t.Context()
		key := "01a00000-0000-7000-8000-000000000001/sha256/55/66/" +
			strings.Repeat("55", 32)

		// An empty payload is legal: a source event can archive a zero-byte document, and
		// a backend that stored nothing would make it indistinguishable from a missing
		// artifact, which is a provenance chain that ends in a lie rather than a gap.
		if _, err := store.Put(ctx, key, []byte{}); err != nil {
			t.Fatalf("put empty: %v", err)
		}
		got, err := store.Get(ctx, key)
		if err != nil {
			t.Fatalf("get empty: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("empty content came back as %d bytes", len(got))
		}
		info, err := store.Stat(ctx, key)
		if err != nil {
			t.Fatalf("stat empty: %v", err)
		}
		if info.Size != 0 {
			t.Fatalf("stat reported %d bytes for empty content", info.Size)
		}
	})

	t.Run(name+"/healthy", func(t *testing.T) {
		if err := store.Healthy(t.Context()); err != nil {
			t.Fatalf("a usable backend reported unhealthy: %v", err)
		}
	})

	t.Run(name+"/names itself", func(t *testing.T) {
		// Recorded on every artifact row so a later migration can tell where existing
		// bytes live. An empty name makes those rows useless.
		if strings.TrimSpace(store.Name()) == "" {
			t.Fatal("the backend does not identify itself")
		}
	})
}
