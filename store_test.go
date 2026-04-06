package flat_test

import (
	"bytes"
	"io"
	"testing"

	"github.com/lesomnus/flat"
	"github.com/lesomnus/flat/internal/x"
)

var digest_nil = flat.Digest("0000000000000000000000000000000000000000000000000000000000000000")

type newStoreFn func(t *testing.T) flat.Store

func testStore(t *testing.T, new_store newStoreFn) {
	t.Helper()

	data := []byte("Royale with Cheese")
	new_reader := func() io.Reader {
		return bytes.NewReader(data)
	}

	t.Run("add then get", func(t *testing.T) {
		x := x.New(t)
		s := new_store(t)

		labels := flat.Labels{"Content-Type": {"text/plain"}}
		added, err := s.Add(t.Context(), flat.Meta{Labels: labels}, new_reader())
		x.NotError(err)

		got, err := s.Get(t.Context(), added.Digest)
		x.NotError(err)
		x.Eq(added.Digest, got.Digest)
		x.Eq(int64(len(data)), got.Size)
	})
	t.Run("add duplicate returns ErrAlreadyExists", func(t *testing.T) {
		x := x.New(t)
		s := new_store(t)

		_, err := s.Add(t.Context(), flat.Meta{}, new_reader())
		x.NotError(err)

		_, err = s.Add(t.Context(), flat.Meta{}, new_reader())
		x.ErrorIs(err, flat.ErrAlreadyExists)
	})
	t.Run("get missing returns ErrNotExist", func(t *testing.T) {
		x := x.New(t)
		s := new_store(t)

		_, err := s.Get(t.Context(), digest_nil)
		x.ErrorIs(err, flat.ErrNotExist)
	})
	t.Run("open returns same content after add", func(t *testing.T) {
		x := x.New(t)
		s := new_store(t)

		m, err := s.Add(t.Context(), flat.Meta{}, new_reader())
		x.NotError(err)

		r, _, err := s.Open(t.Context(), m.Digest)
		x.NotError(err)
		defer r.Close()

		got, err := io.ReadAll(r)
		x.NotError(err)
		x.Eq(data, got)
	})
	t.Run("label updates blob labels", func(t *testing.T) {
		x := x.New(t)
		s := new_store(t)

		labels_init := flat.Labels{"Content-Type": {"text/plain"}, "Version": {"1"}}
		added, err := s.Add(t.Context(), flat.Meta{Labels: labels_init}, new_reader())
		x.NotError(err)

		got, err := s.Get(t.Context(), added.Digest)
		x.NotError(err)
		x.Eq(labels_init.Get("Content-Type"), got.Labels.Get("Content-Type"))
		x.Eq(labels_init.Get("Version"), got.Labels.Get("Version"))

		labels_new := flat.Labels{"Content-Type": {"application/json"}, "Version": {"2"}, "Author": {"test"}}
		err = s.Label(t.Context(), added.Digest, labels_new)
		x.NotError(err)

		got, err = s.Get(t.Context(), added.Digest)
		x.NotError(err)
		x.Eq(labels_new.Get("Content-Type"), got.Labels.Get("Content-Type"))
		x.Eq(labels_new.Get("Version"), got.Labels.Get("Version"))
		x.Eq(labels_new.Get("Author"), got.Labels.Get("Author"))
	})
	t.Run("label on missing blob returns ErrNotExist", func(t *testing.T) {
		x := x.New(t)
		s := new_store(t)

		labels := flat.Labels{"foo": {"bar"}}
		err := s.Label(t.Context(), digest_nil, labels)
		x.ErrorIs(err, flat.ErrNotExist)
	})
}
