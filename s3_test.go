package flob

import (
	"context"
	"crypto/sha256"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lesomnus/flob/internal/x"
)

// mockS3 is a minimal in-memory S3-compatible server: enough of PUT/HEAD/GET/
// DELETE and ListObjectsV2 (path-style) to exercise the S3 store. It mimics S3's
// lowercasing of user metadata keys so the round-trip through the real HTTP client
// is faithful. Signatures are accepted without verification (the SigV4 algorithm
// is pinned separately by the AWS reference vectors in sigv4_test.go). PUT payload
// hashes are verified against the bytes received, as required by S3.
type mockS3 struct {
	bucket string

	mu      sync.Mutex
	objects map[string]mockObject // keyed by in-bucket key
}

type mockObject struct {
	data []byte
	meta map[string]string // lowercased x-amz-meta-* name -> value
}

func newMockS3(bucket string) *mockS3 {
	return &mockS3{bucket: bucket, objects: map[string]mockObject{}}
}

func (m *mockS3) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.objects)
}

func (m *mockS3) countPrefix(prefix string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for k := range m.objects {
		if strings.HasPrefix(k, prefix) {
			n++
		}
	}
	return n
}

func (m *mockS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Path-style: /{bucket}/{key...}; r.URL.Path is already percent-decoded.
	trimmed := strings.TrimPrefix(r.URL.Path, "/")
	bucket, key, _ := strings.Cut(trimmed, "/")
	if bucket != m.bucket {
		http.Error(w, "NoSuchBucket", http.StatusNotFound)
		return
	}

	if r.Method == http.MethodGet && r.URL.Query().Get("list-type") == "2" {
		m.list(w, r)
		return
	}

	switch r.Method {
	case http.MethodPut:
		m.put(w, r, key)
	case http.MethodHead:
		m.getOrHead(w, r, key, false)
	case http.MethodGet:
		m.getOrHead(w, r, key, true)
	case http.MethodDelete:
		m.mu.Lock()
		delete(m.objects, key)
		m.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "MethodNotAllowed", http.StatusMethodNotAllowed)
	}
}

func (m *mockS3) put(w http.ResponseWriter, r *http.Request, key string) {
	data, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if got, want := r.Header.Get("X-Amz-Content-Sha256"), fmt.Sprintf("%x", sha256.Sum256(data)); got != want {
		http.Error(w, "XAmzContentSHA256Mismatch", http.StatusBadRequest)
		return
	}
	meta := map[string]string{}
	for name, values := range r.Header {
		if lower := strings.ToLower(name); strings.HasPrefix(lower, "x-amz-meta-") {
			meta[lower] = strings.Join(values, ",")
		}
	}

	m.mu.Lock()
	if r.Header.Get("If-None-Match") == "*" {
		if _, ok := m.objects[key]; ok {
			m.mu.Unlock()
			http.Error(w, "PreconditionFailed", http.StatusPreconditionFailed)
			return
		}
	}
	m.objects[key] = mockObject{data: data, meta: meta}
	m.mu.Unlock()

	w.Header().Set("ETag", `"`+key+`"`)
	w.WriteHeader(http.StatusOK)
}

func (m *mockS3) getOrHead(w http.ResponseWriter, r *http.Request, key string, body bool) {
	m.mu.Lock()
	obj, ok := m.objects[key]
	m.mu.Unlock()
	if !ok {
		http.Error(w, "NoSuchKey", http.StatusNotFound)
		return
	}

	h := w.Header()
	for name, value := range obj.meta {
		// Write the lowercase key directly to mimic S3's on-wire casing; the Go
		// client canonicalizes it on receipt.
		h[name] = []string{value}
	}
	h.Set("Content-Length", strconv.Itoa(len(obj.data)))
	w.WriteHeader(http.StatusOK)
	if body {
		w.Write(obj.data)
	}
}

