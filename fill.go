package flob

import (
	"context"
	"errors"
	"fmt"
	"hash"
	"io"
	"sync"
)

// Filler is an optional capability of a [Store] whose blob can be read while
// it is being added. [CacheStore] uses it so that every caller missing the same
// blob follows one fill from the origin, from its first byte, instead of
// waiting for the whole fill to finish.
type Filler interface {
	// Fill begins adding the blob m describes; m.Digest and m.Size must be set.
	// Like [Store.Add], it returns [ErrAlreadyExists] if the store already
	// has m.Digest.
	Fill(ctx context.Context, m Meta) (Fill, error)
}

// Fill is a blob being added. One writer writes it in order while any number
// of readers follow it.
type Fill interface {
	// Write appends p. Writing past the size the fill was begun with fails.
	Write(p []byte) (int, error)
	// Follow opens a reader from the blob's first byte. At the end of what has
	// been written, Read waits for more. Once every byte is written and they
	// match the digest, it reads to io.EOF; if they do not, it fails with
	// [ErrDigestMismatch]; if the fill is aborted, it fails with the error
	// given to Abort. ctx bounds the waiting. Follow fails once the fill has
	// ended and every reader has closed.
	Follow(ctx context.Context) (io.ReadSeekCloser, error)
	// Commit adds the blob to the store once every byte has been written, and
	// returns its [Meta] as [Store.Add] would.
	Commit(ctx context.Context) (Meta, error)
	// Abort ends the fill without adding the blob. Readers waiting for bytes
	// fail with err; readers of a fill that was completely written read on.
	// Abort after Commit only releases the fill.
	Abort(err error)
}

// AsFiller returns the first [Filler] in s's decorator chain, following
// Unwrap() Store methods, or false if none is found. As with [AsLinker],
// decorator Add policies are not applied to a Fill.
func AsFiller(s Store) (Filler, bool) {
	for s != nil {
		if f, ok := s.(Filler); ok {
			return f, true
		}
		u, ok := s.(storeUnwrapper)
		if !ok {
			return nil, false
		}
		s = u.Unwrap()
	}
	return nil, false
}

// errFillReleased is returned by Follow on a fill that has ended with no reader
// left, whose bytes are no longer kept.
var errFillReleased = errors.New("fill released")

// fillTracker is what the stores' fills have in common. It hashes what is
// written, lets readers follow it, and releases the fill's resources once it
// has ended and its last reader has closed.
type fillTracker struct {
	src  io.ReaderAt // the bytes written so far
	size int64
	want Digest
	h    hash.Hash

	mu      sync.Mutex
	wake    chan struct{} // closed and replaced whenever the state changes
	written int64
	// state is nil while bytes are expected, io.EOF once they are all written
	// and verified, and otherwise why the fill failed.
	state    error
	ended    bool
	readers  int
	release  func()
	released bool
}

func newFillTracker(m Meta, src io.ReaderAt, release func()) (*fillTracker, error) {
	if m.Size < 0 {
		return nil, fmt.Errorf("fill: size %d", m.Size)
	}
	t := &fillTracker{src: src, size: m.Size, want: m.Digest, h: m.Digest.Algorithm().Hash(), wake: make(chan struct{}), release: release}
	if t.size == 0 {
		t.verify()
	}
	return t, nil
}

// write stores p with store and records what it stored.
func (t *fillTracker) write(p []byte, store func([]byte) (int, error)) (int, error) {
	t.mu.Lock()
	state, room := t.state, t.size-t.written
	t.mu.Unlock()
	if state != nil {
		if state == io.EOF {
			return 0, errors.New("fill: write past its size")
		}
		return 0, state
	}
	short := false
	if int64(len(p)) > room {
		p, short = p[:room], true
	}

	n, err := store(p)
	t.h.Write(p[:n])

	t.mu.Lock()
	defer t.mu.Unlock()
	t.written += int64(n)
	if t.written == t.size {
		t.verify()
	}
	t.notify()
	if err == nil && short {
		err = errors.New("fill: write past its size")
	}
	return n, err
}

// verify settles a completely written fill. t.mu is held or t is not shared yet.
func (t *fillTracker) verify() {
	if Digest(fmt.Sprintf("%s:%x", t.want.Algorithm(), t.h.Sum(nil))) == t.want {
		t.state = io.EOF
	} else {
		t.state = ErrDigestMismatch
	}
}

func (t *fillTracker) notify() {
	close(t.wake)
	t.wake = make(chan struct{})
}

// complete reports whether every byte is written and verified.
func (t *fillTracker) complete() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	switch t.state {
	case io.EOF:
		return nil
	case nil:
		return io.ErrUnexpectedEOF
	default:
		return t.state
	}
}

// abort fails a fill that is still expecting bytes with err, and ends it.
func (t *fillTracker) abort(err error) {
	if err == nil {
		err = io.ErrUnexpectedEOF
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.state == nil {
		t.state = err
	}
	t.ended = true
	t.notify()
	t.maybeRelease()
}

// end marks a committed fill ended.
func (t *fillTracker) end() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.ended = true
	t.maybeRelease()
}

// maybeRelease releases an ended fill nobody reads. t.mu is held.
func (t *fillTracker) maybeRelease() {
	if t.ended && t.readers == 0 && !t.released {
		t.released = true
		if t.release != nil {
			t.release()
		}
	}
}

func (t *fillTracker) follow(ctx context.Context) (io.ReadSeekCloser, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.released {
		return nil, errFillReleased
	}
	t.readers++
	return &fillReader{t: t, ctx: ctx, closed: make(chan struct{})}, nil
}

// fillReader follows a [fillTracker].
type fillReader struct {
	t         *fillTracker
	ctx       context.Context
	off       int64
	closed    chan struct{}
	closeOnce sync.Once
}

func (r *fillReader) Read(b []byte) (int, error) {
	t := r.t
	for {
		select {
		case <-r.closed:
			return 0, errors.New("fill: read after close")
		default:
		}
		t.mu.Lock()
		written, state, wake := t.written, t.state, t.wake
		t.mu.Unlock()

		if state != nil && state != io.EOF {
			return 0, state
		}
		if r.off >= t.size && state == io.EOF {
			return 0, io.EOF
		}
		if r.off < written {
			if len(b) == 0 {
				return 0, nil
			}
			if int64(len(b)) > written-r.off {
				b = b[:written-r.off]
			}
			n, err := t.src.ReadAt(b, r.off)
			r.off += int64(n)
			if err == io.EOF && n == len(b) {
				err = nil
			}
			return n, err
		}
		select {
		case <-wake:
		case <-r.closed:
		case <-r.ctx.Done():
			return 0, r.ctx.Err()
		}
	}
}

func (r *fillReader) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		offset += r.off
	case io.SeekEnd:
		offset += r.t.size
	default:
		return 0, errors.New("fill: invalid whence")
	}
	if offset < 0 {
		return 0, errors.New("fill: negative position")
	}
	r.off = offset
	return offset, nil
}

func (r *fillReader) Close() error {
	r.closeOnce.Do(func() {
		close(r.closed)
		t := r.t
		t.mu.Lock()
		defer t.mu.Unlock()
		t.readers--
		t.maybeRelease()
	})
	return nil
}
