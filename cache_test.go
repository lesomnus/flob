package flob

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/lesomnus/flob/internal/x"
)

// nonDrainingStores is a primary that never holds the blob (Open/Stat always miss) and whose
// Add returns immediately WITHOUT reading the supplied reader — mimicking a primary that
// short-circuits because the blob was committed concurrently. It exercises the blobTap path
// where Primary.Add does not drain the tee pipe.
type nonDrainingStores struct{}

func (nonDrainingStores) Use(string) Store { return nonDrainingStore{} }

type nonDrainingStore struct{}

func (nonDrainingStore) Add(context.Context, Meta, io.Reader) (Meta, error) {
	return Meta{}, ErrAlreadyExists
}
func (nonDrainingStore) Stat(context.Context, Digest) (Info, error) { return nil, ErrNotExist }
func (nonDrainingStore) Open(context.Context, Digest) (io.ReadSeekCloser, Info, error) {
	return nil, nil, ErrNotExist
}
func (nonDrainingStore) Label(context.Context, Digest, Labels) error { return ErrNotExist }
func (nonDrainingStore) Erase(context.Context, Digest) error         { return nil }

func TestCacheStore(t *testing.T) {
	new_stores := func(t *testing.T) Stores {
		t.Helper()
		return &CacheStores{
			Primary: NewMemStores(),
			Origin:  NewMemStores(),
		}
	}
	new_store := func(t *testing.T) *CacheStore {
		t.Helper()
		stores := new_stores(t)
		return stores.Use("test").(*CacheStore)
	}

	t.Run("contract", func(t *testing.T) {
		testStore(t, func(t *testing.T) Stores {
			t.Helper()
			return &CacheStores{
				Primary: NewMemStores(),
				Origin:  NewMemStores(),
			}
		})
	})

	t.Run("primary only blob can be read", func(t *testing.T) {
		ctx, x := x.New(t)
		s := new_store(t)

		m, err := s.Primary.Add(ctx, Meta{}, x.Reader())
		x.NoError(err)

		_, err = statMeta(ctx, s, m.Digest)
		x.NoError(err)

		_, err = statMeta(ctx, s.Origin, m.Digest)
		x.ErrorIs(err, ErrNotExist)
	})
	t.Run("stat from origin not cached", func(t *testing.T) {
		ctx, x := x.New(t)
		s := new_store(t)

		m, err := s.Origin.Add(ctx, Meta{}, x.Reader())
		x.NoError(err)

		_, err = statMeta(ctx, s, m.Digest)
		x.NoError(err)

		_, err = statMeta(ctx, s.Primary, m.Digest)
		x.ErrorIs(err, ErrNotExist)
	})
	t.Run("full read from origin makes cache", func(t *testing.T) {
		ctx, x := x.New(t)
		s := new_store(t)

		m, err := s.Origin.Add(ctx, Meta{}, x.Reader())
		x.NoError(err)

		r, _, err := s.Open(ctx, m.Digest)
		x.NoError(err)
		defer r.Close()

		_, err = io.Copy(io.Discard, r)
		x.NoError(err)

		// It may take some time for the blob to be cached in the primary store,
		// so we wait for a while before checking.
		time.Sleep(30 * time.Millisecond)

		_, err = statMeta(ctx, s.Primary, m.Digest)
		x.NoError(err)
	})
	t.Run("open does not deadlock when primary add short-circuits", func(t *testing.T) {
		ctx, x := x.New(t)

		origin := NewMemStores()
		m, err := origin.Use("t").Add(ctx, Meta{}, x.Reader())
		x.NoError(err)

		// Primary.Open misses (so Open taps the origin read), but Primary.Add returns
		// immediately without draining the tee pipe. Before the fix, blobTap.Read blocked
		// forever on the unbuffered pipe write and the caller's read hung.
		s := (&CacheStores{Primary: nonDrainingStores{}, Origin: origin}).Use("t")

		done := make(chan []byte, 1)
		go func() {
			r, _, err := s.Open(ctx, m.Digest)
			if err != nil {
				done <- nil
				return
			}
			data, _ := io.ReadAll(r)
			r.Close()
			done <- data
		}()

		select {
		case data := <-done:
			x.Eq(x.Data(), data)
		case <-time.After(2 * time.Second):
			t.Fatal("Open deadlocked: blobTap.Read blocked on a pipe the primary never drains")
		}
	})
	t.Run("add does not affect origin", func(t *testing.T) {
		ctx, x := x.New(t)
		s := new_store(t)

		m, err := s.Add(ctx, Meta{}, x.Reader())
		x.NoError(err)

		_, err = statMeta(ctx, s.Origin, m.Digest)
		x.ErrorIs(err, ErrNotExist)
	})
}

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
	for name, factory := range map[string]func(*testing.T) Store{
		"memory": func(t *testing.T) Store { return NewMemStores().Use("t") },
		"http": func(t *testing.T) Store {
			stores, _ := newHttpStores(t, NewMemStores())
			return stores.Use("t")
		},
		"s3": func(t *testing.T) Store {
			stores, _ := newMockS3Stores(t)
			return stores.Use("t")
		},
	} {
		t.Run(name, func(t *testing.T) {
			for _, mode := range []string{"sniff", "content-type", "handler", "range"} {
				t.Run(mode, func(t *testing.T) {
					content := []byte(strings.Repeat("blob content\n", 10000))
					origin := factory(t)
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

type flightTestStore struct {
	Store
	open func(context.Context, Digest) (io.ReadSeekCloser, Info, error)
	add  func(context.Context, Meta, io.Reader) (Meta, error)
}

func (s flightTestStore) Open(ctx context.Context, d Digest) (io.ReadSeekCloser, Info, error) {
	if s.open != nil {
		return s.open(ctx, d)
	}
	return s.Store.Open(ctx, d)
}
func (s flightTestStore) Add(ctx context.Context, m Meta, r io.Reader) (Meta, error) {
	if s.add != nil {
		return s.add(ctx, m, r)
	}
	return s.Store.Add(ctx, m, r)
}

type flightTestOrigins struct {
	Stores
	opens atomic.Int32
}

func (s *flightTestOrigins) Use(id string) Store {
	source := s.Stores.Use(id)
	return flightTestStore{Store: source, open: func(ctx context.Context, d Digest) (io.ReadSeekCloser, Info, error) {
		s.opens.Add(1)
		return source.Open(ctx, d)
	}}
}

type flightOpenResult struct {
	reader io.ReadSeekCloser
	info   Info
	err    error
}

func flightOpen(ctx context.Context, store Store, d Digest) <-chan flightOpenResult {
	done := make(chan flightOpenResult, 1)
	go func() { r, info, err := store.Open(ctx, d); done <- flightOpenResult{r, info, err} }()
	return done
}
func flightResult(t *testing.T, done <-chan flightOpenResult) flightOpenResult {
	t.Helper()
	select {
	case r := <-done:
		return r
	default:
		t.Fatal("Open did not return")
		return flightOpenResult{}
	}
}
func flightPending(t *testing.T, done <-chan flightOpenResult) {
	t.Helper()
	select {
	case r := <-done:
		if r.reader != nil {
			r.reader.Close()
		}
		t.Fatalf("Open returned before fill completed: %v", r.err)
	default:
	}
}
func flightRead(t *testing.T, r io.ReadSeekCloser, want string) {
	t.Helper()
	got, err := io.ReadAll(r)
	r.Close()
	if err != nil || string(got) != want {
		t.Fatalf("read = %q, %v; want %q", got, err, want)
	}
}
func flightReadResult(t *testing.T, done <-chan flightOpenResult, want string) {
	t.Helper()
	r := flightResult(t, done)
	if r.err != nil {
		t.Fatal(r.err)
	}
	flightRead(t, r.reader, want)
}
func flightActive(t *testing.T, f *cacheFlights, want int) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.active) != want {
		t.Fatalf("active flights = %d; want %d", len(f.active), want)
	}
}
func flightAdd(t *testing.T, store Store, content string) Digest {
	t.Helper()
	m, err := store.Add(t.Context(), Meta{}, strings.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	return m.Digest
}

func TestCacheFlightRepeatedUse(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const content = "shared cache content"
		origins := &flightTestOrigins{Stores: NewMemStores()}
		d := flightAdd(t, origins.Stores.Use("t"), content)
		cache := NewCacheStores(NewMemStores(), origins)
		leader, _, err := cache.Use("t").Open(t.Context(), d)
		if err != nil {
			t.Fatal(err)
		}
		followers := make([]<-chan flightOpenResult, 20)
		for i := range followers {
			followers[i] = flightOpen(t.Context(), cache.Use("t"), d)
		}
		synctest.Wait()
		for _, done := range followers {
			flightPending(t, done)
		}
		if got := origins.opens.Load(); got != 1 {
			t.Fatalf("origin opens = %d; want 1", got)
		}
		flightRead(t, leader, content)
		synctest.Wait()
		for _, done := range followers {
			flightReadResult(t, done, content)
		}
		flightActive(t, &cache.flights, 0)
		if got := origins.opens.Load(); got != 1 {
			t.Fatalf("origin opens = %d; want 1", got)
		}
	})
}

func TestCacheFlightIndependentKeys(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		origins := &flightTestOrigins{Stores: NewMemStores()}
		first := flightAdd(t, origins.Stores.Use("a"), "first")
		second := flightAdd(t, origins.Stores.Use("a"), "second")
		flightAdd(t, origins.Stores.Use("b"), "first")
		cache := NewCacheStores(NewMemStores(), origins)
		results := []<-chan flightOpenResult{flightOpen(t.Context(), cache.Use("a"), first), flightOpen(t.Context(), cache.Use("a"), second), flightOpen(t.Context(), cache.Use("b"), first)}
		synctest.Wait()
		flightActive(t, &cache.flights, 3)
		for i, want := range []string{"first", "second", "first"} {
			flightReadResult(t, results[i], want)
		}
		synctest.Wait()
		if got := origins.opens.Load(); got != 3 {
			t.Fatalf("origin opens = %d; want 3", got)
		}
		flightActive(t, &cache.flights, 0)
	})
}

func TestCacheFlightWaitsForCommit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const content = "content"
		source, primary := NewMemStores().Use("t"), NewMemStores().Use("t")
		d := flightAdd(t, source, content)
		commit := make(chan struct{})
		staged := make(chan struct{})
		cache := NewCacheStore(flightTestStore{Store: primary, add: func(ctx context.Context, m Meta, r io.Reader) (Meta, error) {
			data, err := io.ReadAll(r)
			if err != nil {
				return Meta{}, err
			}
			close(staged)
			<-commit
			return primary.Add(ctx, m, bytes.NewReader(data))
		}}, source)
		leader, _, err := cache.Open(t.Context(), d)
		if err != nil {
			t.Fatal(err)
		}
		follower := flightOpen(t.Context(), cache, d)
		synctest.Wait()
		flightRead(t, leader, content)
		synctest.Wait()
		<-staged
		flightPending(t, follower)
		flightActive(t, &cache.local, 1)
		close(commit)
		synctest.Wait()
		flightReadResult(t, follower, content)
		flightActive(t, &cache.local, 0)
	})
}

func TestCacheFlightWaiterCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		source := NewMemStores().Use("t")
		d := flightAdd(t, source, "content")
		cache := NewCacheStore(NewMemStores().Use("t"), source)
		leader, _, err := cache.Open(t.Context(), d)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		follower := flightOpen(ctx, cache, d)
		synctest.Wait()
		cancel()
		synctest.Wait()
		if got := flightResult(t, follower).err; !errors.Is(got, context.Canceled) {
			t.Fatalf("waiter error = %v", got)
		}
		flightActive(t, &cache.local, 1)
		flightRead(t, leader, "content")
		synctest.Wait()
		flightActive(t, &cache.local, 0)
	})
}

func TestCacheFlightPartialLeader(t *testing.T) {
	for _, mode := range []string{"close", "gap"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				const content = "0123456789"
				origins := &flightTestOrigins{Stores: NewMemStores()}
				d := flightAdd(t, origins.Stores.Use("t"), content)
				cache := NewCacheStores(NewMemStores(), origins)
				leader, _, err := cache.Use("t").Open(t.Context(), d)
				if err != nil {
					t.Fatal(err)
				}
				follower := flightOpen(t.Context(), cache.Use("t"), d)
				synctest.Wait()
				flightPending(t, follower)
				if mode == "gap" {
					if _, err := leader.Seek(3, io.SeekStart); err != nil {
						t.Fatal(err)
					}
					b := make([]byte, 1)
					if n, err := leader.Read(b); n != 1 || err != nil || b[0] != '3' {
						t.Fatalf("gap read = %q, %v", b, err)
					}
				} else {
					leader.Close()
				}
				synctest.Wait()
				flightReadResult(t, follower, content)
				leader.Close()
				synctest.Wait()
				if got := origins.opens.Load(); got != 2 {
					t.Fatalf("origin opens = %d; want 2", got)
				}
				flightActive(t, &cache.flights, 0)
			})
		})
	}
}

func TestCacheFlightOriginFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		failure := errors.New("origin unavailable")
		gate := make(chan struct{})
		var calls atomic.Int32
		origin := flightTestStore{Store: UnimplementedStore{}, open: func(context.Context, Digest) (io.ReadSeekCloser, Info, error) {
			calls.Add(1)
			<-gate
			return nil, nil, failure
		}}
		cache := NewCacheStore(ErrorStore{Err: ErrNotExist}, origin)
		d := DigestFromBytes([]byte("content"))
		leader := flightOpen(t.Context(), cache, d)
		synctest.Wait()
		follower := flightOpen(t.Context(), cache, d)
		synctest.Wait()
		close(gate)
		synctest.Wait()
		for _, done := range []<-chan flightOpenResult{leader, follower} {
			if got := flightResult(t, done).err; !errors.Is(got, failure) {
				t.Fatalf("Open = %v", got)
			}
		}
		if calls.Load() != 1 {
			t.Fatalf("origin opens = %d; want 1", calls.Load())
		}
		flightActive(t, &cache.local, 0)
	})
}

func TestCacheFlightWriteFailure(t *testing.T) {
	for _, stage := range []string{"labels", "add", "duplicate"} {
		t.Run(stage, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				const content = "content"
				source, primary := NewMemStores().Use("t"), NewMemStores().Use("t")
				d := flightAdd(t, source, content)
				gate := make(chan struct{})
				var opens, adds atomic.Int32
				origin := flightTestStore{Store: source, open: func(ctx context.Context, d Digest) (io.ReadSeekCloser, Info, error) {
					opens.Add(1)
					r, info, err := source.Open(ctx, d)
					if stage == "labels" {
						info = NewInfo(d, int64(len(content)), time.Time{}, func(context.Context) (Labels, error) { <-gate; return nil, errors.New("label failure") })
					}
					return r, info, err
				}}
				destination := flightTestStore{Store: primary, add: func(ctx context.Context, m Meta, r io.Reader) (Meta, error) {
					adds.Add(1)
					<-gate
					if stage == "duplicate" {
						if _, err := primary.Add(ctx, m, strings.NewReader(content)); err != nil {
							return Meta{}, err
						}
						return m, ErrAlreadyExists
					}
					return Meta{}, errors.New("cache write failed")
				}}
				cache := NewCacheStore(destination, origin)
				leader, _, err := cache.Open(t.Context(), d)
				if err != nil {
					t.Fatal(err)
				}
				follower := flightOpen(t.Context(), cache, d)
				synctest.Wait()
				flightPending(t, follower)
				close(gate)
				synctest.Wait()
				flightRead(t, leader, content)
				flightReadResult(t, follower, content)
				synctest.Wait()
				flightActive(t, &cache.local, 0)
				want := int32(2)
				if stage == "duplicate" {
					want = 1
				}
				if opens.Load() != want {
					t.Fatalf("origin opens = %d; want %d", opens.Load(), want)
				}
				if stage == "labels" && adds.Load() != 0 {
					t.Fatal("Add ran after labels failed")
				}
			})
		})
	}
}