func (m *mockS3) list(w http.ResponseWriter, r *http.Request) {
	prefix := r.URL.Query().Get("prefix")
	maxKeys := 1000
	if v := r.URL.Query().Get("max-keys"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			maxKeys = n
		}
	}

	m.mu.Lock()
	var keys []string
	for k := range m.objects {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	m.mu.Unlock()
	sort.Strings(keys)
	if len(keys) > maxKeys {
		keys = keys[:maxKeys]
	}

	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>`)
	b.WriteString(`<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">`)
	fmt.Fprintf(&b, `<Name>%s</Name><Prefix>%s</Prefix><KeyCount>%d</KeyCount><MaxKeys>%d</MaxKeys><IsTruncated>false</IsTruncated>`,
		m.bucket, prefix, len(keys), maxKeys)
	for _, k := range keys {
		fmt.Fprintf(&b, `<Contents><Key>%s</Key></Contents>`, k)
	}
	b.WriteString(`</ListBucketResult>`)

	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	io.WriteString(w, b.String())
}

// newMockS3Stores wires an [S3Stores] to a fresh in-memory mock server.
func newMockS3Stores(t *testing.T) (*S3Stores, *mockS3) {
	t.Helper()
	mock := newMockS3("flob-test")
	srv := httptest.NewServer(mock)
	t.Cleanup(srv.Close)

	stores, err := NewS3Stores(S3Config{
		Endpoint:     srv.URL,
		Region:       "us-east-1",
		Bucket:       "flob-test",
		Credentials:  Credentials{AccessKeyID: "AKIAEXAMPLE", SecretAccessKey: "secretexample"},
		UsePathStyle: true,
		Client:       srv.Client(),
	})
	if err != nil {
		t.Fatalf("new s3 stores: %v", err)
	}
	return stores, mock
}

