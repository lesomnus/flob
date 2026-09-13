package flob

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/lesomnus/flob/internal/x"
)

// recordingStore delegates to an embedded Store but records whether Add was invoked, so a
// decorator that silently drops the Add (instead of forwarding it) can be detected.
type recordingStore struct {
	Store
	addCalled  bool
	lastDigest Digest
}

func (r *recordingStore) Add(ctx context.Context, m Meta, rd io.Reader) (Meta, error) {
	r.addCalled = true
	r.lastDigest = m.Digest
	return r.Store.Add(ctx, m, rd)
}

type errSeeker struct{ io.Reader }

func (errSeeker) Seek(int64, int) (int64, error) {
	return 0, errors.New("seek boom")
}

func TestPrepareDigest(t *testing.T) {
	// Regression: the branches in prepareDigest.Add were once inverted, so on the normal
	// (hash-success) path it returned (Meta{}, nil) WITHOUT calling the underlying Add — the
	// blob was silently dropped while the caller saw success.
	t.Run("forwards to underlying store and stores the blob", func(t *testing.T) {
		ctx, x := x.New(t)
		rec := &recordingStore{Store: NewMemStores().Use("t")}
		s := PrepareDigest(rec, Canonical)

		d := DigestFromBytes(x.Data())
		m, err := s.Add(ctx, Meta{Digest: d}, x.Reader())
		x.NoError(err)
		x.Eq(d, m.Digest)

		if !rec.addCalled {
			t.Fatal("underlying store.Add was never called: blob silently dropped")
		}
		x.Eq(d, rec.lastDigest)

		r, _, err := s.Open(ctx, d)
		x.NoError(err)
		defer r.Close()
		got, err := io.ReadAll(r)
		x.NoError(err)
		x.Eq(x.Data(), got)
	})
	t.Run("non-seeker reader passes through", func(t *testing.T) {
		ctx, x := x.New(t)
		rec := &recordingStore{Store: NewMemStores().Use("t")}
		s := PrepareDigest(rec, Canonical)

		d := DigestFromBytes(x.Data())
		// io.NopCloser is a plain io.Reader (not an io.Seeker).
		m, err := s.Add(ctx, Meta{Digest: d}, io.NopCloser(x.Reader()))
		x.NoError(err)
		x.Eq(d, m.Digest)
		if !rec.addCalled {
			t.Fatal("underlying store.Add was never called")
		}
	})
	t.Run("seek failure surfaces an error and does not forward", func(t *testing.T) {
		ctx, x := x.New(t)
		rec := &recordingStore{Store: NewMemStores().Use("t")}
		s := PrepareDigest(rec, Canonical)

		d := DigestFromBytes(x.Data())
		_, err := s.Add(ctx, Meta{Digest: d}, errSeeker{x.Reader()})
		if err == nil {
			t.Fatal("expected an error when the reader cannot be restored, got nil")
		}
		if rec.addCalled {
			t.Fatal("underlying store.Add must not be called after a hash failure")
		}
	})
}

func TestFallbackStore(t *testing.T) {
	t.Run("stat and open fall back to secondary", func(t *testing.T) {
		ctx, x := x.New(t)

		primary := NewMemStores()
		secondary := NewMemStores().Use("secondary")
		stores := FallbackStores{Primary: primary, Secondary: secondary}

		// The blob exists only in the secondary.
		m, err := secondary.Add(ctx, Meta{}, x.Reader())
		x.NoError(err)

		s := stores.Use("repo")
		got, err := statMeta(ctx, s, m.Digest)
		x.NoError(err)
		x.Eq(m.Digest, got.Digest)

		r, _, err := s.Open(ctx, m.Digest)
		x.NoError(err)
		defer r.Close()
		data, err := io.ReadAll(r)
		x.NoError(err)
		x.Eq(x.Data(), data)
	})
	t.Run("add targets the primary only", func(t *testing.T) {
		ctx, x := x.New(t)

		primary := NewMemStores()
		secondary := NewMemStores().Use("secondary")
		stores := FallbackStores{Primary: primary, Secondary: secondary}

		s := stores.Use("repo")
		m, err := s.Add(ctx, Meta{}, x.Reader())
		x.NoError(err)

		_, err = statMeta(ctx, primary.Use("repo"), m.Digest)
		x.NoError(err)

		_, err = statMeta(ctx, secondary, m.Digest)
		x.ErrorIs(err, ErrNotExist)
	})
}

func TestAllowDuplicates(t *testing.T) {
	ctx, x := x.New(t)
	s := AllowDuplicates(NewMemStores().Use("t"))

	m, err := s.Add(ctx, Meta{}, x.Reader())
	x.NoError(err)

	// A duplicate Add must be reported as success, not ErrAlreadyExists.
	m2, err := s.Add(ctx, Meta{}, x.Reader())
	x.NoError(err)
	x.Eq(m.Digest, m2.Digest)
}

func TestCheckExistence(t *testing.T) {
	ctx, x := x.New(t)
	s := CheckExistence(NewMemStores().Use("t"))

	m, err := s.Add(ctx, Meta{}, x.Reader())
	x.NoError(err)

	_, err = s.Add(ctx, Meta{Digest: m.Digest}, x.Reader())
	x.ErrorIs(err, ErrAlreadyExists)
}