func TestCacheFlightCancellationDuringWriter(t *testing.T) {
	for _, stage := range []string{"labels", "add"} {
		t.Run(stage, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				const content = "content"
				source, primary := NewMemStores().Use("t"), NewMemStores().Use("t")
				d := flightAdd(t, source, content)
				stalled := make(chan struct{})
				var opens, adds atomic.Int32
				origin := flightTestStore{Store: source, open: func(ctx context.Context, d Digest) (io.ReadSeekCloser, Info, error) {
					first := opens.Add(1) == 1
					r, info, err := source.Open(ctx, d)
					if stage == "labels" && first {
						info = NewInfo(d, int64(len(content)), time.Time{}, func(ctx context.Context) (Labels, error) { <-stalled; return nil, ctx.Err() })
					}
					return r, info, err
				}}
				destination := flightTestStore{Store: primary, add: func(ctx context.Context, m Meta, r io.Reader) (Meta, error) {
					if adds.Add(1) == 1 && stage == "add" {
						<-stalled
						return Meta{}, ctx.Err()
					}
					return primary.Add(ctx, m, r)
				}}
				cache := NewCacheStore(destination, origin)
				ctx, cancel := context.WithCancel(t.Context())
				leader, _, err := cache.Open(ctx, d)
				if err != nil {
					t.Fatal(err)
				}
				follower := flightOpen(t.Context(), cache, d)
				synctest.Wait()
				flightPending(t, follower)
				cancel()
				synctest.Wait()
				// The first writer deliberately ignores cancellation until released. Its
				// stalled goroutine must not retain the flight or delay the follower.
				flightReadResult(t, follower, content)
				flightActive(t, &cache.local, 0)
				close(stalled)
				leader.Close()
				synctest.Wait()
				if opens.Load() != 2 {
					t.Fatalf("origin opens = %d; want 2", opens.Load())
				}
			})
		})
	}
}