func TestS3Store(t *testing.T) {
	t.Run("contract", func(t *testing.T) {
		testStore(t, func(t *testing.T) Stores {
			t.Helper()
			stores, _ := newMockS3Stores(t)
			return stores
		})
	})

	t.Run("contract with prefix", func(t *testing.T) {
		testStore(t, func(t *testing.T) Stores {
			t.Helper()
			mock := newMockS3("flob-test")
			srv := httptest.NewServer(mock)
			t.Cleanup(srv.Close)
			stores, err := NewS3Stores(S3Config{
				Endpoint:     srv.URL,
				Bucket:       "flob-test",
				Prefix:       "some/prefix",
				Credentials:  Credentials{AccessKeyID: "AKIAEXAMPLE", SecretAccessKey: "secretexample"},
				UsePathStyle: true,
				Client:       srv.Client(),
			})
			if err != nil {
				t.Fatalf("new s3 stores: %v", err)
			}
			return stores
		})
	})

	t.Run("identical content across stores is stored once", func(t *testing.T) {
		ctx, x := x.New(t)
		stores, mock := newMockS3Stores(t)

		m1, err := stores.Use("a").Add(ctx, Meta{}, x.Reader())
		x.NoError(err)
		m2, err := stores.Use("b").Add(ctx, Meta{}, x.Reader())
		x.NoError(err)
		x.Eq(m1.Digest, m2.Digest)

		// One shared blob, two per-store reference markers.
		x.Eq(1, mock.countPrefix("blob/"))
		x.Eq(2, mock.countPrefix("refs/"))
	})

	t.Run("erase removes only the store's reference", func(t *testing.T) {
		ctx, x := x.New(t)
		stores, mock := newMockS3Stores(t)

		added, err := stores.Use("a").Add(ctx, Meta{}, x.Reader())
		x.NoError(err)
		_, err = stores.Use("b").Add(ctx, Meta{}, x.Reader())
		x.NoError(err)

		// a erases: only a's marker is removed; b's marker and the shared blob
		// remain, so b still sees the content.
		err = stores.Use("a").Erase(ctx, added.Digest)
		x.NoError(err)
		x.Eq(1, mock.countPrefix("refs/"))
		x.Eq(1, mock.countPrefix("blob/"))
		_, err = statMeta(ctx, stores.Use("b"), added.Digest)
		x.NoError(err)

		// b erases the last reference. The shared blob is intentionally retained
		// (reclamation is deferred to an out-of-band sweep) rather than deleted
		// inline, which could not be done without risking data loss. The content
		// is no longer visible from any store, but its bytes leak until GC.
		err = stores.Use("b").Erase(ctx, added.Digest)
		x.NoError(err)
		x.Eq(0, mock.countPrefix("refs/"))
		x.Eq(1, mock.countPrefix("blob/")) // retained, not reclaimed

		_, err = statMeta(ctx, stores.Use("b"), added.Digest)
		x.ErrorIs(err, ErrNotExist)

		// Re-adding reuses the retained blob and restores visibility.
		m2, err := stores.Use("b").Add(ctx, Meta{}, x.Reader())
		x.NoError(err)
		x.Eq(added.Digest, m2.Digest)
		x.Eq(1, mock.countPrefix("blob/"))
		x.Eq(1, mock.countPrefix("refs/"))
	})

	t.Run("erase during an in-flight add of the same digest keeps content readable", func(t *testing.T) {
		// Deterministic reproduction of the data-loss race that inline blob
		// reclamation caused. It drives the store's steps by hand to interleave
		// them precisely; the fixed Erase must not delete the shared blob.
		ctx, x := x.New(t)
		g, mock := newMockS3Stores(t)
		d := DigestFromBytes(x.Data())

		// Store "a" already holds the content (blob + ref).
		_, err := g.Use("a").Add(ctx, Meta{}, x.Reader())
		x.NoError(err)

		// Store "b" is mid-Add: it has decided to skip the upload because the
		// shared blob already exists, but has not yet written its marker.
		ok, err := g.exists(ctx, g.blobKey(d))
		x.NoError(err)
		x.Eq(true, ok)

		// "a" erases now. It was the sole reference, so inline reclamation would
		// delete the only blob copy here; the fixed Erase removes just a's marker.
		err = g.Use("a").Erase(ctx, d)
		x.NoError(err)

		// "b" finishes its Add by writing its marker.
		err = g.putRef(ctx, d, "b", nil, int64(len(x.Data())))
		x.NoError(err)

		// b's committed content must still be readable: no dangling reference.
		r, _, err := g.Use("b").Open(ctx, d)
		x.NoError(err)
		got, err := io.ReadAll(r)
		r.Close()
		x.NoError(err)
		x.Eq(d, DigestFromBytes(got))
		x.Eq(1, mock.countPrefix("blob/"))
	})

	t.Run("concurrent add/erase is race-free", func(t *testing.T) {
		ctx, x := x.New(t)
		stores, _ := newMockS3Stores(t)
		d := DigestFromBytes(x.Data())

		// General concurrency smoke test (run under -race): many stores hammer
		// the same digest with Add/Erase/Add. Each ends on a successful Add, so
		// each must be able to read the content back.
		const n = 32
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				s := stores.Use(fmt.Sprintf("s-%d", i))
				s.Add(ctx, Meta{}, x.Reader())
				s.Erase(ctx, d)
				s.Add(ctx, Meta{}, x.Reader())
			}(i)
		}
		wg.Wait()

		for i := 0; i < n; i++ {
			s := stores.Use(fmt.Sprintf("s-%d", i))
			if _, err := statMeta(ctx, s, d); err != nil {
				t.Fatalf("store %d Stat: %v", i, err)
			}
			r, _, err := s.Open(ctx, d)
			if err != nil {
				t.Fatalf("store %d Open (dangling reference = data loss): %v", i, err)
			}
			got, err := io.ReadAll(r)
			r.Close()
			x.NoError(err)
			x.Eq(d, DigestFromBytes(got))
		}
	})

	t.Run("labels survive round-trip via object metadata", func(t *testing.T) {
		ctx, x := x.New(t)
		stores, mock := newMockS3Stores(t)

		labels := Labels{"Media-Type": {"application/json"}, "Version": {"3"}}
		added, err := stores.Use("t").Add(ctx, Meta{Labels: labels}, x.Reader())
		x.NoError(err)

		// The labels really live in the marker's metadata, not a separate object.
		key := stores.refKey(added.Digest, "t")
		mock.mu.Lock()
		obj := mock.objects[key]
		mock.mu.Unlock()
		x.Eq("application/json", obj.meta["x-amz-meta-media-type"])
		x.Eq("3", obj.meta["x-amz-meta-version"])
		x.Contains(obj.meta, "x-amz-meta-flob-size")

		got, err := statMeta(ctx, stores.Use("t"), added.Digest)
		x.NoError(err)
		x.Eq("application/json", got.Labels.Get("Media-Type"))
		x.Eq("3", got.Labels.Get("Version"))
	})

	t.Run("virtual-hosted addressing builds bucket host", func(t *testing.T) {
		stores, err := NewS3Stores(S3Config{
			Region:      "us-west-2",
			Bucket:      "my-bucket",
			Credentials: Credentials{AccessKeyID: "k", SecretAccessKey: "s"},
		})
		if err != nil {
			t.Fatal(err)
		}
		req, err := stores.newRequest(t.Context(), http.MethodHead, stores.refKey(digest_nil, "id"), nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if req.URL.Host != "my-bucket.s3.us-west-2.amazonaws.com" {
			t.Fatalf("host=%q", req.URL.Host)
		}
		if !strings.HasPrefix(req.URL.Opaque, "/refs/") {
			t.Fatalf("opaque=%q", req.URL.Opaque)
		}
	})

	t.Run("presign is gated per store", func(t *testing.T) {
		ctx, x := x.New(t)
		stores, _ := newMockS3Stores(t)

		added, err := stores.Use("a").Add(ctx, Meta{}, x.Reader())
		x.NoError(err)

		// b never added the content: no presigned URL is issued.
		_, _, err = stores.Use("b").(Presigner).PresignOpen(ctx, added.Digest, time.Minute)
		x.ErrorIs(err, ErrNotExist)

		// a gets a signed URL that points at the shared blob object.
		loc, m, err := stores.Use("a").(Presigner).PresignOpen(ctx, added.Digest, time.Minute)
		x.NoError(err)
		x.Eq(added.Digest, m.Digest)
		if !strings.Contains(loc, "X-Amz-Signature=") || !strings.Contains(loc, "/blob/") {
			t.Fatalf("unexpected presigned url: %q", loc)
		}
	})

	t.Run("presign uses the public endpoint", func(t *testing.T) {
		ctx, x := x.New(t)
		mock := newMockS3("flob-test")
		srv := httptest.NewServer(mock)
		t.Cleanup(srv.Close)

		stores, err := NewS3Stores(S3Config{
			Endpoint:       srv.URL,
			PublicEndpoint: "https://cdn.example.com",
			Bucket:         "flob-test",
			Credentials:    Credentials{AccessKeyID: "k", SecretAccessKey: "s"},
			UsePathStyle:   true,
			Client:         srv.Client(),
		})
		x.NoError(err)

		added, err := stores.Use("t").Add(ctx, Meta{}, x.Reader())
		x.NoError(err)

		loc, _, err := stores.Use("t").(Presigner).PresignOpen(ctx, added.Digest, time.Minute)
		x.NoError(err)
		if !strings.HasPrefix(loc, "https://cdn.example.com/flob-test/blob/") {
			t.Fatalf("presign did not use public endpoint: %q", loc)
		}
	})

	t.Run("http handler redirects GET to a presigned url", func(t *testing.T) {
		ctx, x := x.New(t)
		stores, _ := newMockS3Stores(t)

		added, err := stores.Use("t").Add(ctx, Meta{Labels: Labels{"Media-Type": {"text/plain"}}}, x.Reader())
		x.NoError(err)

		fe := httptest.NewServer(HttpHandler{Stores: stores, Redirect: true})
		t.Cleanup(fe.Close)
		blobURL := fe.URL + "/t/" + string(added.Digest)

		// Inspect the redirect itself without following it.
		noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}}
		resp, err := noFollow.Get(blobURL)
		x.NoError(err)
		resp.Body.Close()
		x.Eq(http.StatusTemporaryRedirect, resp.StatusCode)
		x.Eq(`"`+string(added.Digest)+`"`, resp.Header.Get("ETag"))
		x.Eq("text/plain", resp.Header.Get("Media-Type"))
		x.Eq("no-store", resp.Header.Get("Cache-Control"))
		loc := resp.Header.Get("Location")
		if !strings.Contains(loc, "X-Amz-Signature=") || !strings.Contains(loc, "/blob/") {
			t.Fatalf("bad Location: %q", loc)
		}

		// Following it downloads the blob directly from (mock) S3.
		resp2, err := http.Get(blobURL)
		x.NoError(err)
		defer resp2.Body.Close()
		x.Eq(http.StatusOK, resp2.StatusCode)
		got, err := io.ReadAll(resp2.Body)
		x.NoError(err)
		x.Eq(x.Data(), got)
	})

	t.Run("redirect falls back to streaming for a non-presigner backend", func(t *testing.T) {
		ctx, x := x.New(t)
		mem := NewMemStores()
		m, err := mem.Use("t").Add(ctx, Meta{}, x.Reader())
		x.NoError(err)

		fe := httptest.NewServer(HttpHandler{Stores: mem, Redirect: true})
		t.Cleanup(fe.Close)

		resp, err := http.Get(fe.URL + "/t/" + string(m.Digest))
		x.NoError(err)
		defer resp.Body.Close()
		x.Eq(http.StatusOK, resp.StatusCode)
		got, err := io.ReadAll(resp.Body)
		x.NoError(err)
		x.Eq(x.Data(), got)
	})

	t.Run("AsPresigner sees through Unwrap decorators", func(t *testing.T) {
		stores, _ := newMockS3Stores(t)
		raw := stores.Use("t") // *S3Store implements Presigner

		if _, ok := AsPresigner(raw); !ok {
			t.Fatal("raw S3 store should be a Presigner")
		}
		// A decorator that embeds Store (so PresignOpen is not promoted) but
		// forwards Unwrap keeps the capability discoverable, even nested.
		if _, ok := AsPresigner(unwrapStore{raw}); !ok {
			t.Fatal("AsPresigner should see through an Unwrap decorator")
		}
		if _, ok := AsPresigner(unwrapStore{unwrapStore{raw}}); !ok {
			t.Fatal("AsPresigner should walk nested Unwrap decorators")
		}
		// A decorator that hides PresignOpen and does not implement Unwrap stops
		// the walk — the capability is genuinely gone.
		if _, ok := AsPresigner(hiddenStore{raw}); ok {
			t.Fatal("AsPresigner must not find a Presigner hidden without Unwrap")
		}
	})

	t.Run("redirect works when the store is behind a capability-hiding decorator", func(t *testing.T) {
		ctx, x := x.New(t)
		stores, _ := newMockS3Stores(t)
		added, err := stores.Use("t").Add(ctx, Meta{}, x.Reader())
		x.NoError(err)

		// Model the production wrapping: every Store handed to the handler is
		// wrapped in a decorator that embeds Store (hiding PresignOpen) but
		// forwards Unwrap.
		wrapped := wrapStores{inner: stores, wrap: func(s Store) Store { return unwrapStore{s} }}
		fe := httptest.NewServer(HttpHandler{Stores: wrapped, Redirect: true})
		t.Cleanup(fe.Close)

		noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}}
		resp, err := noFollow.Get(fe.URL + "/t/" + string(added.Digest))
		x.NoError(err)
		resp.Body.Close()
		x.Eq(http.StatusTemporaryRedirect, resp.StatusCode)
	})

	t.Run("parseEndpoint tolerates scheme-less host:port and ip:port", func(t *testing.T) {
		cases := []struct{ endpoint, scheme, host string }{
			{"localhost:9000", "https", "localhost:9000"},
			{"10.0.0.5:9000", "https", "10.0.0.5:9000"},
			{"192.168.1.10:9000", "https", "192.168.1.10:9000"},
			{"http://minio:9000", "http", "minio:9000"},
			{"https://s3.example.com", "https", "s3.example.com"},
		}
		for _, c := range cases {
			s, err := NewS3Stores(S3Config{
				Endpoint:    c.endpoint,
				Bucket:      "b",
				Credentials: Credentials{AccessKeyID: "k", SecretAccessKey: "s"},
			})
			if err != nil {
				t.Fatalf("endpoint %q: %v", c.endpoint, err)
			}
			if s.scheme != c.scheme || s.host != c.host {
				t.Errorf("endpoint %q -> scheme=%q host=%q, want %q/%q", c.endpoint, s.scheme, s.host, c.scheme, c.host)
			}
		}
	})
}

