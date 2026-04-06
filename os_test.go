package flat

import (
	"os"
	"testing"

	"github.com/lesomnus/flat/internal/x"
)

func TestOsStore(t *testing.T) {
	new_store := func(t *testing.T) Store {
		t.Helper()

		root := t.TempDir()
		stores := NewOsStores(root)
		return stores.Use("test")
	}

	path_to_blob := func(s Store, d Digest) string {
		return s.(OsStore).pathToBlob(d)
	}

	t.Run("contract", func(t *testing.T) {
		testStore(t, new_store)
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
		_, err = s1.Get(ctx, m.Digest)
		x.ErrorIs(err, ErrNotExist)

		// s2 must still see the blob.
		_, err = s2.Get(ctx, m.Digest)
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
}