type flightBlockingReader struct {
	closed       chan struct{}
	closeEntered chan struct{}
	closeGate    chan struct{}
	once         sync.Once
}

func (r *flightBlockingReader) Read([]byte) (int, error)       { <-r.closed; return 0, io.ErrClosedPipe }
func (r *flightBlockingReader) Seek(int64, int) (int64, error) { return 0, nil }
func (r *flightBlockingReader) Close() error {
	r.once.Do(func() { close(r.closeEntered); <-r.closeGate; close(r.closed) })
	return nil
}

func TestCacheFlightLeaderCancellationClosesSource(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const content = "content"
		source := NewMemStores().Use("t")
		d := flightAdd(t, source, content)
		blocked := &flightBlockingReader{closed: make(chan struct{}), closeEntered: make(chan struct{}), closeGate: make(chan struct{})}
		var calls atomic.Int32
		origin := flightTestStore{Store: source, open: func(ctx context.Context, d Digest) (io.ReadSeekCloser, Info, error) {
			if calls.Add(1) == 1 {
				return blocked, NewInfo(d, int64(len(content)), time.Time{}, nil), nil
			}
			return source.Open(ctx, d)
		}}
		cache := NewCacheStore(NewMemStores().Use("t"), origin)
		ctx, cancel := context.WithCancel(t.Context())
		leader, _, err := cache.Open(ctx, d)
		if err != nil {
			t.Fatal(err)
		}
		readDone := make(chan error, 1)
		go func() { _, err := io.ReadAll(leader); readDone <- err }()
		follower := flightOpen(t.Context(), cache, d)
		synctest.Wait()
		cancel()
		synctest.Wait()
		<-blocked.closeEntered
		// A slow Close must not delay flight release or the follower's own fetch.
		flightReadResult(t, follower, content)
		flightActive(t, &cache.local, 0)
		close(blocked.closeGate)
		synctest.Wait()
		if err := <-readDone; !errors.Is(err, io.ErrClosedPipe) {
			t.Fatalf("leader read = %v", err)
		}
		leader.Close()
	})
}