// wrapStores wraps every Store from inner with wrap, modeling the trace/metrics
// decorators the CLI applies.
type wrapStores struct {
	inner Stores
	wrap  func(Store) Store
}

func (w wrapStores) Use(id string) Store { return w.wrap(w.inner.Use(id)) }

// unwrapStore embeds Store (so it does NOT promote PresignOpen) but exposes the
// wrapped store via Unwrap — the pattern the real decorators use.
type unwrapStore struct{ Store }

func (u unwrapStore) Unwrap() Store { return u.Store }

// hiddenStore embeds Store and does NOT implement Unwrap, so it opaquely hides any
// optional capability of the store beneath it.
type hiddenStore struct{ Store }

func TestS3LinkOnlyTransfersReference(t *testing.T) {
	mock := newMockS3("bucket")
	var mu sync.Mutex
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		mu.Unlock()
		if r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/refs/") {
			data, err := io.ReadAll(r.Body)
			if err != nil || len(data) != 0 {
				t.Errorf("reference body = %q, %v", data, err)
			}
			r.Body = io.NopCloser(strings.NewReader(string(data)))
		}
		mock.ServeHTTP(w, r)
	}))
	defer srv.Close()
	stores, err := NewS3Stores(S3Config{Endpoint: srv.URL, Region: "us-east-1", Bucket: "bucket", UsePathStyle: true, Credentials: Credentials{AccessKeyID: "key", SecretAccessKey: "secret"}})
	if err != nil {
		t.Fatal(err)
	}
	source := stores.Use("source").(*S3Store)
	dest := stores.Use("dest").(*S3Store)
	added, err := source.Add(t.Context(), Meta{Labels: Labels{"Owner": {"source"}}}, strings.NewReader("content"))
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	calls = nil
	mu.Unlock()
	linked, err := dest.Link(t.Context(), added.Digest, AllowDuplicates(source))
	if err != nil {
		t.Fatal(err)
	}
	if linked.Digest != added.Digest || linked.Size != 7 || linked.Labels.Get("Owner") != "source" {
		t.Fatalf("Link Meta = %#v", linked)
	}
	mu.Lock()
	gotCalls := append([]string(nil), calls...)
	mu.Unlock()
	want := []string{"HEAD /bucket/" + stores.refKey(added.Digest, "source"), "PUT /bucket/" + stores.refKey(added.Digest, "dest")}
	if fmt.Sprint(gotCalls) != fmt.Sprint(want) {
		t.Fatalf("Link requests = %v; want %v", gotCalls, want)
	}
	linked.Labels["Owner"][0] = "mutated"
	if err := source.Erase(t.Context(), added.Digest); err != nil {
		t.Fatal(err)
	}
	r, info, err := dest.Open(t.Context(), added.Digest)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	data, err := io.ReadAll(r)
	if err != nil || string(data) != "content" {
		t.Fatalf("linked content = %q, %v", data, err)
	}
	labels, err := info.Labels(t.Context())
	if err != nil || labels.Get("Owner") != "source" {
		t.Fatalf("linked labels = %v, %v", labels, err)
	}
}

