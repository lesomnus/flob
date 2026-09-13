package flob

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/lesomnus/flob/internal/x"
)

var digest_nil = Digest("sha256:0000000000000000000000000000000000000000000000000000000000000000")

type newStoresFn func(t *testing.T) Stores

func testStore(t *testing.T, new_stores newStoresFn) {
	t.Helper()

	new_store := func(t *testing.T) Store {
		t.Helper()
		stores := new_stores(t)
		return stores.Use("test")
	}

	t.Run("add then stat", func(t *testing.T) {
		ctx, x := x.New(t)
		s := new_store(t)

		labels := Labels{"Foo": {"bar"}}
		added, err := s.Add(ctx, Meta{Labels: labels}, x.Reader())
		x.NoError(err)
		x.Eq(x.Digest(), string(added.Digest))
		x.Contains(added.Labels, "Foo")
		x.Len(added.Labels["Foo"], 1)
		x.Eq("bar", added.Labels["Foo"][0])

		got, err := statMeta(ctx, s, added.Digest)
		x.NoError(err)
		x.Eq(added.Digest, got.Digest)
		x.Contains(got.Labels, "Foo")
		x.Len(got.Labels["Foo"], 1)
		x.Eq("bar", got.Labels["Foo"][0])
	})
	t.Run("add duplicate returns digest and ErrAlreadyExists", func(t *testing.T) {
		ctx, x := x.New(t)
		s := new_store(t)

		m, err := s.Add(ctx, Meta{}, x.Reader())
		x.NoError(err)

		got, err := s.Add(ctx, Meta{}, x.Reader())
		x.ErrorIs(err, ErrAlreadyExists)
		x.Eq(m.Digest, got.Digest)
	})
	t.Run("stat missing returns ErrNotExist", func(t *testing.T) {
		ctx, x := x.New(t)
		s := new_store(t)

		_, err := statMeta(ctx, s, digest_nil)
		x.ErrorIs(err, ErrNotExist)
	})
	t.Run("add with matching digest succeeds", func(t *testing.T) {
		ctx, x := x.New(t)
		s := new_store(t)

		added, err := s.Add(ctx, Meta{Digest: Digest(x.Digest())}, x.Reader())
		x.NoError(err)
		x.Eq(x.Digest(), string(added.Digest))

		got, err := statMeta(ctx, s, added.Digest)
		x.NoError(err)
		x.Eq(added.Digest, got.Digest)
	})
	t.Run("add with mismatched digest returns ErrDigestMismatch", func(t *testing.T) {
		ctx, x := x.New(t)
		s := new_store(t)

		// digest_nil is a well-formed digest that does not match the content.
		_, err := s.Add(ctx, Meta{Digest: digest_nil}, x.Reader())
		x.ErrorIs(err, ErrDigestMismatch)

		// The failed Add must not have stored anything.
		_, err = statMeta(ctx, s, Digest(x.Digest()))
		x.ErrorIs(err, ErrNotExist)
	})
	t.Run("add pre-supplied digest of existing returns ErrAlreadyExists", func(t *testing.T) {
		ctx, x := x.New(t)
		s := new_store(t)

		m, err := s.Add(ctx, Meta{}, x.Reader())
		x.NoError(err)

		got, err := s.Add(ctx, Meta{Digest: m.Digest}, x.Reader())
		x.ErrorIs(err, ErrAlreadyExists)
		x.Eq(m.Digest, got.Digest)
	})
	t.Run("open content matches the requested digest", func(t *testing.T) {
		ctx, x := x.New(t)
		s := new_store(t)

		m, err := s.Add(ctx, Meta{}, x.Reader())
		x.NoError(err)

		r, _, err := s.Open(ctx, m.Digest)
		x.NoError(err)
		defer r.Close()

		got, err := io.ReadAll(r)
		x.NoError(err)
		x.Eq(m.Digest, DigestFromBytes(got))
	})
	t.Run("erase then re-add succeeds", func(t *testing.T) {
		ctx, x := x.New(t)
		s := new_store(t)

		m, err := s.Add(ctx, Meta{}, x.Reader())
		x.NoError(err)

		err = s.Erase(ctx, m.Digest)
		x.NoError(err)

		_, err = statMeta(ctx, s, m.Digest)
		x.ErrorIs(err, ErrNotExist)

		// Re-adding the same content after erase must succeed, not fail with a leftover.
		m2, err := s.Add(ctx, Meta{}, x.Reader())
		x.NoError(err)
		x.Eq(m.Digest, m2.Digest)

		got, err := statMeta(ctx, s, m2.Digest)
		x.NoError(err)
		x.Eq(m.Digest, got.Digest)
	})
	t.Run("add empty content roundtrips", func(t *testing.T) {
		ctx, x := x.New(t)
		s := new_store(t)

		m, err := s.Add(ctx, Meta{}, bytes.NewReader(nil))
		x.NoError(err)
		x.Eq(int64(0), m.Size)

		r, gm, err := s.Open(ctx, m.Digest)
		x.NoError(err)
		defer r.Close()
		x.Eq(int64(0), gm.Size())

		got, err := io.ReadAll(r)
		x.NoError(err)
		x.Eq(0, len(got))
	})
	t.Run("returned labels are isolated from the stored copy", func(t *testing.T) {
		ctx, x := x.New(t)
		s := new_store(t)

		added, err := s.Add(ctx, Meta{Labels: Labels{"Media-Type": {"text/plain"}}}, x.Reader())
		x.NoError(err)

		// Mutating a returned label must not leak into the store's own copy.
		got, err := statMeta(ctx, s, added.Digest)
		x.NoError(err)
		if vs := got.Labels["Media-Type"]; len(vs) > 0 {
			vs[0] = "MUTATED"
		}

		again, err := statMeta(ctx, s, added.Digest)
		x.NoError(err)
		x.Eq("text/plain", again.Labels.Get("Media-Type"))
	})
	t.Run("open returns same content after add", func(t *testing.T) {
		ctx, x := x.New(t)
		s := new_store(t)

		m, err := s.Add(ctx, Meta{}, x.Reader())
		x.NoError(err)

		r, _, err := s.Open(ctx, m.Digest)
		x.NoError(err)
		defer r.Close()

		got, err := io.ReadAll(r)
		x.NoError(err)
		x.Eq(x.Data(), got)
	})
	t.Run("label updates blob labels", func(t *testing.T) {
		ctx, x := x.New(t)
		s := new_store(t)

		labels_init := Labels{"Media-Type": {"text/plain"}, "Version": {"1"}}
		added, err := s.Add(ctx, Meta{Labels: labels_init}, x.Reader())
		x.NoError(err)

		got, err := statMeta(ctx, s, added.Digest)
		x.NoError(err)
		x.Eq(labels_init.Get("Media-Type"), got.Labels.Get("Media-Type"))
		x.Eq(labels_init.Get("Version"), got.Labels.Get("Version"))

		labels_new := Labels{"Media-Type": {"application/json"}, "Version": {"2"}, "Author": {"test"}}
		err = s.Label(ctx, added.Digest, labels_new)
		x.NoError(err)

		got, err = statMeta(ctx, s, added.Digest)
		x.NoError(err)
		x.Eq(labels_new.Get("Media-Type"), got.Labels.Get("Media-Type"))
		x.Eq(labels_new.Get("Version"), got.Labels.Get("Version"))
		x.Eq(labels_new.Get("Author"), got.Labels.Get("Author"))
	})
	t.Run("label on missing blob returns ErrNotExist", func(t *testing.T) {
		ctx, x := x.New(t)
		s := new_store(t)

		labels := Labels{"foo": {"bar"}}
		err := s.Label(ctx, digest_nil, labels)
		x.ErrorIs(err, ErrNotExist)
	})
	t.Run("erase on missing blob does not return error", func(t *testing.T) {
		ctx, x := x.New(t)
		s := new_store(t)

		err := s.Erase(ctx, digest_nil)
		x.NoError(err)
	})
	t.Run("stat across stores returns ErrNotExist", func(t *testing.T) {
		ctx, x := x.New(t)

		stores := new_stores(t)
		s1 := stores.Use("a")
		s2 := stores.Use("b")

		added, err := s1.Add(ctx, Meta{}, x.Reader())
		x.NoError(err)

		_, err = statMeta(ctx, s2, added.Digest)
		x.ErrorIs(err, ErrNotExist)
	})
	t.Run("open across stores returns ErrNotExist", func(t *testing.T) {
		ctx, x := x.New(t)

		stores := new_stores(t)
		s1 := stores.Use("a")
		s2 := stores.Use("b")

		added, err := s1.Add(ctx, Meta{}, x.Reader())
		x.NoError(err)

		_, _, err = s2.Open(ctx, added.Digest)
		x.ErrorIs(err, ErrNotExist)
	})
	t.Run("label across stores returns ErrNotExist", func(t *testing.T) {
		ctx, x := x.New(t)

		stores := new_stores(t)
		s1 := stores.Use("a")
		s2 := stores.Use("b")

		added, err := s1.Add(ctx, Meta{}, x.Reader())
		x.NoError(err)

		err = s2.Label(ctx, added.Digest, Labels{"foo": {"bar"}})
		x.ErrorIs(err, ErrNotExist)
	})
	t.Run("erase does not remove blob from other store", func(t *testing.T) {
		ctx, x := x.New(t)

		stores := new_stores(t)
		s1 := stores.Use("a")
		s2 := stores.Use("b")

		added, err := s1.Add(ctx, Meta{}, x.Reader())
		x.NoError(err)

		err = s2.Erase(ctx, added.Digest)
		x.NoError(err)

		_, err = statMeta(ctx, s1, added.Digest)
		x.NoError(err)
	})
	t.Run("duplicate check is scoped to each store", func(t *testing.T) {
		ctx, x := x.New(t)

		stores := new_stores(t)
		s1 := stores.Use("a")
		s2 := stores.Use("b")

		_, err := s1.Add(ctx, Meta{}, x.Reader())
		x.NoError(err)
		_, err = s2.Add(ctx, Meta{}, x.Reader())
		x.NoError(err)

		_, err = s1.Add(ctx, Meta{}, x.Reader())
		x.ErrorIs(err, ErrAlreadyExists)
		_, err = s2.Add(ctx, Meta{}, x.Reader())
		x.ErrorIs(err, ErrAlreadyExists)
	})
	t.Run("same digest labels are isolated by store", func(t *testing.T) {
		ctx, x := x.New(t)

		stores := new_stores(t)
		s1 := stores.Use("a")
		s2 := stores.Use("b")

		m1, err := s1.Add(ctx, Meta{}, x.Reader())
		x.NoError(err)
		m2, err := s2.Add(ctx, Meta{}, x.Reader())
		x.NoError(err)
		x.Eq(m1.Digest, m2.Digest)

		labels_a := Labels{"Media-Type": {"text/plain"}, "Repo": {"a"}}
		labels_b := Labels{"Media-Type": {"application/json"}, "Repo": {"b"}}

		err = s1.Label(ctx, m1.Digest, labels_a)
		x.NoError(err)
		err = s2.Label(ctx, m2.Digest, labels_b)
		x.NoError(err)

		got_a, err := statMeta(ctx, s1, m1.Digest)
		x.NoError(err)
		got_b, err := statMeta(ctx, s2, m2.Digest)
		x.NoError(err)

		x.Eq(labels_a.Get("Media-Type"), got_a.Labels.Get("Media-Type"))
		x.Eq(labels_a.Get("Repo"), got_a.Labels.Get("Repo"))
		x.Eq(labels_b.Get("Media-Type"), got_b.Labels.Get("Media-Type"))
		x.Eq(labels_b.Get("Repo"), got_b.Labels.Get("Repo"))
	})
}

// statMeta materializes labels for tests that exercise complete metadata.
func statMeta(ctx context.Context, s Store, d Digest) (Meta, error) {
	info, err := s.Stat(ctx, d)
	if err != nil {
		return Meta{}, err
	}
	return infoMeta(ctx, info)
}
