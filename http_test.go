package flob

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

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

// rangeRoundTripper records or modifies real HTTP requests without replacing the
// streaming response body, so tests still exercise transport cancellation.
type rangeRoundTripper func(*http.Request) (*http.Response, error)

func (f rangeRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestHttpStoreLazyRanges(t *testing.T) {
	const content = "0123456789abcdefghijklmnopqrstuvwxyz"
	origin := NewMemStores()
	m, err := origin.Use("t").Add(t.Context(), Meta{}, strings.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var methods, ranges, conditions []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		methods = append(methods, r.Method)
		if r.Method == http.MethodGet {
			ranges = append(ranges, r.Header.Get("Range"))
			conditions = append(conditions, r.Header.Get("If-Match"))
		}
		mu.Unlock()
		HttpHandler{Stores: origin}.ServeHTTP(w, r)
	}))
	defer server.Close()
	store := HttpStores{Client: server.Client(), Target: server.URL}.Use("t")
	reader, info, err := store.Open(t.Context(), m.Digest)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if info.Size() != int64(len(content)) {
		t.Fatalf("size = %d", info.Size())
	}
	for _, seek := range []struct {
		offset int64
		whence int
	}{{0, io.SeekEnd}, {0, io.SeekStart}} {
		if _, err := reader.Seek(seek.offset, seek.whence); err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	if len(methods) != 1 || methods[0] != http.MethodHead {
		t.Errorf("Open/probes requests = %v", methods)
	}
	mu.Unlock()
	first := make([]byte, 4)
	if _, err := io.ReadFull(reader, first); err != nil || string(first) != content[:4] {
		t.Fatalf("first read = %q, %v", first, err)
	}
	if _, err := reader.Seek(5, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	rest, err := io.ReadAll(reader)
	if err != nil || string(rest) != content[5:] {
		t.Fatalf("seek read = %q, %v", rest, err)
	}
	if _, err := reader.Seek(-1, io.SeekStart); err == nil {
		t.Fatal("negative seek succeeded")
	}
	if _, err := reader.Seek(0, 99); err == nil {
		t.Fatal("invalid whence succeeded")
	}
	if _, err := reader.Seek(1<<63-1, io.SeekEnd); err == nil {
		t.Fatal("overflow seek succeeded")
	}
	if _, err := reader.Seek(10, io.SeekEnd); err != nil {
		t.Fatal(err)
	}
	if n, err := reader.Read(first); n != 0 || err != io.EOF {
		t.Fatalf("past-end read = %d, %v", n, err)
	}
	reader.Close()
	reader.Close()
	if _, err := reader.Read(first); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("closed read = %v", err)
	}
	if _, err := reader.Seek(0, io.SeekStart); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("closed seek = %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if fmt.Sprint(ranges) != "[bytes=0-35 bytes=5-35]" {
		t.Fatalf("ranges = %v", ranges)
	}
	if conditions[0] != "" || conditions[1] != `"`+string(m.Digest)+`"` {
		t.Fatalf("conditions = %v", conditions)
	}
}

func TestHttpStoreRangeValidation(t *testing.T) {
	for _, tc := range []struct {
		name, contentRange, length, encoding string
		status                               int
	}{
		{name: "missing range", status: 206, length: "4"},
		{name: "wrong start", status: 206, contentRange: "bytes 1-5/6", length: "5"},
		{name: "wrong total", status: 206, contentRange: "bytes 2-5/7", length: "4"},
		{name: "end beyond total", status: 206, contentRange: "bytes 2-6/6", length: "5"},
		{name: "reversed bounds", status: 206, contentRange: "bytes 5-2/6", length: "4"},
		{name: "unknown total", status: 206, contentRange: "bytes 2-5/*", length: "4"},
		{name: "overflow", status: 206, contentRange: "bytes 2-9223372036854775808/6", length: "4"},
		{name: "signed number", status: 206, contentRange: "bytes +2-5/6", length: "4"},
		{name: "length mismatch", status: 206, contentRange: "bytes 2-5/6", length: "3"},
		{name: "range ignored", status: 200, length: "6"},
		{name: "encoded", status: 206, contentRange: "bytes 2-5/6", length: "4", encoding: "gzip"},
		{name: "unsatisfiable", status: 416, length: "0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("ETag", `"version"`)
				if r.Method == http.MethodHead {
					w.Header().Set("Content-Length", "6")
					return
				}
				w.Header().Set("Content-Length", tc.length)
				if tc.contentRange != "" {
					w.Header().Set("Content-Range", tc.contentRange)
				}
				if tc.encoding != "" {
					w.Header().Set("Content-Encoding", tc.encoding)
				}
				w.WriteHeader(tc.status)
				w.Write([]byte("abcdef"))
			}))
			defer server.Close()
			store := HttpStores{Client: server.Client(), Target: server.URL}.Use("t")
			reader, _, err := store.Open(t.Context(), DigestFromBytes([]byte("abcdef")))
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			reader.Seek(2, io.SeekStart)
			if n, err := reader.Read(make([]byte, 4)); n != 0 || err == nil {
				t.Fatalf("invalid response read = %d, %v", n, err)
			}
		})
	}
}

