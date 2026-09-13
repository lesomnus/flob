package flob

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/lesomnus/flob/internal/x"
	"github.com/opencontainers/go-digest"
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
	for _, algo := range []digest.Algorithm{digest.SHA384, digest.SHA512} {
		t.Run(string(algo)+" lifecycle", func(t *testing.T) {
			ctx, x := x.New(t)
			s := new_store(t)
			d := Digest(algo.FromBytes(x.Data()))
			m, err := s.Add(ctx, Meta{Digest: d, Labels: Labels{"Foo": {"bar"}}}, x.Reader())
			x.NoError(err)
			x.Eq(d, m.Digest)
			got, err := statMeta(ctx, s, d)
			x.NoError(err)
			x.Eq(d, got.Digest)
			x.Eq(int64(len(x.Data())), got.Size)
			x.Eq(m.Labels, got.Labels)
			r, opened, err := s.Open(ctx, d)
			x.NoError(err)
			defer r.Close()
			data, err := io.ReadAll(r)
			x.NoError(err)
			x.Eq(x.Data(), data)
			x.Eq(d, opened.Digest())
			_, err = s.Add(ctx, Meta{Digest: d}, x.Reader())
			x.ErrorIs(err, ErrAlreadyExists)
			x.NoError(s.Label(ctx, d, Labels{"Foo": {"updated"}}))
			got, err = statMeta(ctx, s, d)
			x.NoError(err)
			x.Eq(Labels{"Foo": {"updated"}}, got.Labels)
			canonical, err := s.Add(ctx, Meta{}, x.Reader())
			x.NoError(err)
			x.Eq(DigestFromBytes(x.Data()), canonical.Digest)
			x.NoError(s.Erase(ctx, d))
			_, err = statMeta(ctx, s, d)
			x.ErrorIs(err, ErrNotExist)
			_, err = statMeta(ctx, s, canonical.Digest)
			x.NoError(err)
		})
		t.Run(string(algo)+" mismatch", func(t *testing.T) {
			ctx, x := x.New(t)
			s := new_store(t)
			d := Digest(algo.FromBytes([]byte("different content")))
			_, err := s.Add(ctx, Meta{Digest: d}, x.Reader())
			x.ErrorIs(err, ErrDigestMismatch)
			_, err = statMeta(ctx, s, d)
			x.ErrorIs(err, ErrNotExist)
			_, err = statMeta(ctx, s, Digest(algo.FromBytes(x.Data())))
			x.ErrorIs(err, ErrNotExist)
		})
	}
	t.Run("invalid digest returns an error", func(t *testing.T) {
		ctx, x := x.New(t)
		s := new_store(t)
		for _, d := range []Digest{"unknown:abcd", "sha512:abcd", "malformed"} {
			if _, err := s.Add(ctx, Meta{Digest: d}, x.Reader()); err == nil {
				t.Errorf("Add with invalid digest %q succeeded", d)
			}
		}
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

func requireLinker(t *testing.T, store Store) Linker {
	t.Helper()
	linker, ok := AsLinker(store)
	if !ok {
		t.Fatalf("%T does not expose Linker", store)
	}
	return linker
}
func linkMeta(t *testing.T, store Store, d Digest) Meta {
	t.Helper()
	m, err := statMeta(t.Context(), store, d)
	if err != nil {
		t.Fatal(err)
	}
	return m
}
func requireMissingLink(t *testing.T, store Store, d Digest) {
	t.Helper()
	if _, err := store.Stat(t.Context(), d); !errors.Is(err, ErrNotExist) {
		t.Fatalf("destination Stat = %v; want ErrNotExist", err)
	}
}

func TestLinkStore(t *testing.T) {
	factories := map[string]func(*testing.T) Stores{
		"memory": func(t *testing.T) Stores { return NewMemStores() },
		"os":     func(t *testing.T) Stores { return NewOsStores(t.TempDir()) },
		"s3":     func(t *testing.T) Stores { s, _ := newMockS3Stores(t); return s },
	}
	for name, newStores := range factories {
		t.Run(name, func(t *testing.T) {
			for _, algo := range []digest.Algorithm{digest.SHA256, digest.SHA512} {
				t.Run(string(algo)+" lifecycle", func(t *testing.T) {
					stores := newStores(t)
					source := stores.Use("source")
					const content = "linked content"
					d := Digest(algo.FromString(content))
					labels := Labels{"Owner": {"source"}, "Multi": {"one", "two"}}
					added, err := source.Add(t.Context(), Meta{Digest: d, Labels: labels}, strings.NewReader(content))
					if err != nil {
						t.Fatal(err)
					}
					// Compare with stored metadata: backends may normalize labels on write.
					added = linkMeta(t, source, d)
					ids := []string{"destination", "../escape", "a/b", "a%2Fb", "", "한글"}
					for _, id := range ids {
						target := stores.Use(id)
						linked, err := requireLinker(t, target).Link(t.Context(), d, source)
						if err != nil {
							t.Fatalf("Link %q: %v", id, err)
						}
						if linked.Digest != d || linked.Size != added.Size || !reflect.DeepEqual(linked.Labels, added.Labels) {
							t.Fatalf("Link metadata = %#v; want %#v", linked, added)
						}
						linked.Labels["Owner"][0] = "mutated result"
						if got := linkMeta(t, target, d).Labels["Owner"][0]; got != "source" {
							t.Fatalf("returned metadata aliases target labels: %q", got)
						}
					}
					if err := stores.Use(ids[0]).Label(t.Context(), d, Labels{"Owner": {"target"}}); err != nil {
						t.Fatal(err)
					}
					if got := linkMeta(t, source, d).Labels["Owner"][0]; got != "source" {
						t.Fatalf("source labels changed with target: %q", got)
					}
					if err := source.Label(t.Context(), d, Labels{"Owner": {"updated source"}}); err != nil {
						t.Fatal(err)
					}
					for i, id := range ids {
						want := "source"
						if i == 0 {
							want = "target"
						}
						if got := linkMeta(t, stores.Use(id), d).Labels["Owner"][0]; got != want {
							t.Fatalf("target %q labels = %q; want %q", id, got, want)
						}
					}
					if err := source.Erase(t.Context(), d); err != nil {
						t.Fatal(err)
					}
					requireMissingLink(t, source, d)
					for _, id := range ids {
						r, _, err := stores.Use(id).Open(t.Context(), d)
						if err != nil {
							t.Fatalf("Open %q after source erase: %v", id, err)
						}
						got, err := io.ReadAll(r)
						r.Close()
						if err != nil || string(got) != content {
							t.Fatalf("target %q bytes = %q, %v", id, got, err)
						}
					}
				})
			}
			t.Run("duplicate self and missing source", func(t *testing.T) {
				stores := newStores(t)
				source, target := stores.Use("source"), stores.Use("target")
				added, err := source.Add(t.Context(), Meta{Labels: Labels{"Owner": {"source"}}}, strings.NewReader("content"))
				if err != nil {
					t.Fatal(err)
				}
				if _, err := requireLinker(t, source).Link(t.Context(), added.Digest, source); !errors.Is(err, ErrAlreadyExists) {
					t.Fatalf("self Link = %v", err)
				}
				if _, err := requireLinker(t, target).Link(t.Context(), added.Digest, source); err != nil {
					t.Fatal(err)
				}
				if err := target.Label(t.Context(), added.Digest, Labels{"Owner": {"target"}}); err != nil {
					t.Fatal(err)
				}
				if _, err := requireLinker(t, target).Link(t.Context(), added.Digest, source); !errors.Is(err, ErrAlreadyExists) {
					t.Fatalf("duplicate Link = %v", err)
				}
				if got := linkMeta(t, target, added.Digest).Labels["Owner"][0]; got != "target" {
					t.Fatalf("duplicate replaced labels: %q", got)
				}
				missing := stores.Use("missing")
				if _, err := requireLinker(t, target).Link(t.Context(), added.Digest, missing); !errors.Is(err, ErrNotExist) {
					t.Fatalf("missing source with existing target = %v", err)
				}
				emptyTarget := stores.Use("empty target")
				if _, err := requireLinker(t, emptyTarget).Link(t.Context(), added.Digest, missing); !errors.Is(err, ErrNotExist) {
					t.Fatalf("globally present but source absent = %v", err)
				}
				requireMissingLink(t, emptyTarget, added.Digest)
			})
			t.Run("invalid canceled incompatible", func(t *testing.T) {
				stores := newStores(t)
				source, target := stores.Use("source"), stores.Use("target")
				added, err := source.Add(t.Context(), Meta{}, strings.NewReader("content"))
				if err != nil {
					t.Fatal(err)
				}
				linker := requireLinker(t, target)
				for _, d := range []Digest{"", "garbage", "sha256:../invalid"} {
					if _, err := linker.Link(t.Context(), d, source); err == nil {
						t.Fatalf("invalid digest %q succeeded", d)
					}
				}
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				if _, err := linker.Link(ctx, added.Digest, source); !errors.Is(err, context.Canceled) {
					t.Fatalf("canceled Link = %v", err)
				}
				requireMissingLink(t, target, added.Digest)
				foreign := newStores(t).Use("source")
				if _, err := foreign.Add(t.Context(), Meta{}, strings.NewReader("content")); err != nil {
					t.Fatal(err)
				}
				for _, from := range []Store{nil, UnimplementedStore{}, foreign} {
					if _, err := linker.Link(t.Context(), added.Digest, from); !errors.Is(err, ErrIncompatibleStore) {
						t.Fatalf("source %T Link = %v; want ErrIncompatibleStore", from, err)
					}
					requireMissingLink(t, target, added.Digest)
				}
			})
			t.Run("decorators and primary-only sources", func(t *testing.T) {
				stores := newStores(t)
				source, target := stores.Use("source"), stores.Use("target")
				added, err := source.Add(t.Context(), Meta{}, strings.NewReader("content"))
				if err != nil {
					t.Fatal(err)
				}
				wrappedSource := AllowDuplicates(CheckExistence(PrepareDigest(CacheStore{Primary: source, Origin: stores.Use("origin")}, Canonical)))
				wrappedTarget := AllowDuplicates(FallbackStore{Primary: target, Secondary: stores.Use("secondary")})
				if _, err := requireLinker(t, wrappedTarget).Link(t.Context(), added.Digest, wrappedSource); err != nil {
					t.Fatal(err)
				}
				for _, wrapper := range []Store{CacheStore{Primary: stores.Use("missing"), Origin: source}, FallbackStore{Primary: stores.Use("missing"), Secondary: source}} {
					fresh := stores.Use("fresh")
					if _, err := requireLinker(t, fresh).Link(t.Context(), added.Digest, wrapper); !errors.Is(err, ErrNotExist) {
						t.Fatalf("origin-only source Link = %v", err)
					}
					requireMissingLink(t, fresh, added.Digest)
				}
			})
		})
	}
}

func TestLinkerDiscovery(t *testing.T) {
	for _, store := range []Store{nil, UnimplementedStore{}, HttpStore{}} {
		if _, ok := AsLinker(store); ok {
			t.Fatalf("unexpected Linker for %T", store)
		}
	}
	source := NewMemStores().Use("source")
	for _, store := range []Store{CacheStore{Primary: UnimplementedStore{}, Origin: source}, FallbackStore{Primary: UnimplementedStore{}, Secondary: source}} {
		if _, ok := AsLinker(store); ok {
			t.Fatal("secondary Linker was exposed")
		}
	}
}

func TestOsLinkEquivalentRoots(t *testing.T) {
	root := t.TempDir()
	source := NewOsStores(root).Use("source")
	added, err := source.Add(t.Context(), Meta{}, strings.NewReader("content"))
	if err != nil {
		t.Fatal(err)
	}
	target := NewOsStores(root + string(filepath.Separator) + ".").Use("target")
	if _, err := requireLinker(t, target).Link(t.Context(), added.Digest, source); err != nil {
		t.Fatal(err)
	}
	if got := linkMeta(t, target, added.Digest); got.Size != added.Size {
		t.Fatalf("linked size = %d", got.Size)
	}
}

func walkTestFactories() map[string]newStoresFn {
	return map[string]newStoresFn{
		"memory": func(t *testing.T) Stores { return NewMemStores() },
		"os":     func(t *testing.T) Stores { return NewOsStores(t.TempDir()) },
		"s3":     func(t *testing.T) Stores { s, _ := newMockS3Stores(t); return s },
	}
}

func TestWalkContract(t *testing.T) {
	for name, factory := range walkTestFactories() {
		t.Run(name, func(t *testing.T) {
			stores := factory(t)
			source := stores.Use("source")
			walker, ok := AsWalker(source)
			if !ok {
				t.Fatal("missing Walker")
			}
			missing, ok := AsWalker(stores.Use("missing"))
			if !ok {
				t.Fatal("missing Walker on empty namespace")
			}
			for info, err := range missing.Walk(t.Context()) {
				t.Fatalf("empty Walk yielded %v, %v", info, err)
			}
			expected := map[Digest]Meta{}
			for i, algo := range []digest.Algorithm{digest.SHA256, digest.SHA384, digest.SHA512} {
				content := strings.Repeat(fmt.Sprint(i), 5+i)
				m, err := source.Add(t.Context(), Meta{Digest: Digest(algo.FromString(content)), Labels: Labels{"Owner": {fmt.Sprint(i)}}}, strings.NewReader(content))
				if err != nil {
					t.Fatal(err)
				}
				expected[m.Digest] = m
			}
			if _, err := stores.Use("other").Add(t.Context(), Meta{}, strings.NewReader("not in source")); err != nil {
				t.Fatal(err)
			}
			seq := walker.Walk(t.Context())
			// Breaking one iteration must not consume state shared with later iterations.
			first := 0
			for info, err := range seq {
				if err != nil || info == nil {
					t.Fatalf("early Walk = %v, %v", info, err)
				}
				first++
				break
			}
			if first != 1 {
				t.Fatal("early Walk yielded nothing")
			}
			for range 2 {
				seen := map[Digest]bool{}
				for info, err := range seq {
					if err != nil {
						t.Fatal(err)
					}
					if info == nil {
						t.Fatal("nil Info")
					}
					m, ok := expected[info.Digest()]
					if !ok || seen[info.Digest()] {
						t.Fatalf("unexpected or duplicate digest %s", info.Digest())
					}
					seen[info.Digest()] = true
					labels, err := info.Labels(t.Context())
					if err != nil || info.Size() != m.Size || labels.Get("Owner") != m.Labels.Get("Owner") {
						t.Fatalf("Info mismatch: %s %d %v, %v", info.Digest(), info.Size(), labels, err)
					}
				}
				if len(seen) != len(expected) {
					t.Fatalf("walk count = %d", len(seen))
				}
			}
			for d := range expected {
				if err := source.Erase(t.Context(), d); err != nil {
					t.Fatal(err)
				}
			}
			for info, err := range walker.Walk(t.Context()) {
				t.Fatalf("erased Walk yielded %v, %v", info, err)
			}
		})
	}
}

func TestNamespacesContract(t *testing.T) {
	for name, factory := range walkTestFactories() {
		t.Run(name, func(t *testing.T) {
			stores := factory(t)
			ns, ok := AsNamespacer(stores)
			if !ok {
				t.Fatal("missing Namespacer")
			}
			stores.Use("never written")
			for id, err := range ns.Namespaces(t.Context()) {
				t.Fatalf("empty Namespaces = %q, %v", id, err)
			}
			expected := map[string]bool{"": true, "a/b": true, "~": true, "plain": true, "한글": true}
			for id := range expected {
				if _, err := stores.Use(id).Add(t.Context(), Meta{}, strings.NewReader("content")); err != nil {
					t.Fatal(err)
				}
			}
			erased := stores.Use("erased")
			m, err := erased.Add(t.Context(), Meta{}, strings.NewReader("content"))
			if err != nil {
				t.Fatal(err)
			}
			if err := erased.Erase(t.Context(), m.Digest); err != nil {
				t.Fatal(err)
			}
			seq := ns.Namespaces(t.Context())
			for _, err := range seq {
				if err != nil {
					t.Fatal(err)
				}
				break
			}
			for range 2 {
				seen := map[string]bool{}
				for id, err := range seq {
					if err != nil {
						t.Fatal(err)
					}
					if !expected[id] || seen[id] {
						t.Fatalf("unexpected or duplicate namespace %q", id)
					}
					seen[id] = true
				}
				if len(seen) != len(expected) {
					t.Fatalf("namespace count = %d", len(seen))
				}
			}
		})
	}
}

func TestEnumerationCancellation(t *testing.T) {
	for name, factory := range walkTestFactories() {
		t.Run(name, func(t *testing.T) {
			stores := factory(t)
			for _, id := range []string{"a", "b", "c"} {
				for _, content := range []string{"one", "two", "three"} {
					if _, err := stores.Use(id).Add(t.Context(), Meta{}, strings.NewReader(content)); err != nil {
						t.Fatal(err)
					}
				}
			}
			walker, _ := AsWalker(stores.Use("a"))
			ns, _ := AsNamespacer(stores)
			for _, before := range []bool{true, false} {
				ctx, cancel := context.WithCancel(t.Context())
				if before {
					cancel()
				}
				count, errorCount := 0, 0
				for info, err := range walker.Walk(ctx) {
					if err != nil {
						if !errors.Is(err, context.Canceled) || info != nil {
							t.Fatalf("Walk cancellation: %v, %v", info, err)
						}
						errorCount++
					} else {
						count++
						cancel()
					}
				}
				cancel()
				want := 1
				if before {
					want = 0
				}
				if count != want || errorCount != 1 {
					t.Fatalf("Walk canceled: %d entries, %d errors", count, errorCount)
				}
				ctx, cancel = context.WithCancel(t.Context())
				if before {
					cancel()
				}
				count, errorCount = 0, 0
				for id, err := range ns.Namespaces(ctx) {
					if err != nil {
						if !errors.Is(err, context.Canceled) {
							t.Fatalf("Namespaces cancellation: %q, %v", id, err)
						}
						errorCount++
					} else {
						count++
						cancel()
					}
				}
				cancel()
				if count != want || errorCount != 1 {
					t.Fatalf("Namespaces canceled: %d entries, %d errors", count, errorCount)
				}
			}
		})
	}
}

type walkStoreWrapper struct{ Store }

func (s walkStoreWrapper) Unwrap() Store { return s.Store }

type walkStoresWrapper struct{ Stores }

func (s walkStoresWrapper) Unwrap() Stores { return s.Stores }

func TestEnumerationDiscovery(t *testing.T) {
	primary := NewMemStores()
	origin := NewMemStores()
	p := primary.Use("a")
	for _, wrapped := range []Store{AllowDuplicates(CheckExistence(p)), walkStoreWrapper{p}, &CacheStore{Primary: p, Origin: origin.Use("a")}, &FallbackStore{Primary: p, Secondary: origin.Use("a")}} {
		got, ok := AsWalker(wrapped)
		if !ok || got != p.(Walker) {
			t.Fatalf("AsWalker(%T) = %v, %v", wrapped, got, ok)
		}
	}
	for _, wrapped := range []Stores{walkStoresWrapper{primary}, &CacheStores{Primary: primary, Origin: origin}, &FallbackStores{Primary: primary, Secondary: origin.Use("a")}} {
		got, ok := AsNamespacer(wrapped)
		if !ok || got != primary {
			t.Fatalf("AsNamespacer(%T) = %v, %v", wrapped, got, ok)
		}
	}
	for _, store := range []Store{nil, UnimplementedStore{}, HttpStores{}.Use("a"), walkStoreWrapper{nil}} {
		if got, ok := AsWalker(store); ok || got != nil {
			t.Fatalf("unexpected Walker for %T", store)
		}
	}
	for _, stores := range []Stores{nil, UnimplementedStores{}, HttpStores{}, walkStoresWrapper{nil}} {
		if got, ok := AsNamespacer(stores); ok || got != nil {
			t.Fatalf("unexpected Namespacer for %T", stores)
		}
	}
}
