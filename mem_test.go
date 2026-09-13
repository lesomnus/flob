package flob

import (
	"fmt"
	"sync"
	"testing"

	"github.com/lesomnus/flob/internal/x"
)

func TestMemStore(t *testing.T) {
	t.Run("contract", func(t *testing.T) {
		testStore(t, func(t *testing.T) Stores {
			t.Helper()
			return NewMemStores()
		})
	})

	t.Run("concurrent add and erase keeps refcount consistent", func(t *testing.T) {
		ctx, x := x.New(t)
		stores := NewMemStores()

		d := DigestFromBytes(x.Data())

		const n = 24
		var wg sync.WaitGroup
		for i := range n {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				id := fmt.Sprintf("s-%d", i)
				stores.Use(id).Add(ctx, Meta{}, x.Reader())
				stores.Use(id).Erase(ctx, d)
			}(i)
		}
		wg.Wait()

		// After every store erased its reference, nothing sees the blob.
		for i := range n {
			_, err := statMeta(ctx, stores.Use(fmt.Sprintf("s-%d", i)), d)
			x.ErrorIs(err, ErrNotExist)
		}

		// A fresh add still works: the global blob entry was cleaned up, not corrupted or
		// left dangling with a stale refcount.
		m, err := stores.Use("fresh").Add(ctx, Meta{}, x.Reader())
		x.NoError(err)
		x.Eq(d, m.Digest)
	})
}
