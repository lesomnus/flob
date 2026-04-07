package flob

import (
	"testing"
)

func TestMemStore(t *testing.T) {
	t.Run("contract", func(t *testing.T) {
		testStore(t, func(t *testing.T) Stores {
			t.Helper()
			return NewMemStores()
		})
	})
}
