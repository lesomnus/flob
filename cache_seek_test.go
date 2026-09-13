package flob

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type completedCacheStore struct {
	Store
	done    chan error
	started chan struct{}
}

func (s completedCacheStore) Add(ctx context.Context, m Meta, r io.Reader) (Meta, error) {
	if s.started != nil {
		close(s.started)
	}
	m, err := s.Store.Add(ctx, m, r)
	s.done <- err
	return m, err
}
func waitCacheWrite(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("cache write did not finish")
		return nil
	}
}

func TestCacheServeContent(t *testing.T) {
	for _, mode := range []string{"sniff", "content-type", "handler", "range"} {
		t.Run(mode, func(t *testing.T) {
			content := []byte(strings.Repeat("blob content\n", 10000))
			origin := NewMemStores().Use("t")
			meta, err := origin.Add(t.Context(), Meta{}, bytes.NewReader(content))
			if err != nil {
				t.Fatal(err)
			}
			primary := completedCacheStore{Store: NewMemStores().Use("t"), done: make(chan error, 1), started: make(chan struct{})}
			store := &CacheStore{Primary: primary, Origin: origin}
			response := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, "/t/"+string(meta.Digest), nil)
			if mode == "handler" {
				HttpHandler{Stores: FixedStores{Store: store}}.ServeHTTP(response, request)
			} else {
				r, _, err := store.Open(t.Context(), meta.Digest)
				if err != nil {
					t.Fatal(err)
				}
				if mode == "content-type" || mode == "range" {
					response.Header().Set("Content-Type", "application/octet-stream")
				}
				if mode == "range" {
					// Ensure this case observes an aborted Add, rather than
					// cancellation before the best-effort writer starts.
					<-primary.started
					request.Header.Set("Range", "bytes=1000-1999")
				}
				http.ServeContent(response, request, "", time.Time{}, r)
				if err := r.Close(); err != nil {
					t.Fatal(err)
				}
			}
			writeErr := waitCacheWrite(t, primary.done)
			if mode == "range" {
				if response.Code != http.StatusPartialContent || !bytes.Equal(response.Body.Bytes(), content[1000:2000]) {
					t.Fatalf("range response = %d, %d bytes", response.Code, response.Body.Len())
				}
				if writeErr == nil {
					t.Fatal("partial range was cached")
				}
				if _, err := primary.Stat(t.Context(), meta.Digest); !errors.Is(err, ErrNotExist) {
					t.Fatalf("partial cache Stat = %v", err)
				}
				return
			}
			if response.Code != http.StatusOK || !bytes.Equal(response.Body.Bytes(), content) {
				t.Fatalf("response = %d, %d bytes", response.Code, response.Body.Len())
			}
			if writeErr != nil {
				t.Fatal(writeErr)
			}
			r, _, err := primary.Open(t.Context(), meta.Digest)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			cached, err := io.ReadAll(r)
			if err != nil || !bytes.Equal(cached, content) {
				t.Fatalf("cached bytes differ: %v", err)
			}
		})
	}
}

type failedSeekReader struct{ *bytes.Reader }

func (r failedSeekReader) Close() error { return nil }
func (r failedSeekReader) Seek(int64, int) (int64, error) {
	r.Reader.Seek(1, io.SeekStart)
	return 0, errors.New("seek failed after moving")
}

type terminalErrorReader struct{ *bytes.Reader }

func (r terminalErrorReader) Close() error { return nil }
func (r terminalErrorReader) Read(b []byte) (int, error) {
	n, _ := r.Reader.Read(b)
	return n, errors.New("source failed")
}

