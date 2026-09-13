package flob

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
)

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
				wrappedSource := AllowDuplicates(CheckExistence(PrepareDigest(&CacheStore{Primary: source, Origin: stores.Use("origin")}, Canonical)))
				wrappedTarget := AllowDuplicates(FallbackStore{Primary: target, Secondary: stores.Use("secondary")})
				if _, err := requireLinker(t, wrappedTarget).Link(t.Context(), added.Digest, wrappedSource); err != nil {
					t.Fatal(err)
				}
				for _, wrapper := range []Store{&CacheStore{Primary: stores.Use("missing"), Origin: source}, FallbackStore{Primary: stores.Use("missing"), Secondary: source}} {
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
	for _, store := range []Store{&CacheStore{Primary: UnimplementedStore{}, Origin: source}, FallbackStore{Primary: UnimplementedStore{}, Secondary: source}} {
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
