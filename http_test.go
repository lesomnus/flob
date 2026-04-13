package flob

import (
	"net/http"
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
	t.Run("target with prefix", func(t *testing.T) {
		testStore(t, func(t *testing.T) Stores {
			t.Helper()

			mux := http.NewServeMux()
			mux.Handle("/prefix/", http.StripPrefix("/prefix", &HttpHandler{Stores: NewMemStores()}))

			s := httptest.NewServer(mux)
			t.Cleanup(s.Close)

			return HttpStores{Client: s.Client(), Target: s.URL + "/prefix"}
		})
	})
}
