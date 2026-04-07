package flat

import (
	"io"
	"testing"
	"time"

	"github.com/lesomnus/flat/internal/x"
)

func TestCacheStore(t *testing.T) {
	new_stores := func(t *testing.T) Stores {
		t.Helper()
		return CacheStores{
			Primary: NewMemStores(),
			Origin:  NewMemStores(),
		}
	}
	new_store := func(t *testing.T) CacheStore {
		t.Helper()
		stores := new_stores(t)
		return stores.Use("test").(CacheStore)
	}

	t.Run("contract", func(t *testing.T) {
		testStore(t, func(t *testing.T) Stores {
			t.Helper()
			return CacheStores{
				Primary: NewMemStores(),
				Origin:  NewMemStores(),
			}
		})
	})

	t.Run("primary only blob can be read", func(t *testing.T) {
		ctx, x := x.New(t)
		s := new_store(t)

		m, err := s.Primary.Add(ctx, Meta{}, x.Reader())
		x.NoError(err)

		_, err = s.Get(ctx, m.Digest)
		x.NoError(err)

		_, err = s.Origin.Get(ctx, m.Digest)
		x.ErrorIs(err, ErrNotExist)
	})
	t.Run("get from origin not cached", func(t *testing.T) {
		ctx, x := x.New(t)
		s := new_store(t)

		m, err := s.Origin.Add(ctx, Meta{}, x.Reader())
		x.NoError(err)

		_, err = s.Get(ctx, m.Digest)
		x.NoError(err)

		_, err = s.Primary.Get(ctx, m.Digest)
		x.ErrorIs(err, ErrNotExist)
	})
	t.Run("full read from origin makes cache", func(t *testing.T) {
		ctx, x := x.New(t)
		s := new_store(t)

		m, err := s.Origin.Add(ctx, Meta{}, x.Reader())
		x.NoError(err)

		r, _, err := s.Open(ctx, m.Digest)
		x.NoError(err)
		defer r.Close()

		_, err = io.Copy(io.Discard, r)
		x.NoError(err)

		// It may take some time for the blob to be cached in the primary store,
		// so we wait for a while before checking.
		time.Sleep(30 * time.Millisecond)

		_, err = s.Primary.Get(ctx, m.Digest)
		x.NoError(err)
	})
	t.Run("add does not affect origin", func(t *testing.T) {
		ctx, x := x.New(t)
		s := new_store(t)

		m, err := s.Add(ctx, Meta{}, x.Reader())
		x.NoError(err)

		_, err = s.Origin.Get(ctx, m.Digest)
		x.ErrorIs(err, ErrNotExist)
	})
}
