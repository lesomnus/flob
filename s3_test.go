package flob

import (
	"bytes"
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
	"sync/atomic"
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

	mu         sync.Mutex
	objects    map[string]mockObject // keyed by in-bucket key
	uploads    map[string]*mockMultipart
	nextUpload int
}

type mockObject struct {
	data     []byte
	modified time.Time
	meta     map[string]string // lowercased x-amz-meta-* name -> value
}

func newMockS3(bucket string) *mockS3 {
	return &mockS3{bucket: bucket, objects: map[string]mockObject{}, uploads: map[string]*mockMultipart{}}
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

	if r.URL.Query().Has("uploads") || r.URL.Query().Has("uploadId") {
		m.multipart(w, r, key)
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
		if match := r.Header.Get("If-Match"); match != "" {
			obj, ok := m.objects[key]
			if !ok || match != fmt.Sprintf(`"%x"`, sha256.Sum256(obj.data)) {
				m.mu.Unlock()
				http.Error(w, "PreconditionFailed", 412)
				return
			}
		}
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
	if match := r.Header.Get("If-Match"); match != "" {
		obj, ok := m.objects[key]
		if !ok || match != fmt.Sprintf(`"%x"`, sha256.Sum256(obj.data)) {
			m.mu.Unlock()
			http.Error(w, "PreconditionFailed", 412)
			return
		}
	}
	m.objects[key] = mockObject{data: data, meta: meta, modified: time.Now()}
	m.mu.Unlock()

	w.Header().Set("ETag", fmt.Sprintf(`"%x"`, sha256.Sum256(data)))
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
	h.Set("ETag", fmt.Sprintf(`"%x"`, sha256.Sum256(obj.data)))
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Last-Modified", obj.modified.UTC().Format(http.TimeFormat))
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(obj.data))
}

