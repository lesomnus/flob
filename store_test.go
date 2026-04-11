package flob

import (
	"io"
	"testing"

	"github.com/lesomnus/flob/internal/x"
)

var digest_nil = Digest("0000000000000000000000000000000000000000000000000000000000000000")

type newStoresFn func(t *testing.T) Stores

func testStore(t *testing.T, new_stores newStoresFn) {
	t.Helper()

	new_store := func(t *testing.T) Store {
		t.Helper()
		stores := new_stores(t)
		return stores.Use("test")
	}

	t.Run("add then get", func(t *testing.T) {
		ctx, x := x.New(t)
		s := new_store(t)

		labels := Labels{"Foo": {"bar"}}
		added, err := s.Add(ctx, Meta{Labels: labels}, x.Reader())
		x.NoError(err)
		x.Eq(x.Digest(), string(added.Digest))
		x.Contains(added.Labels, "Foo")
		x.Len(added.Labels["Foo"], 1)
		x.Eq("bar", added.Labels["Foo"][0])

		got, err := s.Get(ctx, added.Digest)
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
	t.Run("get missing returns ErrNotExist", func(t *testing.T) {
		ctx, x := x.New(t)
		s := new_store(t)

		_, err := s.Get(ctx, digest_nil)
		x.ErrorIs(err, ErrNotExist)
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

		labels_init := Labels{"Content-Type": {"text/plain"}, "Version": {"1"}}
		added, err := s.Add(ctx, Meta{Labels: labels_init}, x.Reader())
		x.NoError(err)

		got, err := s.Get(ctx, added.Digest)
		x.NoError(err)
		x.Eq(labels_init.Get("Content-Type"), got.Labels.Get("Content-Type"))
		x.Eq(labels_init.Get("Version"), got.Labels.Get("Version"))

		labels_new := Labels{"Content-Type": {"application/json"}, "Version": {"2"}, "Author": {"test"}}
		err = s.Label(ctx, added.Digest, labels_new)
		x.NoError(err)

		got, err = s.Get(ctx, added.Digest)
		x.NoError(err)
		x.Eq(labels_new.Get("Content-Type"), got.Labels.Get("Content-Type"))
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
	t.Run("get across stores returns ErrNotExist", func(t *testing.T) {
		ctx, x := x.New(t)

		stores := new_stores(t)
		s1 := stores.Use("a")
		s2 := stores.Use("b")

		added, err := s1.Add(ctx, Meta{}, x.Reader())
		x.NoError(err)

		_, err = s2.Get(ctx, added.Digest)
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

		_, err = s1.Get(ctx, added.Digest)
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

		labels_a := Labels{"Content-Type": {"text/plain"}, "Repo": {"a"}}
		labels_b := Labels{"Content-Type": {"application/json"}, "Repo": {"b"}}

		err = s1.Label(ctx, m1.Digest, labels_a)
		x.NoError(err)
		err = s2.Label(ctx, m2.Digest, labels_b)
		x.NoError(err)

		got_a, err := s1.Get(ctx, m1.Digest)
		x.NoError(err)
		got_b, err := s2.Get(ctx, m2.Digest)
		x.NoError(err)

		x.Eq(labels_a.Get("Content-Type"), got_a.Labels.Get("Content-Type"))
		x.Eq(labels_a.Get("Repo"), got_a.Labels.Get("Repo"))
		x.Eq(labels_b.Get("Content-Type"), got_b.Labels.Get("Content-Type"))
		x.Eq(labels_b.Get("Repo"), got_b.Labels.Get("Repo"))
	})
}
