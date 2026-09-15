package flob

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

var (
	_ Stores = (*CacheStores)(nil)
	_ Store  = (*CacheStore)(nil)
)

// CacheStores read through cache for Stores.
// It tries to read from the primary store first, and if it fails, it reads from the origin store.
// It taps opened blob from the origin store to the primary store, but it does not guarantee the success
// of Primary.Add according to how the blob is read from the origin store.
// See [CacheStore.Open] for details. CacheStores must not be copied after first use,
// and Primary and Origin must not be changed after first use.
type CacheStores struct {
	Primary Stores
	Origin  Stores
	// FillTimeout bounds each cache write after its reader has delivered every
	// byte. Nonpositive uses [DefaultCacheFillTimeout].
	FillTimeout time.Duration
	flights     cacheFlights
}

// Unwrap exposes the primary pool's optional inventory capabilities.
func (s *CacheStores) Unwrap() Stores { return s.Primary }

func (s *CacheStores) Use(id string) Store {
	return &CacheStore{
		Primary:     s.Primary.Use(id),
		Origin:      s.Origin.Use(id),
		FillTimeout: s.FillTimeout,
		shared:      &s.flights,
		namespace:   id,
	}
}

// CacheStore is a per-namespace cache. It must not be copied after first use,
// and Primary and Origin must not be changed after first use.
type CacheStore struct {
	Primary Store
	Origin  Store
	// FillTimeout bounds a cache write after its reader has delivered every byte.
	// Nonpositive uses [DefaultCacheFillTimeout].
	FillTimeout time.Duration
	shared      *cacheFlights
	namespace   string
	local       cacheFlights
}

// DefaultCacheFillTimeout bounds a cache write that outlives its reader when
// FillTimeout is unset.
const DefaultCacheFillTimeout = 15 * time.Minute

// Unwrap exposes the primary store's optional capabilities.
func (s *CacheStore) Unwrap() Store { return s.Primary }

func (s *CacheStore) Add(ctx context.Context, m Meta, r io.Reader) (Meta, error) {
	return s.Primary.Add(ctx, m, r)
}

// Stat reads the primary first, falling back to the origin on failure.
func (s *CacheStore) Stat(ctx context.Context, d Digest) (Info, error) {
	info, err := s.Primary.Stat(ctx, d)
	if err == nil {
		return info, nil
	}
	return s.Origin.Stat(ctx, d)
}

// NewCacheStores creates a cache with miss coordination shared across Use calls.
func NewCacheStores(primary, origin Stores) *CacheStores {
	return &CacheStores{Primary: primary, Origin: origin}
}

// NewCacheStore creates a cache with its own miss coordination.
func NewCacheStore(primary, origin Store) *CacheStore {
	return &CacheStore{Primary: primary, Origin: origin}
}

type cacheFlightKey struct {
	namespace string
	digest    Digest
}
type cacheFlight struct {
	done      chan struct{}
	originErr error // Published by closing done.
}
type cacheFlights struct {
	mu     sync.Mutex
	active map[cacheFlightKey]*cacheFlight
}

func (f *cacheFlights) join(key cacheFlightKey) (*cacheFlight, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if flight := f.active[key]; flight != nil {
		return flight, false
	}
	if f.active == nil {
		f.active = make(map[cacheFlightKey]*cacheFlight)
	}
	flight := &cacheFlight{done: make(chan struct{})}
	f.active[key] = flight
	return flight, true
}
func (f *cacheFlights) finish(key cacheFlightKey, flight *cacheFlight, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.active[key] != flight {
		return
	}
	delete(f.active, key)
	flight.originErr = err
	close(flight.done)
}