func TestS3LinkVisibilityAndDuplicates(t *testing.T) {
	stores, _ := newMockS3Stores(t)
	source := stores.Use("source").(*S3Store)
	dest := stores.Use("dest").(*S3Store)
	m, err := source.Add(t.Context(), Meta{Labels: Labels{"Owner": {"source"}}}, strings.NewReader("content"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dest.Link(t.Context(), m.Digest, stores.Use("missing")); !errors.Is(err, ErrNotExist) {
		t.Fatalf("missing source = %v", err)
	}
	if _, err := dest.Stat(t.Context(), m.Digest); !errors.Is(err, ErrNotExist) {
		t.Fatalf("missing source created target: %v", err)
	}
	const n = 12
	results := make(chan error, n)
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() { _, err := dest.Link(t.Context(), m.Digest, source); results <- err })
	}
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		} else if !errors.Is(err, ErrAlreadyExists) {
			t.Fatal(err)
		}
	}
	if successes != 1 {
		t.Fatalf("successful concurrent links = %d", successes)
	}
	if err := dest.Label(t.Context(), m.Digest, Labels{"Owner": {"dest"}}); err != nil {
		t.Fatal(err)
	}
	for _, from := range []Store{source, dest} {
		if _, err := dest.Link(t.Context(), m.Digest, from); !errors.Is(err, ErrAlreadyExists) {
			t.Fatalf("duplicate = %v", err)
		}
	}
	info, err := dest.Stat(t.Context(), m.Digest)
	if err != nil {
		t.Fatal(err)
	}
	labels, err := info.Labels(t.Context())
	if err != nil || labels.Get("Owner") != "dest" {
		t.Fatalf("duplicate replaced labels: %v, %v", labels, err)
	}
	if _, err := dest.Link(t.Context(), m.Digest, stores.Use("missing")); !errors.Is(err, ErrNotExist) {
		t.Fatalf("source missing priority = %v", err)
	}
	other, _ := newMockS3Stores(t)
	var nilSource *S3Store
	sameBackendDifferentPool := *stores
	for _, from := range []Store{nil, nilSource, NewMemStores().Use("source"), other.Use("source"), sameBackendDifferentPool.Use("source")} {
		if _, err := dest.Link(t.Context(), m.Digest, from); !errors.Is(err, ErrIncompatibleStore) {
			t.Fatalf("incompatible = %v", err)
		}
	}
	if _, err := dest.Link(t.Context(), "bad", source); err == nil {
		t.Fatal("invalid digest accepted")
	}
}