func TestHttpStoreBoundedRanges(t *testing.T) {
	const content = "0123456789"
	var mu sync.Mutex
	var ranges []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"version"`)
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", "10")
			return
		}
		mu.Lock()
		ranges = append(ranges, r.Header.Get("Range"))
		mu.Unlock()
		var start, end int
		fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end)
		if end > start+2 {
			end = start + 2
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/10", start, end))
		w.Header().Set("Content-Length", strconv.Itoa(end-start+1))
		w.WriteHeader(206)
		io.WriteString(w, content[start:end+1])
	}))
	defer server.Close()
	reader, _, err := (HttpStores{Client: server.Client(), Target: server.URL}).Use("t").Open(t.Context(), DigestFromBytes([]byte(content)))
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	got, err := io.ReadAll(reader)
	if err != nil || string(got) != content {
		t.Fatalf("bounded read = %q, %v", got, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if fmt.Sprint(ranges) != "[bytes=0-9 bytes=3-9 bytes=6-9 bytes=9-9]" {
		t.Fatalf("ranges = %v", ranges)
	}
}

func TestHttpStoreRangeFallbackAndValidators(t *testing.T) {
	for _, mode := range []string{"no range", "no validator", "weak validator", "changed before first read", "changed after seek", "truncated", "overlong", "empty"} {
		t.Run(mode, func(t *testing.T) {
			const content = "abcdef"
			size := 6
			if mode == "empty" {
				size = 0
			}
			var gets int
			var mu sync.Mutex
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				etag := `"version"`
				if mode == "no validator" {
					etag = ""
				}
				if mode == "weak validator" {
					etag = `W/"version"`
				}
				if etag != "" {
					w.Header().Set("ETag", etag)
				}
				if r.Method == http.MethodHead {
					w.Header().Set("Content-Length", strconv.Itoa(size))
					return
				}
				mu.Lock()
				gets++
				current := gets
				mu.Unlock()
				if mode == "changed before first read" || (mode == "changed after seek" && current > 1) {
					w.Header().Set("ETag", `"replacement"`)
				}
				var start, end int
				fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end)
				if mode == "no range" {
					w.Header().Set("Content-Length", "6")
					io.WriteString(w, content)
					return
				}
				w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-5/6", start))
				if mode != "overlong" {
					w.Header().Set("Content-Length", strconv.Itoa(6-start))
				}
				w.WriteHeader(206)
				if mode == "overlong" {
					w.(http.Flusher).Flush()
					io.WriteString(w, content+"x")
					return
				}
				if mode == "truncated" {
					io.WriteString(w, content[:3])
					return
				}
				io.WriteString(w, content[start:])
			}))
			defer server.Close()
			reader, _, err := (HttpStores{Client: server.Client(), Target: server.URL}).Use("t").Open(t.Context(), DigestFromBytes([]byte(content)))
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			if mode == "empty" {
				if n, err := reader.Read(make([]byte, 1)); n != 0 || err != io.EOF {
					t.Fatalf("empty read = %d, %v", n, err)
				}
				mu.Lock()
				defer mu.Unlock()
				if gets != 0 {
					t.Fatal("empty blob issued GET")
				}
				return
			}
			if mode == "changed before first read" {
				if n, err := reader.Read(make([]byte, 1)); n != 0 || err == nil {
					t.Fatalf("changed initial read = %d, %v", n, err)
				}
				return
			}
			if mode == "truncated" || mode == "overlong" {
				if _, err := io.ReadAll(reader); err == nil {
					t.Fatal("malformed body accepted")
				}
				return
			}
			if mode == "no range" {
				got, err := io.ReadAll(reader)
				if err != nil || string(got) != content {
					t.Fatalf("sequential fallback = %q, %v", got, err)
				}
			} else {
				b := make([]byte, 2)
				if _, err := io.ReadFull(reader, b); err != nil {
					t.Fatal(err)
				}
			}
			reader.Seek(1, io.SeekStart)
			if n, err := reader.Read(make([]byte, 1)); n != 0 || err == nil {
				t.Fatalf("unsupported/changed reopened read = %d, %v", n, err)
			}
		})
	}
}

func TestHttpStoreLargeLazyReadAndCancellation(t *testing.T) {
	for _, mode := range []string{"cancel blocked read", "close blocked read", "close pending headers"} {
		t.Run(mode, func(t *testing.T) {
			started, finished := make(chan struct{}), make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("ETag", `"version"`)
				w.Header().Set("Content-Length", "2147483648")
				if r.Method == http.MethodHead {
					return
				}
				close(started)
				defer close(finished)
				if mode != "close pending headers" {
					w.Header().Set("Content-Range", "bytes 0-2147483647/2147483648")
					w.WriteHeader(206)
					io.WriteString(w, "data")
					w.(http.Flusher).Flush()
				}
				<-r.Context().Done()
			}))
			defer server.Close()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			reader, info, err := (HttpStores{Client: server.Client(), Target: server.URL}).Use("t").Open(ctx, DigestFromBytes([]byte("data")))
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			if info.Size() != 2147483648 {
				t.Fatalf("size = %d", info.Size())
			}
			done := make(chan error, 1)
			if mode != "close pending headers" {
				// The server never sends the remaining 2 GiB. A small Read must return now.
				go func() {
					b := make([]byte, 4)
					_, err := io.ReadFull(reader, b)
					if err == nil && string(b) != "data" {
						err = fmt.Errorf("first bytes %q", b)
					}
					done <- err
				}()
				select {
				case err := <-done:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("small read buffered the full blob")
				}
			}
			go func() { _, err := reader.Read(make([]byte, 1)); done <- err }()
			<-started
			if mode == "cancel blocked read" {
				cancel()
			} else {
				reader.Close()
			}
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("canceled read succeeded")
				}
			case <-time.After(3 * time.Second):
				t.Fatal("Close/cancellation did not unblock Read")
			}
			select {
			case <-finished:
			case <-time.After(3 * time.Second):
				t.Fatal("HTTP request was not canceled")
			}
		})
	}
}

func TestHttpStoreRedirectResourceScope(t *testing.T) {
	for _, mode := range []string{"different path", "different query", "renewed signature"} {
		t.Run(mode, func(t *testing.T) {
			var mu sync.Mutex
			gets := 0
			resourceGets := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasPrefix(r.URL.Path, "/t/") {
					if r.Method == http.MethodHead {
						w.Header().Set("Content-Length", "6")
						w.Header().Set("ETag", `"flob-digest"`)
						return
					}
					mu.Lock()
					gets++
					current := gets
					mu.Unlock()
					target := "/blob?version=one"
					if current > 1 && mode == "different path" {
						target = "/other?version=one"
					}
					if current > 1 && mode == "different query" {
						target = "/blob?version=two"
					}
					if mode == "renewed signature" {
						target = fmt.Sprintf("/blob?version=one&X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Signature=signature%d&X-Amz-Date=date%d", current, current)
					}
					if r.Header.Get("If-Match") != "" {
						http.Error(w, "final-resource validator leaked to redirect endpoint", 412)
						return
					}
					http.Redirect(w, r, target, 307)
					return
				}
				mu.Lock()
				resourceGets++
				resourceCurrent := resourceGets
				mu.Unlock()
				if resourceCurrent == 2 {
					target := "/other?version=one"
					if mode == "different query" {
						target = "/blob?version=two"
					}
					if mode == "renewed signature" {
						target = "/blob?version=one&X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Signature=changed"
					}
					http.Redirect(w, r, target, 307)
					return
				}
				w.Header().Set("ETag", `"same-tag"`)
				w.Header().Set("Content-Type", "application/octet-stream")
				http.ServeContent(w, r, "", time.Time{}, strings.NewReader("abcdef"))
			}))
			defer server.Close()
			reader, _, err := (HttpStores{Client: server.Client(), Target: server.URL}).Use("t").Open(t.Context(), DigestFromBytes([]byte("abcdef")))
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			if _, err := io.ReadFull(reader, make([]byte, 2)); err != nil {
				t.Fatal(err)
			}
			reader.Seek(3, io.SeekStart)
			got, err := io.ReadAll(reader)
			if err == nil || len(got) != 0 {
				t.Fatalf("changed resource read = %q, %v", got, err)
			}
		})
	}
}
