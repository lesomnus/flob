package flob

import (
	"context"
	"io"
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