// Open reads the primary first. On a miss, one caller streams from the origin
// while concurrent callers for the same namespace and digest wait for its cache
// write to finish. Waiters can cancel independently. After an unsuccessful fill,
// a waiter tries the origin once itself; ordinary origin Open errors are shared.
//
// Caching completes when all bytes have been read in order. Size probes and
// rereads of an already-read prefix are supported; reading past a gap, closing
// early, or canceling ctx before the last byte cancels the cache write and
// releases waiters. Once every byte has been read, the write no longer depends on
// the reader or ctx: it may finish after the reader closes and ctx is canceled,
// as when an HTTP handler returns, within FillTimeout, and waiters keep waiting
// for it. Writes remain best-effort. Callers must close the reader.
func (s *CacheStore) Open(ctx context.Context, d Digest) (io.ReadSeekCloser, Info, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	r, info, err := s.Primary.Open(ctx, d)
	if err == nil {
		return r, info, nil
	}
	registry := s.shared
	if registry == nil {
		registry = &s.local
	}
	key := cacheFlightKey{namespace: s.namespace, digest: d}
	flight, leader := registry.join(key)
	if !leader {
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-flight.done:
		}
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		if r, info, err := s.Primary.Open(ctx, d); err == nil {
			return r, info, nil
		}
		if flight.originErr != nil {
			return nil, nil, flight.originErr
		}
		return s.openOrigin(ctx, d, func(error) {})
	}
	finish := func(err error) { registry.finish(key, flight, err) }
	stopCancellation := context.AfterFunc(ctx, func() { finish(nil) })
	defer stopCancellation()
	// A fill may have committed between the initial miss and joining the registry.
	if r, info, err := s.Primary.Open(ctx, d); err == nil {
		finish(nil)
		return r, info, nil
	}
	return s.openOrigin(ctx, d, finish)
}

func (s *CacheStore) openOrigin(ctx context.Context, d Digest, finish func(error)) (io.ReadSeekCloser, Info, error) {
	if err := ctx.Err(); err != nil {
		finish(nil)
		return nil, nil, err
	}
	// Release waiters even if an origin operation has not returned on cancellation.
	stopFlightCancellation := context.AfterFunc(ctx, func() { finish(nil) })
	// The fill keeps ctx's values but not its cancellation: ctx aborts it only while
	// bytes are still flowing (see stopSourceCancellation).
	fillCtx, cancelFill := context.WithCancel(context.WithoutCancel(ctx))
	r, info, err := s.Origin.Open(ctx, d)
	var size int64
	if err == nil {
		if size, err = info.Size(ctx); err != nil {
			r.Close()
		}
	}
	if err != nil {
		cancelFill()
		stopFlightCancellation()
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			finish(nil)
		} else {
			finish(err)
		}
		return nil, nil, err
	}
	tap, sink := newBlobTap(r, size)
	abort := func(err error) {
		cancelFill()
		sink.CloseWithError(err)
		finish(nil)
	}
	fillDone := make(chan struct{})
	timeout := s.FillTimeout
	if timeout <= 0 {
		timeout = DefaultCacheFillTimeout
	}
	complete := func() {
		// Every byte reached the cache. Its write no longer depends on the reader or
		// ctx, and waiters keep waiting for it, but it must finish within timeout.
		stopFlightCancellation()
		go func() {
			select {
			case <-fillDone:
			case <-time.After(timeout):
				cancelFill()
			}
		}()
	}
	tap.onAbort = abort
	tap.onComplete = complete
	if size < 0 {
		abort(io.ErrUnexpectedEOF)
	}
	if size == 0 {
		// newBlobTap completed the empty blob before the hooks were set.
		complete()
	}
	stopSourceCancellation := context.AfterFunc(ctx, func() {
		// Abort a fill that is still receiving bytes, releasing the flight and any
		// blocked pipe write, then close the source. Stopping the tap either aborts
		// or finds it complete, so a fill that received every byte is left alone.
		tap.stop(ctx.Err())
		tap.Close()
	})
	go func() {
		defer close(fillDone)
		meta, err := infoMeta(fillCtx, info)
		if err == nil && fillCtx.Err() == nil {
			s.Primary.Add(fillCtx, meta, sink)
		}
		// Non-draining Add and label failures must unblock the leading reader too.
		sink.Close()
		finish(nil)
		stopFlightCancellation()
		cancelFill()
	}()
	reader := &cacheReader{blobTap: tap, stopCancellation: stopSourceCancellation}
	if err := ctx.Err(); err != nil {
		reader.Close()
		return nil, nil, err
	}
	return reader, info, nil
}

