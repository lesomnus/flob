package flob

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lesomnus/flob/internal/x"
)

func newHttpStores(t *testing.T, backend Stores) (HttpStores, *httptest.Server) {
	t.Helper()
	s := httptest.NewServer(&HttpHandler{Stores: backend})
	t.Cleanup(s.Close)
	return HttpStores{Client: s.Client(), Target: s.URL}, s
}

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
	t.Run("contract over os backend", func(t *testing.T) {
		testStore(t, func(t *testing.T) Stores {
			t.Helper()

			h := &HttpHandler{Stores: NewOsStores(t.TempDir())}
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
	t.Run("digest mismatch maps to ErrDigestMismatch and status 422", func(t *testing.T) {
		ctx, x := x.New(t)
		stores, srv := newHttpStores(t, NewMemStores())

		// digest_nil is well-formed but does not match the content.
		_, err := stores.Use("t").Add(ctx, Meta{Digest: digest_nil}, x.Reader())
		x.ErrorIs(err, ErrDigestMismatch)

		req, err := http.NewRequest(http.MethodPost, srv.URL+"/t/"+string(digest_nil), bytes.NewReader(x.Data()))
		x.NoError(err)
		resp, err := srv.Client().Do(req)
		x.NoError(err)
		resp.Body.Close()
		x.Eq(http.StatusUnprocessableEntity, resp.StatusCode)
	})
	t.Run("open does not leak content-type as a label", func(t *testing.T) {
		ctx, x := x.New(t)
		stores, _ := newHttpStores(t, NewMemStores())

		m, err := stores.Use("t").Add(ctx, Meta{}, x.Reader())
		x.NoError(err)

		r, om, err := stores.Use("t").Open(ctx, m.Digest)
		x.NoError(err)
		r.Close()

		labels, err := om.Labels(ctx)
		x.NoError(err)
		if _, ok := labels["Content-Type"]; ok {
			t.Fatalf("Open leaked a transport Content-Type as a label: %v", labels)
		}
	})
	t.Run("add maps not-found to ErrNotExist", func(t *testing.T) {
		ctx, x := x.New(t)
		stores, _ := newHttpStores(t, FixedStores{Store: ErrorStore{Err: ErrNotExist}})

		_, err := stores.Use("t").Add(ctx, Meta{}, x.Reader())
		x.ErrorIs(err, ErrNotExist)
	})
	t.Run("post response advertises no phantom body length", func(t *testing.T) {
		// Regression: Add set Content-Length to the stored blob size on the
		// bodyless 201/200 response, so strict clients and reverse proxies wait
		// for bytes that never arrive (io.ReadAll -> unexpected EOF).
		_, x := x.New(t)
		_, srv := newHttpStores(t, NewMemStores())

		data := []byte("hello flob")
		post := func() *http.Response {
			t.Helper()
			req, err := http.NewRequest(http.MethodPost, srv.URL+"/t", bytes.NewReader(data))
			x.NoError(err)
			resp, err := srv.Client().Do(req)
			x.NoError(err)
			return resp
		}

		// First POST stores the blob (201 Created).
		resp := post()
		x.Eq(http.StatusCreated, resp.StatusCode)
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		x.NoError(err)
		x.Eq(0, len(body))
		if resp.ContentLength > 0 {
			t.Fatalf("201 advertised Content-Length %d but sends no body", resp.ContentLength)
		}

		// Second POST hits the already-exists path (200 OK), also bodyless.
		resp = post()
		x.Eq(http.StatusOK, resp.StatusCode)
		body, err = io.ReadAll(resp.Body)
		resp.Body.Close()
		x.NoError(err)
		x.Eq(0, len(body))
		if resp.ContentLength > 0 {
			t.Fatalf("200 advertised Content-Length %d but sends no body", resp.ContentLength)
		}
	})
}