func TestS3LinkConditionalConflict(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodHead:
			w.Header().Set(metaPrefix+metaSizeKey, "7")
		case http.MethodPut:
			if r.Header.Get("If-None-Match") != "*" {
				t.Error("missing conditional create")
			}
			w.WriteHeader(http.StatusConflict)
		default:
			t.Errorf("unexpected %s", r.Method)
		}
	}))
	defer srv.Close()
	stores, err := NewS3Stores(S3Config{Endpoint: srv.URL, Region: "us-east-1", Bucket: "bucket", UsePathStyle: true, Credentials: Credentials{AccessKeyID: "key", SecretAccessKey: "secret"}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = stores.Use("dest").(*S3Store).Link(t.Context(), DigestFromBytes([]byte("content")), stores.Use("source"))
	if err == nil || errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("conditional conflict = %v", err)
	}
}

func walkS3Server(t *testing.T, handler http.HandlerFunc) *S3Stores {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	s, err := NewS3Stores(S3Config{Endpoint: srv.URL, Bucket: "bucket", Region: "us-east-1", UsePathStyle: true, Credentials: Credentials{AccessKeyID: "key", SecretAccessKey: "secret"}})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestS3WalkPages(t *testing.T) {
	d := DigestFromBytes([]byte("hello"))
	deleted := DigestFromBytes([]byte("deleted"))
	ref := func(d Digest, id string) string {
		return "refs/" + d.Algorithm().String() + "/" + d.Encoded() + "/" + namespaceSegment(id)
	}
	lists, heads := 0, 0
	s := walkS3Server(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			lists++
			if r.URL.Query().Get("list-type") != "2" || r.URL.Query().Get("encoding-type") != "url" {
				t.Error("not URL-encoded ListObjectsV2")
			}
			var keys []string
			next := ""
			switch r.URL.Query().Get("continuation-token") {
			case "":
				keys = []string{ref(d, "other"), ref(deleted, "a/b"), "refs/sha256/bad/a", "refs/sha256/" + d.Encoded() + "/~YQ"}
				next = "next+/=&"
			case "next+/=&":
				keys = []string{ref(d, "a/b")}
			default:
				t.Error("wrong token")
			}
			page := s3ListPage{EncodingType: "url", IsTruncated: next != "", NextContinuationToken: next}
			for _, key := range keys {
				page.Contents = append(page.Contents, struct{ Key string }{url.PathEscape(key)})
			}
			xml.NewEncoder(w).Encode(page)
		case http.MethodHead:
			heads++
			if strings.Contains(r.URL.Path, deleted.Encoded()) {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set(metaPrefix+metaSizeKey, "5")
			w.Header().Set(metaPrefix+"owner", "value")
		default:
			t.Errorf("unexpected blob I/O: %s %s", r.Method, r.URL)
		}
	})
	count := 0
	for info, err := range s.Use("a/b").(*S3Store).Walk(t.Context()) {
		if err != nil {
			t.Fatal(err)
		}
		count++
		if info.Digest() != d || info.Size() != 5 {
			t.Fatalf("Info = %v", info)
		}
		labels, err := info.Labels(t.Context())
		if err != nil || labels.Get("owner") != "value" {
			t.Fatalf("labels = %v, %v", labels, err)
		}
	}
	if count != 1 || lists != 2 || heads != 2 {
		t.Fatalf("count/list/head = %d/%d/%d", count, lists, heads)
	}
	lists, heads = 0, 0
	got := map[string]bool{}
	for id, err := range s.Namespaces(t.Context()) {
		if err != nil {
			t.Fatal(err)
		}
		got[id] = true
	}
	if len(got) != 2 || !got["a/b"] || !got["other"] || lists != 2 || heads != 0 {
		t.Fatalf("namespaces %v list/head %d/%d", got, lists, heads)
	}
}

