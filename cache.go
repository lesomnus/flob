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
// If the blob read from the origin store is not read to completion or is sought, it may not be cached
// in the primary store, and the add operation is canceled.
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

	r, sink := newBlobTap(r)
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
	w writeCloserWithError
	r io.ReadSeekCloser
	m Meta
}

func newBlobTap(src io.ReadSeekCloser) (*blobTap, *io.PipeReader) {
	r, w := io.Pipe()
	return &blobTap{w: w, r: src}, r
}

func (t *blobTap) Read(b []byte) (n int, err error) {
	n, err = t.r.Read(b)
	if n > 0 {
		if _, err_ := t.w.Write(b[:n]); err_ != nil {
			t.stop(err_)
		}
	}
	if err != nil {
		t.stop(err)
	}
	return n, err
}

func (t *blobTap) Seek(offset int64, whence int) (int64, error) {
	// Stop tapping if the blob is seeked, because the content may be read in a non-sequential way and
	// it may cause integrity issues.
	// Maybe if the seek is within the already read content, it can be still tapped, but for simplicity,
	// just stop tapping on any seek for now.
	t.stop(io.ErrUnexpectedEOF)

	return t.r.Seek(offset, whence)
}

func (t *blobTap) Close() error {
	t.stop(io.ErrUnexpectedEOF)
	return t.r.Close()
}

func (t *blobTap) stop(err error) {
	t.w.CloseWithError(err)
	t.w = discardSink{}
}

type writeCloserWithError interface {
	io.Writer
	CloseWithError(err error) error
}

type discardSink struct{}

func (s discardSink) Write(b []byte) (n int, err error) {
	return len(b), nil
}

func (s discardSink) CloseWithError(err error) error {
	return nil
}
