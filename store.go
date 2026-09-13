package flob

import (
	"context"
	"io"
	"iter"
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
	// If m.Digest is set, its algorithm is used to compute the digest; otherwise [Canonical] is used.
	// If m.Digest is set and if it does not match the computed digest, it returns [ErrDigestMismatch].
	// It may block until the blob is fully read from r even if the context is canceled, so it is caller's
	// responsibility to close r when the context is canceled.
	Add(ctx context.Context, m Meta, r io.Reader) (Meta, error)
	// Stat checks existence and retrieves blob information without requiring label access.
	// It returns [ErrNotExist] if the blob does not exist in this store.
	Stat(ctx context.Context, d Digest) (Info, error)
	// Open opens the blob with the given digest for reading without loading labels.
	// Labels can be requested separately through the returned [Info].
	// It returns [ErrNotExist] if the blob does not exist.
	Open(ctx context.Context, d Digest) (io.ReadSeekCloser, Info, error)
	// Label updates the labels of the blob with the given digest.
	// It returns [ErrNotExist] if the blob does not exist.
	Label(ctx context.Context, d Digest, labels Labels) error
	// Erase removes the blob with the given digest from the store.
	// It does not return [ErrNotExist] even if the blob does not exist.
	Erase(ctx context.Context, d Digest) error
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

// Linker is an optional capability for adding a reference to an existing blob
// without reading or copying its content. The source and destination must share
// a compatible backing pool: the same filesystem root, MemStores instance, or
// S3Stores instance. Link copies source labels independently into the destination.
type Linker interface {
	// Link adds d to the destination using from's existing reference. It returns
	// ErrNotExist if the source does not contain d, including when the destination
	// already contains it. Otherwise an existing destination returns
	// ErrAlreadyExists with partial metadata and without changing its labels.
	// Invalid digests return the
	// validation error from Digest.Sanitize; incompatible sources return
	// ErrIncompatibleStore.
	// Source decorators exposing Unwrap() Store are followed to the underlying
	// store. CacheStore and FallbackStore therefore expose only their primary
	// store as the source; their read fallbacks do not apply.
	Link(ctx context.Context, d Digest, from Store) (Meta, error)
}

// AsLinker returns the first Linker in s's decorator chain, following Unwrap()
// Store methods, or false if none is found. CacheStore and FallbackStore expose
// their primary store. Decorator Add policies, including duplicate handling and
// digest preparation, are not automatically applied to Link.
func AsLinker(s Store) (Linker, bool) {
	for s != nil {
		if l, ok := s.(Linker); ok {
			return l, true
		}
		u, ok := s.(storeUnwrapper)
		if !ok {
			return nil, false
		}
		s = u.Unwrap()
	}
	return nil, false
}

func unwrapLinkSource(s Store) Store {
	for s != nil {
		u, ok := s.(storeUnwrapper)
		if !ok {
			return s
		}
		s = u.Unwrap()
	}
	return nil
}

// Walker optionally inventories blobs physically held in a store's namespace.
// Walk yields lazy Info values without promising order or a coherent snapshot.
// Concurrent changes may be omitted. A failure is yielded once, then iteration
// stops. Breaking iteration stops further work; cancellation yields ctx.Err().
type Walker interface {
	Walk(context.Context) iter.Seq2[Info, error]
}

// Namespacer optionally inventories namespaces containing at least one valid
// blob reference. Merely calling Use does not create an enumerable namespace.
// Namespaces has the same ordering, snapshot, cancellation, and error semantics
// as Walker.Walk.
type Namespacer interface {
	Namespaces(context.Context) iter.Seq2[string, error]
}

// AsWalker follows Unwrap() Store to discover physical inventory support.
// For cache/fallback decorators this inventories the primary, not a union.
func AsWalker(s Store) (Walker, bool) {
	for s != nil {
		if w, ok := s.(Walker); ok {
			return w, true
		}
		u, ok := s.(interface{ Unwrap() Store })
		if !ok {
			break
		}
		s = u.Unwrap()
	}
	return nil, false
}

// AsNamespacer follows Unwrap() Stores to discover namespace inventory support.
func AsNamespacer(s Stores) (Namespacer, bool) {
	for s != nil {
		if n, ok := s.(Namespacer); ok {
			return n, true
		}
		u, ok := s.(interface{ Unwrap() Stores })
		if !ok {
			break
		}
		s = u.Unwrap()
	}
	return nil, false
}
