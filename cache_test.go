package flob

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/lesomnus/flob/internal/x"
)

// nonDrainingStores is a primary that never holds the blob (Open/Stat always miss) and whose
// Add returns immediately WITHOUT reading the supplied reader — mimicking a primary that
// short-circuits because the blob was committed concurrently. It exercises the blobTap path
// where Primary.Add does not drain the tee pipe.
type nonDrainingStores struct{}

func (nonDrainingStores) Use(string) Store { return nonDrainingStore{} }

type nonDrainingStore struct{}

func (nonDrainingStore) Add(context.Context, Meta, io.Reader) (Meta, error) {
	return Meta{}, ErrAlreadyExists
}
func (nonDrainingStore) Stat(context.Context, Digest) (Info, error) { return nil, ErrNotExist }
func (nonDrainingStore) Open(context.Context, Digest) (io.ReadSeekCloser, Info, error) {
	return nil, nil, ErrNotExist
}
func (nonDrainingStore) Label(context.Context, Digest, Labels) error { return ErrNotExist }
func (nonDrainingStore) Erase(context.Context, Digest) error         { return nil }

func TestCacheStore(t *testing.T) {
	new_stores := func(t *testing.T) Stores {
		t.Helper()
		return &CacheStores{
			Primary: NewMemStores(),
			Origin:  NewMemStores(),
		}
	}
	new_store := func(t *testing.T) *CacheStore {
		t.Helper()
		stores := new_stores(t)
		return stores.Use("test").(*CacheStore)
	}

	t.Run("contract", func(t *testing.T) {
		testStore(t, func(t *testing.T) Stores {
			t.Helper()
			return &CacheStores{
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

		_, err = statMeta(ctx, s, m.Digest)
		x.NoError(err)

		_, err = statMeta(ctx, s.Origin, m.Digest)
		x.ErrorIs(err, ErrNotExist)
	})
	t.Run("stat from origin not cached", func(t *testing.T) {
		ctx, x := x.New(t)
		s := new_store(t)

		m, err := s.Origin.Add(ctx, Meta{}, x.Reader())
		x.NoError(err)

		_, err = statMeta(ctx, s, m.Digest)
		x.NoError(err)

		_, err = statMeta(ctx, s.Primary, m.Digest)
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

		_, err = statMeta(ctx, s.Primary, m.Digest)
		x.NoError(err)
	})
	t.Run("open does not deadlock when primary add short-circuits", func(t *testing.T) {
		ctx, x := x.New(t)

		origin := NewMemStores()
		m, err := origin.Use("t").Add(ctx, Meta{}, x.Reader())
		x.NoError(err)

		// Primary.Open misses (so Open taps the origin read), but Primary.Add returns
		// immediately without draining the tee pipe. Before the fix, blobTap.Read blocked
		// forever on the unbuffered pipe write and the caller's read hung.
		s := (&CacheStores{Primary: nonDrainingStores{}, Origin: origin}).Use("t")

		done := make(chan []byte, 1)
		go func() {
			r, _, err := s.Open(ctx, m.Digest)
			if err != nil {
				done <- nil
				return
			}
			data, _ := io.ReadAll(r)
			r.Close()
			done <- data
		}()

		select {
		case data := <-done:
			x.Eq(x.Data(), data)
		case <-time.After(2 * time.Second):
			t.Fatal("Open deadlocked: blobTap.Read blocked on a pipe the primary never drains")
		}
	})
	t.Run("add does not affect origin", func(t *testing.T) {
		ctx, x := x.New(t)
		s := new_store(t)

		m, err := s.Add(ctx, Meta{}, x.Reader())
		x.NoError(err)

		_, err = statMeta(ctx, s.Origin, m.Digest)
		x.ErrorIs(err, ErrNotExist)
	})
}
