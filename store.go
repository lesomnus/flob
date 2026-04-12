package flob

import (
	"context"
	"io"
	"maps"
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

type Meta struct {
	Digest Digest
	Labels Labels
	Size   int64
}

func (m *Meta) Clone() Meta {
	m_ := *m
	m_.Labels = maps.Clone(m.Labels)
	return m_
}