func (m *mockS3) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	prefix, delimiter := q.Get("prefix"), q.Get("delimiter")
	maxKeys := 1000
	if v := q.Get("max-keys"); v != "" {
		if n, e := strconv.Atoi(v); e == nil {
			maxKeys = n
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	// Keys past the delimiter roll up into one common prefix, which pages like a key.
	var keys []string
	common := map[string]bool{}
	for key := range m.objects {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		if i := strings.Index(key[len(prefix):], delimiter); delimiter != "" && i >= 0 {
			rolled := key[:len(prefix)+i+len(delimiter)]
			if common[rolled] {
				continue
			}
			common[rolled] = true
			key = rolled
		}
		if key > q.Get("continuation-token") {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	page := struct {
		XMLName                             xml.Name `xml:"ListBucketResult"`
		Name, Prefix                        string
		KeyCount, MaxKeys                   int
		IsTruncated                         bool
		NextContinuationToken, EncodingType string
		Contents                            []s3ListedKey
		CommonPrefixes                      []struct{ Prefix string }
	}{Name: m.bucket, Prefix: prefix, MaxKeys: maxKeys, EncodingType: q.Get("encoding-type")}
	if len(keys) > maxKeys {
		page.IsTruncated = true
		keys = keys[:maxKeys]
		page.NextContinuationToken = keys[len(keys)-1]
	}
	for _, key := range keys {
		name := key
		if page.EncodingType == "url" {
			name = url.PathEscape(key)
		}
		if common[key] {
			page.CommonPrefixes = append(page.CommonPrefixes, struct{ Prefix string }{name})
			continue
		}
		page.Contents = append(page.Contents, s3ListedKey{Key: name, LastModified: m.objects[key].modified})
	}
	page.KeyCount = len(keys)
	w.Header().Set("Content-Type", "application/xml")
	xml.NewEncoder(w).Encode(page)
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
	erased := DigestFromBytes([]byte("erased"))
	listed := time.Date(2026, 9, 15, 12, 0, 0, 123e6, time.UTC)
	ref := func(d Digest) string {
		return "refs/~YS9i/" + d.Algorithm().String() + "/" + d.Encoded()
	}
	var lists []string
	heads := 0
	s := walkS3Server(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			q := r.URL.Query()
			if q.Get("list-type") != "2" || q.Get("encoding-type") != "url" {
				t.Error("not URL-encoded ListObjectsV2")
			}
			request := q.Get("prefix") + "|" + q.Get("delimiter") + "|" + q.Get("continuation-token")
			lists = append(lists, request)
			var keys, prefixes []string
			next := ""
			switch request {
			case "refs/~YS9i/||":
				keys = []string{ref(d), "refs/~YS9i/sha256/bad", ref(d) + "/extra"}
				next = "next+/=&"
			case "refs/~YS9i/||next+/=&":
				keys = []string{ref(erased)}
			case "refs/|/|":
				keys = []string{"refs/stray"}
				prefixes = []string{"refs/~YS9i/", "refs/other/", "refs/~YQ/"}
				next = "next+/=&"
			case "refs/|/|next+/=&":
				prefixes = []string{"refs/~/"}
			default:
				t.Errorf("unexpected list %q", request)
			}
			page := s3ListPage{EncodingType: "url", IsTruncated: next != "", NextContinuationToken: next}
			for _, key := range keys {
				page.Contents = append(page.Contents, s3ListedKey{Key: url.PathEscape(key), LastModified: listed})
			}
			for _, prefix := range prefixes {
				page.CommonPrefixes = append(page.CommonPrefixes, struct{ Prefix string }{url.PathEscape(prefix)})
			}
			xml.NewEncoder(w).Encode(page)
		case http.MethodHead:
			heads++
			if strings.Contains(r.URL.Path, erased.Encoded()) {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set(metaPrefix+metaSizeKey, "5")
			w.Header().Set(metaPrefix+"owner", "value")
		default:
			t.Errorf("unexpected blob I/O: %s %s", r.Method, r.URL)
		}
	})
	// The walk lists only its own namespace and skips malformed digest paths,
	// without a HEAD per reference.
	var infos []Info
	for info, err := range s.Use("a/b").(*S3Store).Walk(t.Context()) {
		if err != nil {
			t.Fatal(err)
		}
		infos = append(infos, info)
	}
	if len(infos) != 2 || infos[0].Digest() != d || infos[1].Digest() != erased || len(lists) != 2 || heads != 0 {
		t.Fatalf("walk = %d entries, lists %q, %d heads", len(infos), lists, heads)
	}
	// The listing's time needs no HEAD.
	if added, err := infos[0].Added(t.Context()); err != nil || !added.Equal(listed) || heads != 0 {
		t.Fatalf("listed Added = %v, %v (%d heads)", added, err, heads)
	}
	// Size and labels share one reference HEAD.
	if size := mustSize(t, infos[0]); size != 5 {
		t.Fatalf("size = %d", size)
	}
	labels, err := infos[0].Labels(t.Context())
	if err != nil || labels.Get("owner") != "value" || heads != 1 {
		t.Fatalf("labels = %v, %v (%d heads)", labels, err, heads)
	}
	// A reference erased after the listing is reported when used.
	if _, err := infos[1].Size(t.Context()); !errors.Is(err, ErrNotExist) {
		t.Fatalf("erased size = %v", err)
	}
	lists, heads = nil, 0
	got := map[string]bool{}
	for id, err := range s.Namespaces(t.Context()) {
		if err != nil {
			t.Fatal(err)
		}
		got[id] = true
	}
	if len(got) != 3 || !got["a/b"] || !got["other"] || !got[""] || len(lists) != 2 || heads != 0 {
		t.Fatalf("namespaces %v, lists %q, %d heads", got, lists, heads)
	}
}

func TestS3EnumerationStops(t *testing.T) {
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
					page.CommonPrefixes = append(page.CommonPrefixes, struct{ Prefix string }{"%bad%"})
				} else {
					page.CommonPrefixes = append(page.CommonPrefixes, struct{ Prefix string }{"refs/a/"})
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

func TestS3StoreLazyRanges(t *testing.T) {
	stores, _ := newMockS3Stores(t)
	store := stores.Use("t")
	const content = "0123456789abcdefghijklmnopqrstuvwxyz"
	m, err := store.Add(t.Context(), Meta{}, strings.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var methods, ranges, conditions []string
	transport := stores.cl.Transport
	stores.cl.Transport = rangeRoundTripper(func(req *http.Request) (*http.Response, error) {
		mu.Lock()
		methods = append(methods, req.Method)
		if req.Method == http.MethodGet {
			ranges = append(ranges, req.Header.Get("Range"))
			conditions = append(conditions, req.Header.Get("If-Match"))
		}
		mu.Unlock()
		return transport.RoundTrip(req)
	})
	reader, info, err := store.Open(t.Context(), m.Digest)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if size := mustSize(t, info); size != int64(len(content)) {
		t.Fatalf("size = %d", size)
	}
	reader.Seek(0, io.SeekEnd)
	reader.Seek(0, io.SeekStart)
	mu.Lock()
	if fmt.Sprint(methods) != "[HEAD HEAD]" {
		t.Errorf("Open/probes requests = %v", methods)
	}
	mu.Unlock()
	b := make([]byte, 4)
	if _, err := io.ReadFull(reader, b); err != nil || string(b) != content[:4] {
		t.Fatalf("first bytes = %q, %v", b, err)
	}
	reader.Seek(-5, io.SeekEnd)
	got, err := io.ReadAll(reader)
	if err != nil || string(got) != content[len(content)-5:] {
		t.Fatalf("tail = %q, %v", got, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if fmt.Sprint(ranges) != "[bytes=0-35 bytes=31-35]" {
		t.Fatalf("ranges = %v", ranges)
	}
	etag := fmt.Sprintf(`"%x"`, sha256.Sum256([]byte(content)))
	if len(conditions) != 2 || conditions[0] != etag || conditions[1] != etag {
		t.Fatalf("If-Match = %v", conditions)
	}
}

func TestS3StoreLazyMissingAndReplacement(t *testing.T) {
	for _, mode := range []string{"missing before Open", "missing before Read", "replaced before Read", "size mismatch", "empty"} {
		t.Run(mode, func(t *testing.T) {
			stores, mock := newMockS3Stores(t)
			store := stores.Use("t")
			content := "content"
			if mode == "empty" {
				content = ""
			}
			m, err := store.Add(t.Context(), Meta{}, strings.NewReader(content))
			if err != nil {
				t.Fatal(err)
			}
			key := stores.blobKey(m.Digest)
			if mode == "missing before Open" {
				mock.mu.Lock()
				delete(mock.objects, key)
				mock.mu.Unlock()
			}
			if mode == "size mismatch" {
				mock.mu.Lock()
				obj := mock.objects[key]
				obj.data = []byte("x")
				mock.objects[key] = obj
				mock.mu.Unlock()
			}
			reader, _, err := store.Open(t.Context(), m.Digest)
			if mode == "missing before Open" {
				if !errors.Is(err, ErrNotExist) {
					t.Fatalf("Open = %v", err)
				}
				return
			}
			if mode == "size mismatch" {
				if err == nil {
					reader.Close()
					t.Fatal("size mismatch accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			if mode == "missing before Read" {
				mock.mu.Lock()
				delete(mock.objects, key)
				mock.mu.Unlock()
			}
			if mode == "replaced before Read" {
				mock.mu.Lock()
				obj := mock.objects[key]
				obj.data = []byte("changed")
				mock.objects[key] = obj
				mock.mu.Unlock()
			}
			got, err := io.ReadAll(reader)
			if mode == "empty" {
				if err != nil || len(got) != 0 {
					t.Fatalf("empty read = %q, %v", got, err)
				}
				return
			}
			if mode == "missing before Read" && !errors.Is(err, ErrNotExist) {
				t.Fatalf("Read = %v; want ErrNotExist", err)
			}
			if err == nil || len(got) != 0 {
				t.Fatalf("changed/missing read = %q, %v", got, err)
			}
		})
	}
}

func TestHttpStoreLazyPresignedS3Redirect(t *testing.T) {
	stores, _ := newMockS3Stores(t)
	var now atomic.Int64
	now.Store(time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC).Unix())
	stores.signer.now = func() time.Time { return time.Unix(now.Load(), 0) }
	const content = "0123456789abcdefghijklmnopqrstuvwxyz"
	m, err := stores.Use("t").Add(t.Context(), Meta{}, strings.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(HttpHandler{Stores: stores, Redirect: true})
	defer server.Close()
	reader, info, err := (HttpStores{Client: server.Client(), Target: server.URL}).Use("t").Open(t.Context(), m.Digest)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if info.Digest() != m.Digest {
		t.Fatalf("digest = %s", info.Digest())
	}
	first := make([]byte, 4)
	if _, err := io.ReadFull(reader, first); err != nil || string(first) != content[:4] {
		t.Fatalf("first = %q, %v", first, err)
	}
	// A fresh redirect would now have a different signed query. The reader
	// must reuse its original final URL instead of minting another one.
	now.Add(2)
	reader.Seek(7, io.SeekStart)
	got, err := io.ReadAll(reader)
	if err != nil || string(got) != content[7:] {
		t.Fatalf("redirected seek = %q, %v", got, err)
	}
}

type mockMultipart struct {
	Key       string
	Initiated time.Time
	Parts     map[int][]byte
}

func (m *mockS3) multipart(w http.ResponseWriter, r *http.Request, key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	q := r.URL.Query()
	id := q.Get("uploadId")
	if q.Has("uploads") {
		if r.Method == http.MethodPost {
			m.nextUpload++
			id = strconv.Itoa(m.nextUpload)
			m.uploads[id] = &mockMultipart{Key: key, Initiated: time.Now(), Parts: map[int][]byte{}}
			fmt.Fprintf(w, "<InitiateMultipartUploadResult><UploadId>%s</UploadId></InitiateMultipartUploadResult>", id)
			return
		}
		fmt.Fprint(w, "<ListMultipartUploadsResult>")
		for id, up := range m.uploads {
			if strings.HasPrefix(up.Key, q.Get("prefix")) {
				fmt.Fprintf(w, "<Upload><Key>%s</Key><UploadId>%s</UploadId><Initiated>%s</Initiated></Upload>", up.Key, id, up.Initiated.Format(time.RFC3339Nano))
			}
		}
		fmt.Fprint(w, "<IsTruncated>false</IsTruncated></ListMultipartUploadsResult>")
		return
	}
	up, ok := m.uploads[id]
	if !ok || up.Key != key {
		http.Error(w, "NoSuchUpload", 404)
		return
	}
	switch r.Method {
	case http.MethodDelete:
		delete(m.uploads, id)
		w.WriteHeader(204)
	case http.MethodPut:
		source, err := url.PathUnescape(r.Header.Get("X-Amz-Copy-Source"))
		if err != nil {
			http.Error(w, "bad copy", 400)
			return
		}
		source = strings.TrimPrefix(source, "/"+m.bucket+"/")
		obj, ok := m.objects[source]
		if !ok {
			http.Error(w, "NoSuchKey", 404)
			return
		}
		number, err := strconv.Atoi(q.Get("partNumber"))
		if err != nil || number < 1 || number > 10000 {
			http.Error(w, "bad part", 400)
			return
		}
		up.Parts[number] = append([]byte(nil), obj.data...)
		fmt.Fprintf(w, `<CopyPartResult><ETag>"%x"</ETag></CopyPartResult>`, sha256.Sum256(obj.data))
	case http.MethodPost:
		var complete struct {
			Parts []struct {
				Number int `xml:"PartNumber"`
				ETag   string
			} `xml:"Part"`
		}
		if err := xml.NewDecoder(r.Body).Decode(&complete); err != nil {
			http.Error(w, "bad complete", 400)
			return
		}
		var data []byte
		for i, p := range complete.Parts {
			b, ok := up.Parts[p.Number]
			if !ok || p.ETag != fmt.Sprintf(`"%x"`, sha256.Sum256(b)) {
				http.Error(w, "InvalidPart", 400)
				return
			}
			if i < len(complete.Parts)-1 && len(b) < 5<<20 {
				http.Error(w, "EntityTooSmall", 400)
				return
			}
			data = append(data, b...)
		}
		m.objects[key] = mockObject{data: data, meta: map[string]string{}, modified: time.Now()}
		delete(m.uploads, id)
		fmt.Fprint(w, "<CompleteMultipartUploadResult/>")
	default:
		http.Error(w, "bad method", 405)
	}
}

func TestS3StageChunkResumeAndServerCopy(t *testing.T) {
	g, mock := newMockS3Stores(t)
	g.stagePartSize = 5 << 20
	st, err := g.Use("a/b").(*S3Store).Begin(t.Context(), Canonical)
	if err != nil {
		t.Fatal(err)
	}
	part := bytes.Repeat([]byte("x"), 5<<20)
	if n, err := st.Append(t.Context(), 0, bytes.NewReader(part[:123])); err != nil || n != 123 {
		t.Fatalf("append = %d,%v", n, err)
	}
	clone := *g
	resumed, err := clone.Use("a/b").(*S3Store).Resume(t.Context(), st.ID())
	if err != nil {
		t.Fatal(err)
	}
	if n, err := resumed.Append(t.Context(), 123, bytes.NewReader(part)); err != nil || n != int64(len(part)+123) {
		t.Fatalf("append resumed = %d,%v", n, err)
	}
	var gets []string
	transport := g.cl.Transport
	g.cl.Transport = rangeRoundTripper(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodGet {
			gets = append(gets, r.URL.Opaque)
		}
		return transport.RoundTrip(r)
	})
	result, err := resumed.Commit(t.Context(), Meta{Labels: Labels{"Owner": {"stage"}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range gets {
		if !strings.HasSuffix(path, "/manifest") {
			t.Fatalf("commit downloaded data: %s", path)
		}
	}
	expected := append(append([]byte(nil), part[:123]...), part...)
	if result.Digest != DigestFromBytes(expected) || result.Size != int64(len(expected)) {
		t.Fatalf("result = %#v", result)
	}
	mock.mu.Lock()
	got := append([]byte(nil), mock.objects[g.blobKey(result.Digest)].data...)
	left := len(mock.uploads)
	mock.mu.Unlock()
	if !bytes.Equal(got, expected) || left != 0 {
		t.Fatalf("assembled len=%d uploads=%d", len(got), left)
	}
}

func TestS3StageCommitRecoveryAndPruneMPU(t *testing.T) {
	g, mock := newMockS3Stores(t)
	st, err := g.Use("a").(*S3Store).Begin(t.Context(), Canonical)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Append(t.Context(), 0, strings.NewReader("content")); err != nil {
		t.Fatal(err)
	}
	transport := g.cl.Transport
	fail := true
	g.cl.Transport = rangeRoundTripper(func(r *http.Request) (*http.Response, error) {
		if fail && r.Method == http.MethodPut && strings.Contains(r.URL.Opaque, "/refs/") {
			fail = false
			return &http.Response{StatusCode: 503, Status: "503 Service Unavailable", Header: http.Header{}, Body: io.NopCloser(strings.NewReader("injected")), Request: r}, nil
		}
		return transport.RoundTrip(r)
	})
	wanted := Meta{Labels: Labels{"Owner": {"frozen"}}}
	if _, err := st.Commit(t.Context(), wanted); err == nil {
		t.Fatal("injected reference failure ignored")
	}
	info, err := st.Stat(t.Context())
	if err != nil || info.State != StageCommitting {
		t.Fatalf("pending = %#v,%v", info, err)
	}
	if _, err := st.Append(t.Context(), 7, strings.NewReader("bad")); !errors.Is(err, ErrStageClosed) {
		t.Fatalf("append pending = %v", err)
	}
	if _, err := st.Commit(t.Context(), Meta{Labels: Labels{"Owner": {"different"}}}); !errors.Is(err, ErrStageConflict) {
		t.Fatalf("changed retry = %v", err)
	}
	if _, err := g.PruneStages(t.Context()); err != nil {
		t.Fatal(err)
	}
	result, err := st.Commit(t.Context(), wanted)
	if err != nil || result.Labels.Get("Owner") != "frozen" {
		t.Fatalf("recovered = %#v,%v", result, err)
	}
	old := time.Now().Add(-48 * time.Hour)
	mock.mu.Lock()
	mock.uploads["abandoned"] = &mockMultipart{Key: g.blobKey(DigestFromBytes([]byte("abandoned"))), Initiated: old}
	mock.uploads["recent"] = &mockMultipart{Key: g.blobKey(DigestFromBytes([]byte("recent"))), Initiated: time.Now()}
	mock.mu.Unlock()
	if _, err := g.PruneStages(t.Context()); err != nil {
		t.Fatal(err)
	}
	mock.mu.Lock()
	_, oldExists := mock.uploads["abandoned"]
	_, newExists := mock.uploads["recent"]
	mock.mu.Unlock()
	if oldExists || !newExists {
		t.Fatalf("MPU prune old=%v new=%v", oldExists, newExists)
	}
}

func TestS3StageLeaseAndFencing(t *testing.T) {
	g, _ := newMockS3Stores(t)
	now := time.Now()
	g.stage.now = func() time.Time { return now }
	g.stage.OperationTimeout = time.Minute
	stage, err := g.Use("a").(*S3Store).Begin(t.Context(), Canonical)
	if err != nil {
		t.Fatal(err)
	}
	st := stage.(*s3Stage)
	old, oldTag, err := st.acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Append(t.Context(), 0, strings.NewReader("blocked")); !errors.Is(err, ErrStageConflict) {
		t.Fatalf("lease = %v", err)
	}
	now = now.Add(2 * time.Minute)
	if _, err := st.Append(t.Context(), 0, strings.NewReader("winner")); err != nil {
		t.Fatal(err)
	}
	if err := st.release(t.Context(), old, oldTag); !errors.Is(err, ErrStageConflict) {
		t.Fatalf("stale writer = %v", err)
	}
	result, err := st.Commit(t.Context(), Meta{})
	if err != nil || result.Digest != DigestFromBytes([]byte("winner")) {
		t.Fatalf("fenced commit = %#v,%v", result, err)
	}
}

type s3StageTruncatedReader struct{ data []byte }

func (r *s3StageTruncatedReader) Read(p []byte) (int, error) {
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, io.ErrUnexpectedEOF
}
func TestS3StageTruncatedAppendIsAtomic(t *testing.T) {
	for _, size := range []int{123, 5 << 20} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			g, _ := newMockS3Stores(t)
			g.stagePartSize = 5 << 20
			st, err := g.Use("a").(*S3Store).Begin(t.Context(), Canonical)
			if err != nil {
				t.Fatal(err)
			}
			before, err := st.Stat(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := st.Append(t.Context(), 0, &s3StageTruncatedReader{data: bytes.Repeat([]byte("x"), size)}); !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("truncated input = %v", err)
			}
			after, err := st.Stat(t.Context())
			if err != nil || after.Offset != 0 || !after.ExpiresAt.Equal(before.ExpiresAt) {
				t.Fatalf("failed append state %#v,%v", after, err)
			}
		})
	}
}

func TestS3StagePruneProtectsFailedCommittingUpload(t *testing.T) {
	g, mock := newMockS3Stores(t)
	st, err := g.Use("a").(*S3Store).Begin(t.Context(), Canonical)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Append(t.Context(), 0, strings.NewReader("content")); err != nil {
		t.Fatal(err)
	}
	transport := g.cl.Transport
	g.cl.Transport = rangeRoundTripper(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodPut && r.URL.Query().Has("partNumber") {
			return &http.Response{StatusCode: 503, Status: "503 unavailable", Header: http.Header{}, Body: io.NopCloser(strings.NewReader("injected")), Request: r}, nil
		}
		return transport.RoundTrip(r)
	})
	if _, err := st.Commit(t.Context(), Meta{}); err == nil {
		t.Fatal("copy failure ignored")
	}
	mock.mu.Lock()
	for _, upload := range mock.uploads {
		upload.Initiated = time.Now().Add(-72 * time.Hour)
	}
	mock.mu.Unlock()
	if _, err := g.PruneStages(t.Context()); err == nil {
		t.Fatal("failed recovery not reported")
	}
	mock.mu.Lock()
	count := len(mock.uploads)
	mock.mu.Unlock()
	if count != 1 {
		t.Fatalf("protected upload count = %d", count)
	}
	g.cl.Transport = transport
	if _, err := st.Commit(t.Context(), Meta{}); err != nil {
		t.Fatal(err)
	}
}

func TestS3StagePruneLiveOrphansAndTerminalData(t *testing.T) {
	g, mock := newMockS3Stores(t)
	st, err := g.Use("a").(*S3Store).Begin(t.Context(), Canonical)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Append(t.Context(), 0, strings.NewReader("one")); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Append(t.Context(), 3, strings.NewReader("two")); err != nil {
		t.Fatal(err)
	}
	prefix := st.(*s3Stage).prefix() + "chunks/"
	mock.mu.Lock()
	for key, obj := range mock.objects {
		if strings.HasPrefix(key, prefix) {
			obj.modified = time.Now().Add(-time.Hour)
			mock.objects[key] = obj
		}
	}
	mock.mu.Unlock()
	if n, err := g.PruneStages(t.Context()); err != nil || n != 0 {
		t.Fatalf("live prune = %d,%v", n, err)
	}
	if count := mock.countPrefix(prefix); count != 1 {
		t.Fatalf("live chunks=%d", count)
	}
	if _, err := st.Commit(t.Context(), Meta{}); err != nil {
		t.Fatal(err)
	}
	if n, err := g.PruneStages(t.Context()); err != nil || n != 0 {
		t.Fatalf("terminal prune=%d,%v", n, err)
	}
	if count := mock.countPrefix(prefix); count != 0 {
		t.Fatalf("terminal chunks=%d", count)
	}
	if _, err := st.Commit(t.Context(), Meta{}); err != nil {
		t.Fatalf("receipt retry after cleanup=%v", err)
	}
	aborted, err := g.Use("a").(*S3Store).Begin(t.Context(), Canonical)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := aborted.Append(t.Context(), 0, strings.NewReader("abort")); err != nil {
		t.Fatal(err)
	}
	if err := aborted.Abort(t.Context()); err != nil {
		t.Fatal(err)
	}
	if count := mock.countPrefix(aborted.(*s3Stage).prefix() + "chunks/"); count != 0 {
		t.Fatalf("aborted chunks=%d", count)
	}
}

func TestS3StageReadAndBeginOperationTimeout(t *testing.T) {
	g, _ := newMockS3Stores(t)
	store := g.Use("a").(*S3Store)
	st, err := store.Begin(t.Context(), Canonical)
	if err != nil {
		t.Fatal(err)
	}
	g.stage.OperationTimeout = 20 * time.Millisecond
	g.cl.Transport = rangeRoundTripper(func(r *http.Request) (*http.Response, error) { <-r.Context().Done(); return nil, r.Context().Err() })
	if _, err := store.Begin(t.Context(), Canonical); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Begin timeout = %v", err)
	}
	if _, err := store.Resume(t.Context(), st.ID()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Resume timeout = %v", err)
	}
	if _, err := st.Stat(t.Context()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Stat timeout = %v", err)
	}
}
