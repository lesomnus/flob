package flob

import (
	"context"
	"errors"
	"io"

	"github.com/opencontainers/go-digest"
)

var (
	_ Stores = (*UnimplementedStores)(nil)
	_ Store  = (*UnimplementedStore)(nil)
)

type UnimplementedStores struct{}

func (s UnimplementedStores) Use(id string) Store {
	return UnimplementedStore{}
}

type UnimplementedStore struct{}

func (s UnimplementedStore) Add(ctx context.Context, m Meta, r io.Reader) (Meta, error) {
	return Meta{}, ErrUnimplemented
}
func (s UnimplementedStore) Get(ctx context.Context, d Digest) (Meta, error) {
	return Meta{}, ErrUnimplemented
}
func (s UnimplementedStore) Open(ctx context.Context, d Digest) (io.ReadSeekCloser, Meta, error) {
	return nil, Meta{}, ErrUnimplemented
}
func (s UnimplementedStore) Label(ctx context.Context, d Digest, labels Labels) error {
	return ErrUnimplemented
}
func (s UnimplementedStore) Erase(ctx context.Context, d Digest) error {
	return ErrUnimplemented
}

var (
	_ Stores = (*FixedStores)(nil)
)

type FixedStores struct {
	Store Store
}

func (s FixedStores) Use(id string) Store {
	return s.Store
}

var (
	_ Store = (*ErrorStore)(nil)
)

type ErrorStore struct {
	Err error
}

func (s ErrorStore) Add(ctx context.Context, m Meta, r io.Reader) (Meta, error) {
	return Meta{}, s.Err
}
func (s ErrorStore) Get(ctx context.Context, d Digest) (Meta, error) {
	return Meta{}, s.Err
}
func (s ErrorStore) Open(ctx context.Context, d Digest) (io.ReadSeekCloser, Meta, error) {
	return nil, Meta{}, s.Err
}
func (s ErrorStore) Label(ctx context.Context, d Digest, labels Labels) error {
	return s.Err
}
func (s ErrorStore) Erase(ctx context.Context, d Digest) error {
	return s.Err
}

var (
	_ Stores = (*FallbackStores)(nil)
	_ Store  = (*FallbackStore)(nil)
)

type FallbackStores struct {
	Primary   Stores
	Secondary Store
}

func (s FallbackStores) Use(id string) Store {
	return FallbackStore{
		Primary:   s.Primary.Use(id),
		Secondary: s.Secondary,
	}
}

type FallbackStore struct {
	Primary   Store
	Secondary Store
}

// Unwrap exposes the primary store's optional capabilities.
func (s FallbackStore) Unwrap() Store { return s.Primary }

func (s FallbackStore) Add(ctx context.Context, m Meta, r io.Reader) (Meta, error) {
	return s.Primary.Add(ctx, m, r)
}
func (s FallbackStore) Get(ctx context.Context, d Digest) (Meta, error) {
	m, err := s.Primary.Get(ctx, d)
	if err == nil {
		return m, nil
	}

	return s.Secondary.Get(ctx, d)
}
func (s FallbackStore) Open(ctx context.Context, d Digest) (io.ReadSeekCloser, Meta, error) {
	r, m, err := s.Primary.Open(ctx, d)
	if err == nil {
		return r, m, nil
	}

	return s.Secondary.Open(ctx, d)
}

func (s FallbackStore) Label(ctx context.Context, d Digest, labels Labels) error {
	return s.Primary.Label(ctx, d, labels)
}

func (s FallbackStore) Erase(ctx context.Context, d Digest) error {
	return s.Primary.Erase(ctx, d)
}

type allowDuplicates struct {
	Store
}

func AllowDuplicates(s Store) Store {
	return allowDuplicates{Store: s}
}

func (s allowDuplicates) Add(ctx context.Context, m Meta, r io.Reader) (Meta, error) {
	m, err := s.Store.Add(ctx, m, r)
	if errors.Is(err, ErrAlreadyExists) {
		return m, nil
	}
	return m, err
}

func (s allowDuplicates) Unwrap() Store { return s.Store }

type checkExistence struct {
	Store
}

func CheckExistence(s Store) Store {
	return checkExistence{Store: s}
}

func (s checkExistence) Add(ctx context.Context, m Meta, r io.Reader) (Meta, error) {
	if m.Digest != "" {
		if m, err := s.Store.Get(ctx, m.Digest); err == nil {
			return m, ErrAlreadyExists
		}
	}
	return s.Store.Add(ctx, m, r)
}

func (s checkExistence) Unwrap() Store { return s.Store }

type prepareDigest struct {
	Store
	algo digest.Algorithm
}

func PrepareDigest(s Store, algo digest.Algorithm) Store {
	if algo == "" {
		algo = Canonical
	}
	return prepareDigest{s, algo}
}

func (s prepareDigest) Add(ctx context.Context, m Meta, r io.Reader) (Meta, error) {
	if m.Digest != "" {
		if rs, ok := r.(io.ReadSeeker); ok {
			d, err := s.hash(rs)
			if err != nil {
				// Reader is touched but hashing failed or it could not be taken back to the
				// original position, so we return an error instead of proceeding with a
				// potentially corrupted reader.
				return Meta{}, err
			}
			m.Digest = d
		}
	}
	return s.Store.Add(ctx, m, r)
}

func (s prepareDigest) Unwrap() Store { return s.Store }

func (s prepareDigest) hash(r io.ReadSeeker) (Digest, error) {
	c, err := r.Seek(0, io.SeekCurrent)
	if err != nil {
		return "", err
	}

	h := s.algo.Digester()
	if _, err := io.Copy(h.Hash(), r); err != nil {
		// Best-effort restore of the position, but surface the copy error.
		_, _ = r.Seek(c, io.SeekStart)
		return "", err
	}
	d := Digest(h.Digest())
	if _, err := r.Seek(c, io.SeekStart); err != nil {
		return "", err
	}
	return d, nil
}
