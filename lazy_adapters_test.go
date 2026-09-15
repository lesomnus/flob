package flob

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type lazyAdapterStore struct {
	UnimplementedStore
	info Info
	body io.ReadSeekCloser
}

func (s lazyAdapterStore) Stat(context.Context, Digest) (Info, error) { return s.info, nil }
func (s lazyAdapterStore) Open(context.Context, Digest) (io.ReadSeekCloser, Info, error) {
	return s.body, s.info, nil
}

type closeRecordingReader struct {
	*strings.Reader
	closed bool
}

func (r *closeRecordingReader) Close() error { r.closed = true; return nil }

type cacheWriteRecorder struct {
	ErrorStore
	calls atomic.Int32
}

func (s *cacheWriteRecorder) Add(context.Context, Meta, io.Reader) (Meta, error) {
	s.calls.Add(1)
	return Meta{}, errors.New("unexpected cache write")
}

func TestHttpLazyLabelsFailure(t *testing.T) {
	for _, method := range []string{http.MethodHead, http.MethodGet} {
		t.Run(method, func(t *testing.T) {
			const content = "content"
			d := DigestFromBytes([]byte(content))
			body := &closeRecordingReader{Reader: strings.NewReader(content)}
			info := NewInfo(d, int64(len(content)), time.Time{}, func(context.Context) (Labels, error) { return nil, errors.New("labels unavailable") })
			handler := HttpHandler{Stores: FixedStores{Store: lazyAdapterStore{info: info, body: body}}}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(method, "/t/"+string(d), nil))
			if response.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d; want 500", response.Code)
			}
			if response.Header().Get("ETag") != "" || response.Header().Get("Content-Length") != "" {
				t.Fatalf("blob success headers leaked: %v", response.Header())
			}
			if method == http.MethodGet && !body.closed {
				t.Fatal("opened body was not closed")
			}
		})
	}
}

func TestCacheLazyLabelsFailureStillStreams(t *testing.T) {
	const content = "content"
	d := DigestFromBytes([]byte(content))
	labelsCalled := make(chan struct{})
	info := NewInfo(d, int64(len(content)), time.Time{}, func(context.Context) (Labels, error) {
		close(labelsCalled)
		return nil, errors.New("labels unavailable")
	})
	primary := &cacheWriteRecorder{ErrorStore: ErrorStore{Err: ErrNotExist}}
	origin := lazyAdapterStore{info: info, body: &closeRecordingReader{Reader: strings.NewReader(content)}}
	store := &CacheStore{Primary: primary, Origin: origin}
	body, gotInfo, err := store.Open(t.Context(), d)
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	if gotInfo != info {
		t.Fatal("origin Info was replaced")
	}
	type readResult struct {
		data []byte
		err  error
	}
	done := make(chan readResult, 1)
	go func() { data, err := io.ReadAll(body); done <- readResult{data, err} }()
	select {
	case result := <-done:
		if result.err != nil || string(result.data) != content {
			t.Fatalf("read = %q, %v", result.data, result.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reader blocked after label loading failed")
	}
	<-labelsCalled
	if primary.calls.Load() != 0 {
		t.Fatal("cache attempted to write without valid labels")
	}
}

func TestCheckExistenceDoesNotLoadLabels(t *testing.T) {
	d := DigestFromBytes([]byte("content"))
	var calls atomic.Int32
	info := NewInfo(d, 7, time.Time{}, func(context.Context) (Labels, error) { calls.Add(1); return nil, errors.New("labels unavailable") })
	store := CheckExistence(lazyAdapterStore{info: info})
	m, err := store.Add(t.Context(), Meta{Digest: d}, strings.NewReader("content"))
	if !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("Add error = %v; want ErrAlreadyExists", err)
	}
	if m.Digest != d || m.Size != 7 || len(m.Labels) != 0 {
		t.Fatalf("duplicate metadata = %#v", m)
	}
	if calls.Load() != 0 {
		t.Fatal("duplicate check loaded labels")
	}
}

func TestHttpLastModified(t *testing.T) {
	stores := NewMemStores()
	m, err := stores.Use("t").Add(t.Context(), Meta{}, strings.NewReader("content"))
	if err != nil {
		t.Fatal(err)
	}
	local, err := stores.Use("t").Stat(t.Context(), m.Digest)
	if err != nil {
		t.Fatal(err)
	}
	added, err := local.Added(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(HttpHandler{Stores: stores})
	defer server.Close()
	response, err := server.Client().Get(server.URL + "/t/" + string(m.Digest))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if got := response.Header.Get("Last-Modified"); got != added.UTC().Format(http.TimeFormat) {
		t.Fatalf("GET Last-Modified = %q", got)
	}
	remote := HttpStores{Client: server.Client(), Target: server.URL}.Use("t")
	stat, err := remote.Stat(t.Context(), m.Digest)
	if err != nil {
		t.Fatal(err)
	}
	reader, opened, err := remote.Open(t.Context(), m.Digest)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	// HTTP dates carry whole seconds.
	for _, info := range []Info{stat, opened} {
		if got, err := info.Added(t.Context()); err != nil || !got.Equal(added.Truncate(time.Second)) {
			t.Fatalf("remote Added = %v, %v; want %v", got, err, added.Truncate(time.Second))
		}
	}

	d := DigestFromBytes([]byte("content"))
	unknown := HttpHandler{Stores: FixedStores{Store: lazyAdapterStore{info: NewInfo(d, 7, time.Time{}, nil)}}}
	recorder := httptest.NewRecorder()
	unknown.ServeHTTP(recorder, httptest.NewRequest(http.MethodHead, "/t/"+string(d), nil))
	if recorder.Code != http.StatusOK || recorder.Header().Get("Last-Modified") != "" {
		t.Fatalf("unknown time: %d %v", recorder.Code, recorder.Header())
	}
	unknownServer := httptest.NewServer(unknown)
	defer unknownServer.Close()
	info, err := HttpStores{Client: unknownServer.Client(), Target: unknownServer.URL}.Use("t").Stat(t.Context(), d)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := info.Added(t.Context()); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("remote unknown Added = %v", err)
	}

	failing := NewLazyInfo(d, nil, func(context.Context) (time.Time, error) { return time.Time{}, errors.New("stat failed") }, nil)
	recorder = httptest.NewRecorder()
	HttpHandler{Stores: FixedStores{Store: lazyAdapterStore{info: failing}}}.ServeHTTP(recorder, httptest.NewRequest(http.MethodHead, "/t/"+string(d), nil))
	if recorder.Code != http.StatusInternalServerError || recorder.Header().Get("ETag") != "" || recorder.Header().Get("Last-Modified") != "" {
		t.Fatalf("failed time: %d %v", recorder.Code, recorder.Header())
	}
}
