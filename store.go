package flob

import (
	"context"
	"io"
	"time"
)

type Stores interface {
	Use(id string) Store
}

type Store interface {
	// Add adds a new blob to the store with [Meta], reading the content from r.
	// On success, it returns the complete [Meta] with the computed Digest.
	// Returned [Meta] may have additional fields set by the store, such as "Content-Type".
	// If a blob with the same digest already exists, it returns partial [Meta] with digest and [ErrAlreadyExists].
	// If m.Digest is set and [ErrAlreadyExists] is returned, r is not consumed so integrity of the existing blob is not verified.
	// If m.Digest is set and if it does not match the computed digest, it returns [ErrDigestMismatch].
	// It may block until the blob is fully read from r even if the context is canceled, so it is caller's
	// responsibility to close r when the context is canceled.
	Add(ctx context.Context, m Meta, r io.Reader) (Meta, error)
	// Get retrieves the [Meta] of the blob with the given digest.
	// It returns [ErrNotExist] if the blob does not exist.
	Get(ctx context.Context, d Digest) (Meta, error)
	// Open opens the blob with the given digest for reading.
	// It returns [ErrNotExist] if the blob does not exist.
	Open(ctx context.Context, d Digest) (io.ReadSeekCloser, Meta, error)
	// Label updates the labels of the blob with the given digest.
	// It returns [ErrNotExist] if the blob does not exist.
	Label(ctx context.Context, d Digest, labels Labels) error
	// Erase removes the blob with the given digest from the store.
	// It does not return [ErrNotExist] even if the blob does not exist.
	Erase(ctx context.Context, d Digest) error
}

// Stater is an optional capability for checking a blob's existence and size
// without retrieving its labels. Callers can use [AsStater] to discover it,
// falling back to [Store.Get] when unavailable.
type Stater interface {
	// Stat returns the blob's size in bytes, or [ErrNotExist] if it does not
	// exist in this store. Blobs in other stores are not visible.
	Stat(ctx context.Context, d Digest) (size int64, err error)
}

// AsStater returns the first [Stater] in s's decorator chain, following
// Unwrap() Store methods, or false if none is found. Decorators that change
// read semantics must implement Stat themselves to preserve those semantics.
func AsStater(s Store) (Stater, bool) {
	for s != nil {
		if st, ok := s.(Stater); ok {
			return st, true
		}
		u, ok := s.(storeUnwrapper)
		if !ok {
			return nil, false
		}
		s = u.Unwrap()
	}
	return nil, false
}

func stat(ctx context.Context, s Store, d Digest) (int64, error) {
	if st, ok := AsStater(s); ok {
		return st.Stat(ctx, d)
	}
	m, err := s.Get(ctx, d)
	return m.Size, err
}

// Presigner is an optional capability a [Store] may implement. Instead of
// streaming a blob's bytes, it hands out a short-lived direct URL to download it
// (e.g. an S3 presigned URL), letting a server redirect clients straight to the
// backing object store. A [Store] that does not implement Presigner is served by
// streaming through [Store.Open] as usual.
type Presigner interface {
	// PresignOpen returns a direct download URL for the blob with the given
	// digest — valid for approximately ttl — together with its [Meta]. It returns
	// [ErrNotExist] if this store has no such blob. The URL grants bearer access
	// to the blob for its lifetime, so ttl should be kept short. Like [Store.Open],
	// visibility is scoped to the store: a blob added only to another store yields
	// [ErrNotExist].
	PresignOpen(ctx context.Context, d Digest, ttl time.Duration) (url string, m Meta, err error)
}

// storeUnwrapper is implemented by a [Store] decorator to expose the store it
// wraps, so optional capabilities such as [Presigner] can be discovered through a
// chain of decorators. It mirrors the errors.Unwrap convention. A decorator that
// only observes or augments the [Store] methods (tracing, metrics, ...) should
// implement it so it does not hide capabilities of the store beneath it.
type storeUnwrapper interface {
	Unwrap() Store
}

// AsPresigner returns the first [Presigner] in s's decorator chain, following any
// Unwrap() Store methods (see [storeUnwrapper]), or false if none is found. Use
// this instead of a bare type assertion so a store wrapped in tracing/metrics
// decorators still exposes presign support.
func AsPresigner(s Store) (Presigner, bool) {
	for s != nil {
		if p, ok := s.(Presigner); ok {
			return p, true
		}
		u, ok := s.(storeUnwrapper)
		if !ok {
			return nil, false
		}
		s = u.Unwrap()
	}
	return nil, false
}

type Meta struct {
	Digest Digest
	Labels Labels
	Size   int64
}

func (m *Meta) Clone() Meta {
	m_ := *m
	m_.Labels = cloneLabels(m.Labels)
	return m_
}
