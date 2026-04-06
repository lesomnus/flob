package flat_test

import (
	"testing"

	"github.com/lesomnus/flat"
)

func TestOsStore(t *testing.T) {
	t.Run("Basic", func(t *testing.T) {
		testStore(t, func(t *testing.T) flat.Store {
			t.Helper()

			root := t.TempDir()
			stores := flat.NewOsStores(root)
			return stores.Use("test")
		})
	})
}