func TestS3EnumerationStops(t *testing.T) {
	d := DigestFromBytes([]byte("hello"))
	key := "refs/sha256/" + d.Encoded() + "/a"
	for _, mode := range []string{"break", "cancel", "missing-token", "repeated-token", "bad-xml", "http-error", "bad-escape"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			s := walkS3Server(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				if mode == "http-error" {
					w.WriteHeader(503)
					return
				}
				if mode == "bad-xml" {
					fmt.Fprint(w, "<broken>")
					return
				}
				page := s3ListPage{IsTruncated: true, NextContinuationToken: "same"}
				if mode == "missing-token" {
					page.NextContinuationToken = ""
				}
				if mode == "bad-escape" {
					page.EncodingType = "url"
					page.Contents = append(page.Contents, struct{ Key string }{"%bad%"})
				} else {
					page.Contents = append(page.Contents, struct{ Key string }{key})
				}
				xml.NewEncoder(w).Encode(page)
			})
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			values, errs := 0, 0
			for _, err := range s.Namespaces(ctx) {
				if err != nil {
					errs++
					continue
				}
				values++
				if mode == "break" {
					break
				}
				if mode == "cancel" {
					cancel()
				}
			}
			if mode == "break" {
				if values != 1 || errs != 0 || calls != 1 {
					t.Fatalf("break: %d/%d/%d", values, errs, calls)
				}
			} else if errs != 1 {
				t.Fatalf("errors=%d calls=%d", errs, calls)
			}
			if mode == "cancel" && calls != 1 {
				t.Fatalf("I/O after cancel: %d", calls)
			}
			if mode == "repeated-token" && calls != 2 {
				t.Fatalf("token loop: %d", calls)
			}
		})
	}
}
