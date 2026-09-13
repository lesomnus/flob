package flob

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
)

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
						info = NewInfo(d, int64(len(content)), func(context.Context) (Labels, error) { <-gate; return nil, errors.New("label failure") })
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
						info = NewInfo(d, int64(len(content)), func(ctx context.Context) (Labels, error) { <-stalled; return nil, ctx.Err() })
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
				return blocked, NewInfo(d, int64(len(content)), nil), nil
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
