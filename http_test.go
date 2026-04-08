package flob

import (
	"net/http/httptest"
	"testing"
)

func TestHttpStore(t *testing.T) {
	t.Run("contract", func(t *testing.T) {
		testStore(t, func(t *testing.T) Stores {
			t.Helper()

			h := &HttpHandler{Stores: NewMemStores()}
			s := httptest.NewServer(h)
			t.Cleanup(s.Close)

			return HttpStores{Client: s.Client(), Target: s.URL}
		})
	})
}
