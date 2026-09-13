package flob

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	digest "github.com/opencontainers/go-digest"
)

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
