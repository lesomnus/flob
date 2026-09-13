package flob

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/lesomnus/flob/internal/x"
)

func TestOsStore(t *testing.T) {
	new_stores := func(t *testing.T) Stores {
		t.Helper()
		root := t.TempDir()
		return NewOsStores(root)
	}
	new_store := func(t *testing.T) Store {
		t.Helper()
		stores := new_stores(t)
		return stores.Use("test")
	}

	path_to_blob := func(s Store, d Digest) string {
		return s.(OsStore).pathToBlob(d)
	}

	t.Run("contract", func(t *testing.T) {
		testStore(t, new_stores)
	})

	t.Run("single-ref erase removes global blob", func(t *testing.T) {
		ctx, x := x.New(t)
		s := new_store(t)

		m, err := s.Add(ctx, Meta{}, x.Reader())
		x.NoError(err)

		pb := path_to_blob(s, m.Digest)
		n, err := nlink(pb)
		x.NoError(err)
		x.Eq(n, 2)

		err = s.Erase(ctx, m.Digest)
		x.NoError(err)

		// Global blob must be gone after single-ref erase.
		_, err = nlink(pb)
		x.ErrorIs(err, os.ErrNotExist)
	})
	t.Run("cross-repo same blob shares hard link", func(t *testing.T) {
		ctx, x := x.New(t)

		root := t.TempDir()
		stores := NewOsStores(root)
		s1 := stores.Use("store1")
		s2 := stores.Use("store2")

		m1, err := s1.Add(ctx, Meta{}, x.Reader())
		x.NoError(err)

		_, err = s2.Add(ctx, Meta{}, x.Reader())
		x.NoError(err)

		// Both stores reference the same digest; global blob must have nlink == 3
		// (1 for the global namespace + 1 per repo).
		pb := path_to_blob(s1, m1.Digest)
		n, err := nlink(pb)
		x.NoError(err)
		x.Eq(n, 3)
	})
	t.Run("multi-ref erase keeps global blob", func(t *testing.T) {
		ctx, x := x.New(t)

		root := t.TempDir()
		stores := NewOsStores(root)
		s1 := stores.Use("store1")
		s2 := stores.Use("store2")

		m, err := s1.Add(ctx, Meta{}, x.Reader())
		x.NoError(err)
		_, err = s2.Add(ctx, Meta{}, x.Reader())
		x.NoError(err)

		// Erase from s1 only.
		x.NoError(s1.Erase(ctx, m.Digest))

		// Global blob must still exist; only s2's repo link remains (nlink == 2).
		pb := path_to_blob(s1, m.Digest)
		n, err := nlink(pb)
		x.NoError(err)
		x.Eq(n, 2)

		// s1 must no longer see the blob.
		_, err = statMeta(ctx, s1, m.Digest)
		x.ErrorIs(err, ErrNotExist)

		// s2 must still see the blob.
		_, err = statMeta(ctx, s2, m.Digest)
		x.NoError(err)
	})
	t.Run("all refs erased removes global blob", func(t *testing.T) {
		ctx, x := x.New(t)

		root := t.TempDir()
		stores := NewOsStores(root)
		s1 := stores.Use("store1")
		s2 := stores.Use("store2")

		m, err := s1.Add(ctx, Meta{}, x.Reader())
		x.NoError(err)
		_, err = s2.Add(ctx, Meta{}, x.Reader())
		x.NoError(err)

		err = s1.Erase(ctx, m.Digest)
		x.NoError(err)
		err = s2.Erase(ctx, m.Digest)
		x.NoError(err)

		// Global blob must be gone after all refs erased.
		pb := path_to_blob(s1, m.Digest)
		_, err = nlink(pb)
		x.ErrorIs(err, os.ErrNotExist)
	})

	t.Run("malformed digest does not panic", func(t *testing.T) {
		ctx, x := x.New(t)
		s := new_store(t)

		// A digest without the "algo:" separator would panic in go-digest when building a
		// path, so the store must reject it defensively instead.
		bad := Digest("deadbeef")

		_, err := statMeta(ctx, s, bad)
		x.ErrorIs(err, ErrNotExist)

		_, _, err = s.Open(ctx, bad)
		x.ErrorIs(err, ErrNotExist)

		err = s.Label(ctx, bad, Labels{"A": {"b"}})
		x.ErrorIs(err, ErrNotExist)

		// Erase never reports "not exist", so an invalid digest is a no-op success.
		err = s.Erase(ctx, bad)
		x.NoError(err)
	})

	t.Run("add recovers from a leftover repo directory", func(t *testing.T) {
		ctx, x := x.New(t)
		root := t.TempDir()
		s := NewOsStores(root).Use("test").(OsStore)

		d := DigestFromBytes(x.Data())

		// Simulate a crash mid-Erase (RemoveAll unlinks blob, then labels, then the dir):
		// a repo digest directory that holds a labels file but no blob. checkDup only looks
		// at the blob file, so Add reaches the final rename onto this pre-existing directory.
		pr := s.pathToRepo(d)
		x.NoError(os.MkdirAll(pr, 0o755))
		x.NoError(os.WriteFile(filepath.Join(pr, "labels"), []byte("X-Foo: bar\r\n\r\n"), 0o644))

		// The user's Add must still succeed.
		m, err := s.Add(ctx, Meta{}, x.Reader())
		x.NoError(err)
		x.Eq(d, m.Digest)

		r, _, err := s.Open(ctx, d)
		x.NoError(err)
		defer r.Close()
		got, err := io.ReadAll(r)
		x.NoError(err)
		x.Eq(x.Data(), got)
	})

	t.Run("label existence check matches stat and open", func(t *testing.T) {
		ctx, x := x.New(t)
		root := t.TempDir()
		s := NewOsStores(root).Use("test").(OsStore)

		d := DigestFromBytes(x.Data())

		// An orphan repo directory that has a labels file but no blob (e.g. a crash mid-Erase).
		// Stat/Open define existence by the blob file, so they report ErrNotExist; Label must
		// use the same criterion instead of merely checking that the directory exists.
		pr := s.pathToRepo(d)
		x.NoError(os.MkdirAll(pr, 0o755))
		x.NoError(os.WriteFile(filepath.Join(pr, "labels"), []byte("X-Foo: bar\r\n\r\n"), 0o644))

		_, err := statMeta(ctx, s, d)
		x.ErrorIs(err, ErrNotExist)

		_, _, err = s.Open(ctx, d)
		x.ErrorIs(err, ErrNotExist)

		// Label must agree: no blob => ErrNotExist (previously it succeeded on the stray dir).
		err = s.Label(ctx, d, Labels{"A": {"b"}})
		x.ErrorIs(err, ErrNotExist)

		// Once the blob genuinely exists, Label succeeds.
		_, err = s.Add(ctx, Meta{}, x.Reader())
		x.NoError(err)
		x.NoError(s.Label(ctx, d, Labels{"A": {"b"}}))
	})

	t.Run("concurrent add of same content across stores dedupes", func(t *testing.T) {
		ctx, x := x.New(t)
		root := t.TempDir()
		stores := NewOsStores(root)

		const n = 16
		var wg sync.WaitGroup
		errs := make([]error, n)
		digs := make([]Digest, n)
		for i := range n {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				m, err := stores.Use(fmt.Sprintf("store-%d", i)).Add(ctx, Meta{}, x.Reader())
				errs[i] = err
				digs[i] = m.Digest
			}(i)
		}
		wg.Wait()

		for i := range n {
			x.NoError(errs[i])
			x.Eq(digs[0], digs[i])
		}

		// One global inode shared by every repo: nlink == n repo links + 1 global link.
		pb := stores.Use("store-0").(OsStore).pathToBlob(digs[0])
		got, err := nlink(pb)
		x.NoError(err)
		x.Eq(n+1, got)

		// Every repo can read the content back.
		for i := range n {
			r, _, err := stores.Use(fmt.Sprintf("store-%d", i)).Open(ctx, digs[0])
			x.NoError(err)
			data, err := io.ReadAll(r)
			r.Close()
			x.NoError(err)
			x.Eq(x.Data(), data)
		}
	})

	t.Run("concurrent add and erase on the same repo always succeeds", func(t *testing.T) {
		ctx, x := x.New(t)
		root := t.TempDir()
		s := NewOsStores(root).Use("test")

		d := DigestFromBytes(x.Data())

		const workers = 8
		const iters = 250
		var wg sync.WaitGroup
		addErrs := make(chan error, workers*iters)
		for range workers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for range iters {
					// Add must always succeed (or report the benign ErrAlreadyExists),
					// never fail with a leftover-directory error, even while a concurrent
					// Erase is removing the same repo entry.
					if _, err := s.Add(ctx, Meta{}, x.Reader()); err != nil && !errors.Is(err, ErrAlreadyExists) {
						addErrs <- err
					}
					_ = s.Erase(ctx, d)
				}
			}()
		}
		wg.Wait()
		close(addErrs)

		for err := range addErrs {
			t.Fatalf("Add failed under concurrency (must always succeed): %v", err)
		}

		// The store remains usable afterwards.
		if _, err := s.Add(ctx, Meta{}, x.Reader()); err != nil && !errors.Is(err, ErrAlreadyExists) {
			t.Fatalf("final Add failed: %v", err)
		}
	})
}

// TestOsStoreAddWithoutASystemTempDir is the container case: `FROM scratch` has
// no /tmp, and an Add that staged there failed before it read a byte.
func TestOsStoreAddWithoutASystemTempDir(t *testing.T) {
	// TMPDIR at a path that does not exist is what os.TempDir() answers with in
	// an image that never had one.
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "not-here"))

	root := t.TempDir()
	s := NewOsStores(root).Use("_")

	data := []byte("bytes a robot asked for")
	m, err := s.Add(t.Context(), Meta{}, strings.NewReader(string(data)))
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if m.Digest != DigestFromBytes(data) {
		t.Fatalf("digest: got %s", m.Digest)
	}
	if m.Size != int64(len(data)) {
		t.Fatalf("size: got %d want %d", m.Size, len(data))
	}

	if _, err := statMeta(t.Context(), s, m.Digest); err != nil {
		t.Fatalf("stat: %v", err)
	}
}
