package flat

import (
	"io"
	"testing"

	"github.com/lesomnus/flat/internal/x"
)

var digest_nil = Digest("0000000000000000000000000000000000000000000000000000000000000000")

type newStoreFn func(t *testing.T) Store

func testStore(t *testing.T, new_store newStoreFn) {
	t.Helper()

	t.Run("add then get", func(t *testing.T) {
		ctx, x := x.New(t)
		s := new_store(t)

		labels := Labels{"Content-Type": {"text/plain"}}
		added, err := s.Add(ctx, Meta{Labels: labels}, x.Reader())
		x.NoError(err)

		got, err := s.Get(ctx, added.Digest)
		x.NoError(err)
		x.Eq(added.Digest, got.Digest)
		x.Eq(int64(len(x.Data())), got.Size)
	})
	t.Run("add duplicate returns ErrAlreadyExists", func(t *testing.T) {
		ctx, x := x.New(t)
		s := new_store(t)

		_, err := s.Add(ctx, Meta{}, x.Reader())
		x.NoError(err)

		_, err = s.Add(ctx, Meta{}, x.Reader())
		x.ErrorIs(err, ErrAlreadyExists)
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
}