func TestBlobTapSeeks(t *testing.T) {
	for _, tc := range []struct {
		name   string
		size   int64
		source func() io.ReadSeekCloser
		read   func(*testing.T, *blobTap)
		valid  bool
	}{
		{name: "empty probes past end then rewind", size: 10, valid: true, read: func(t *testing.T, r *blobTap) {
			seekTap(t, r, 20, io.SeekStart)
			if n, err := r.Read(nil); n != 0 || (err != nil && err != io.EOF) {
				t.Fatalf("empty read = %d, %v", n, err)
			}
			if n, err := r.Read(make([]byte, 1)); n != 0 || err != io.EOF {
				t.Fatalf("EOF probe = %d, %v", n, err)
			}
			seekTap(t, r, 0, io.SeekStart)
			readTap(t, r, 10)
		}},
		{name: "probe and exact read without EOF", size: 10, valid: true, read: func(t *testing.T, r *blobTap) {
			seekTap(t, r, 0, io.SeekEnd)
			seekTap(t, r, 0, io.SeekStart)
			readTap(t, r, 10)
		}},
		{name: "overlapping replay forwards only suffix", size: 10, valid: true, read: func(t *testing.T, r *blobTap) { readTap(t, r, 4); seekTap(t, r, 2, io.SeekStart); readTap(t, r, 8) }},
		{name: "replay prefix then resume", size: 10, valid: true, read: func(t *testing.T, r *blobTap) {
			readTap(t, r, 4)
			seekTap(t, r, 0, io.SeekStart)
			readTap(t, r, 2)
			seekTap(t, r, 4, io.SeekStart)
			readTap(t, r, 6)
		}},
		{name: "no-op seek", size: 10, valid: true, read: func(t *testing.T, r *blobTap) { readTap(t, r, 4); seekTap(t, r, 0, io.SeekCurrent); readTap(t, r, 6) }},
		{name: "read across gap", size: 10, read: func(t *testing.T, r *blobTap) { readTap(t, r, 2); seekTap(t, r, 4, io.SeekStart); readTap(t, r, 6) }},
		{name: "close partial", size: 10, read: func(t *testing.T, r *blobTap) { readTap(t, r, 4) }},
		{name: "early EOF", size: 11, read: func(t *testing.T, r *blobTap) {
			if _, err := io.ReadAll(r); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "oversized origin", size: 9, read: func(t *testing.T, r *blobTap) { readTap(t, r, 10) }},
		{name: "failed seek", size: 10, source: func() io.ReadSeekCloser { return failedSeekReader{bytes.NewReader([]byte("0123456789"))} }, read: func(t *testing.T, r *blobTap) {
			if _, err := r.Seek(0, io.SeekStart); err == nil {
				t.Fatal("missing seek error")
			}
			got, err := io.ReadAll(r)
			if err != nil || string(got) != "123456789" {
				t.Fatalf("source result = %q, %v", got, err)
			}
		}},
		{name: "read error with final bytes", size: 10, source: func() io.ReadSeekCloser { return terminalErrorReader{bytes.NewReader([]byte("0123456789"))} }, read: func(t *testing.T, r *blobTap) {
			b := make([]byte, 10)
			n, err := r.Read(b)
			if n != 10 || err == nil {
				t.Fatalf("Read = %d, %v", n, err)
			}
		}},
		{name: "empty", size: 0, source: func() io.ReadSeekCloser { return nopCloser{bytes.NewReader(nil)} }, valid: true, read: func(t *testing.T, r *blobTap) {}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := io.ReadSeekCloser(nopCloser{bytes.NewReader([]byte("0123456789"))})
			if tc.source != nil {
				source = tc.source()
			}
			tap, sink := newBlobTap(source, tc.size)
			type result struct {
				data []byte
				err  error
			}
			done := make(chan result, 1)
			go func() { data, err := io.ReadAll(sink); sink.Close(); done <- result{data, err} }()
			tc.read(t, tap)
			tap.Close()
			select {
			case result := <-done:
				if tc.valid {
					want := "0123456789"
					if tc.size == 0 {
						want = ""
					}
					if result.err != nil || string(result.data) != want {
						t.Fatalf("tap = %q, %v", result.data, result.err)
					}
				} else if result.err == nil {
					t.Fatalf("incomplete tap succeeded: %q", result.data)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("tap sink blocked")
			}
		})
	}
}
func seekTap(t *testing.T, r *blobTap, offset int64, whence int) {
	t.Helper()
	if _, err := r.Seek(offset, whence); err != nil {
		t.Fatal(err)
	}
}
func readTap(t *testing.T, r *blobTap, n int) {
	t.Helper()
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		t.Fatal(err)
	}
}