type cacheReader struct {
	*blobTap
	stopCancellation func() bool
}

func (r *cacheReader) Close() error {
	r.stopCancellation()
	return r.blobTap.Close()
}

func (s *CacheStore) Label(ctx context.Context, d Digest, labels Labels) error {
	return s.Primary.Label(ctx, d, labels)
}

func (s *CacheStore) Erase(ctx context.Context, d Digest) error {
	return s.Primary.Erase(ctx, d)
}

type blobTap struct {
	w          *io.PipeWriter
	r          io.ReadSeekCloser
	offset     int64 // Current source position.
	forwarded  int64 // Contiguous prefix already sent to the cache.
	size       int64
	stopped    atomic.Bool
	stopOnce   sync.Once
	closeOnce  sync.Once
	closeErr   error
	onAbort    func(error)
	onComplete func() // Called once the cache received every byte.
}

func newBlobTap(src io.ReadSeekCloser, size int64) (*blobTap, *io.PipeReader) {
	r, w := io.Pipe()
	tap := &blobTap{w: w, r: src, size: size}
	if size == 0 {
		tap.stop(nil)
	}
	if size < 0 {
		tap.stop(io.ErrUnexpectedEOF)
	}
	return tap, r
}

func (t *blobTap) Read(b []byte) (n int, err error) {
	n, err = t.r.Read(b)
	start := t.offset
	t.offset += int64(n)
	if t.stopped.Load() {
		return n, err
	}
	// The origin's size is trusted for completion without an extra EOF read,
	// but an observed read error or size mismatch must not commit a partial blob.
	if err != nil && err != io.EOF {
		t.stop(err)
		return n, err
	}
	if n > 0 && t.offset > t.size {
		t.stop(io.ErrUnexpectedEOF)
		return n, err
	}
	// An EOF probe beyond the forwarded prefix may be followed by a rewind.
	// Only EOF while reading the contiguous prefix proves truncation.
	if err == io.EOF && start <= t.forwarded && t.offset < t.size {
		t.stop(io.ErrUnexpectedEOF)
		return n, err
	}
	if n > 0 {
		if start > t.forwarded {
			t.stop(io.ErrUnexpectedEOF)
			return n, err
		}
		if t.offset > t.forwarded {
			skip := t.forwarded - start
			written, writeErr := t.w.Write(b[int(skip):n])
			t.forwarded += int64(written)
			if writeErr != nil {
				t.stop(writeErr)
				return n, err
			}
		}
	}
	if t.forwarded == t.size {
		t.stop(nil)
	}
	return n, err
}

func (t *blobTap) Seek(offset int64, whence int) (int64, error) {
	position, err := t.r.Seek(offset, whence)
	if err != nil || position < 0 {
		// A failed seek may have moved the source; its next position is unknown.
		t.stop(io.ErrUnexpectedEOF)
	} else {
		// A probe may temporarily move past untapped bytes and then rewind. Only
		// reading across such a gap makes the cache stream incomplete.
		t.offset = position
	}
	return position, err
}

func (t *blobTap) Close() error {
	t.stop(io.ErrUnexpectedEOF)
	t.closeOnce.Do(func() { t.closeErr = t.r.Close() })
	return t.closeErr
}

func (t *blobTap) stop(err error) {
	t.stopOnce.Do(func() {
		t.stopped.Store(true)
		t.w.CloseWithError(err)
		if err != nil && t.onAbort != nil {
			t.onAbort(err)
		}
		if err == nil && t.onComplete != nil {
			t.onComplete()
		}
	})
}
