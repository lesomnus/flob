package flob

import (
	"context"
	"io"
)

var (
	_ Stores = CacheStores{}
	_ Store  = CacheStore{}
)

// CacheStores read through cache for Stores.
// It tries to read from the primary store first, and if it fails, it reads from the origin store.
// It taps opened blob from the origin store to the primary store, but it does not guarantee the success
// of Primary.Add according to how the blob is read from the origin store.
// See [CacheStore.Open] for details.
type CacheStores struct {
	Primary Stores
	Origin  Stores
}

func (s CacheStores) Use(id string) Store {
	return CacheStore{
		Primary: s.Primary.Use(id),
		Origin:  s.Origin.Use(id),
	}
}

type CacheStore struct {
	Primary Store
	Origin  Store
}

// Unwrap exposes the primary store's optional capabilities.
func (s CacheStore) Unwrap() Store { return s.Primary }

func (s CacheStore) Add(ctx context.Context, m Meta, r io.Reader) (Meta, error) {
	return s.Primary.Add(ctx, m, r)
}

// Stat reads the primary first, falling back to the origin on failure.
func (s CacheStore) Stat(ctx context.Context, d Digest) (Info, error) {
	info, err := s.Primary.Stat(ctx, d)
	if err == nil {
		return info, nil
	}
	return s.Origin.Stat(ctx, d)
}

// Open reads the blob from the primary store if it exists.
// Otherwise, it reads from the origin store and caches it in the primary store as it is being read.
// Caching completes when all bytes have been read in order. Size probes and rereads of an
// already-read prefix are supported; reading past a gap or closing early cancels the cache write.
// Because adding to the primary store happens in a separate goroutine, Stat or Open may not be able to
// read the blob from the primary store immediately after it has been read from the origin store.
// This design assumes it is better to return the blob as soon as possible rather than wait for it to be
// cached in the primary store, since the same blob is unlikely to be requested again very soon after the
// first access.
func (s CacheStore) Open(ctx context.Context, d Digest) (io.ReadSeekCloser, Info, error) {
	r, info, err := s.Primary.Open(ctx, d)
	if err == nil {
		return r, info, nil
	}

	r, info, err = s.Origin.Open(ctx, d)
	if err != nil {
		return nil, nil, err
	}

	r, sink := newBlobTap(r, info.Size())
	go func() {
		// Labels are needed only for the best-effort cache write. Failure must
		// not prevent streaming the origin's content.
		meta, err := infoMeta(ctx, info)
		if err == nil {
			s.Primary.Add(ctx, meta, sink)
		}
		// Close the read end so that if Add returned without draining the pipe (e.g.
		// the blob already exists in the primary and Add short-circuits), the pending
		// blobTap.Read write unblocks with io.ErrClosedPipe instead of hanging forever.
		sink.Close()
	}()

	return r, info, nil
}

func (s CacheStore) Label(ctx context.Context, d Digest, labels Labels) error {
	return s.Primary.Label(ctx, d, labels)
}

func (s CacheStore) Erase(ctx context.Context, d Digest) error {
	return s.Primary.Erase(ctx, d)
}

type blobTap struct {
	w         *io.PipeWriter
	r         io.ReadSeekCloser
	offset    int64 // Current source position.
	forwarded int64 // Contiguous prefix already sent to the cache.
	size      int64
	stopped   bool
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
	if t.stopped {
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
	return t.r.Close()
}

func (t *blobTap) stop(err error) {
	if t.stopped {
		return
	}
	t.stopped = true
	t.w.CloseWithError(err)
}
