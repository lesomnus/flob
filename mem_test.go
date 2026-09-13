package flob

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/lesomnus/flob/internal/x"
)

func TestMemStore(t *testing.T) {
	t.Run("contract", func(t *testing.T) {
		testStore(t, func(t *testing.T) Stores {
			t.Helper()
			return NewMemStores()
		})
	})

	t.Run("concurrent add and erase keeps refcount consistent", func(t *testing.T) {
		ctx, x := x.New(t)
		stores := NewMemStores()

		d := DigestFromBytes(x.Data())

		const n = 24
		var wg sync.WaitGroup
		for i := range n {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				id := fmt.Sprintf("s-%d", i)
				stores.Use(id).Add(ctx, Meta{}, x.Reader())
				stores.Use(id).Erase(ctx, d)
			}(i)
		}
		wg.Wait()

		// After every store erased its reference, nothing sees the blob.
		for i := range n {
			_, err := statMeta(ctx, stores.Use(fmt.Sprintf("s-%d", i)), d)
			x.ErrorIs(err, ErrNotExist)
		}

		// A fresh add still works: the global blob entry was cleaned up, not corrupted or
		// left dangling with a stale refcount.
		m, err := stores.Use("fresh").Add(ctx, Meta{}, x.Reader())
		x.NoError(err)
		x.Eq(d, m.Digest)
	})
}

func TestMemLinkSharesBlob(t *testing.T) {
	stores := NewMemStores()
	source := stores.Use("source").(*MemStore)
	target := stores.Use("target").(*MemStore)
	m, err := source.Add(t.Context(), Meta{Labels: Labels{"Owner": {"source"}}}, strings.NewReader("content"))
	if err != nil {
		t.Fatal(err)
	}
	linked, err := target.Link(t.Context(), m.Digest, source)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := source.es.Load(m.Digest)
	b, _ := target.es.Load(m.Digest)
	if a.(*memEntry).blob != b.(*memEntry).blob {
		t.Fatal("link copied blob")
	}
	linked.Labels["Owner"][0] = "returned mutation"
	if err := source.Label(t.Context(), m.Digest, Labels{"Owner": {"new"}}); err != nil {
		t.Fatal(err)
	}
	info, err := target.Stat(t.Context(), m.Digest)
	if err != nil {
		t.Fatal(err)
	}
	labels, err := info.Labels(t.Context())
	if err != nil || labels.Get("Owner") != "source" {
		t.Fatalf("labels = %v, %v", labels, err)
	}
	if err := source.Erase(t.Context(), m.Digest); err != nil {
		t.Fatal(err)
	}
	if err := target.Erase(t.Context(), m.Digest); err != nil {
		t.Fatal(err)
	}
	if _, ok := stores.bs.Load(m.Digest); ok {
		t.Fatal("last erase retained shared blob")
	}
}

func TestMemLinkConcurrentSourceErase(t *testing.T) {
	for range 200 {
		stores := NewMemStores()
		source := stores.Use("source").(*MemStore)
		target := stores.Use("target").(*MemStore)
		m, err := source.Add(t.Context(), Meta{}, strings.NewReader("content"))
		if err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		var linkErr error
		var wg sync.WaitGroup
		wg.Go(func() { <-start; _, linkErr = target.Link(t.Context(), m.Digest, source) })
		wg.Go(func() {
			<-start
			if err := source.Erase(t.Context(), m.Digest); err != nil {
				t.Error(err)
			}
		})
		close(start)
		wg.Wait()
		if linkErr != nil && !errors.Is(linkErr, ErrNotExist) {
			t.Fatal(linkErr)
		}
		if linkErr == nil {
			r, _, err := target.Open(t.Context(), m.Digest)
			if err != nil {
				t.Fatal(err)
			}
			data, err := io.ReadAll(r)
			r.Close()
			if err != nil || string(data) != "content" {
				t.Fatalf("linked bytes = %q, %v", data, err)
			}
			if _, ok := stores.bs.Load(m.Digest); !ok {
				t.Fatal("live destination lost global blob")
			}
		}
		if err := target.Erase(t.Context(), m.Digest); err != nil {
			t.Fatal(err)
		}
		if _, ok := stores.bs.Load(m.Digest); ok {
			t.Fatal("reference leak")
		}
	}
}

func TestMemLinkConcurrentDestinations(t *testing.T) {
	stores := NewMemStores()
	source := stores.Use("source").(*MemStore)
	target := stores.Use("target").(*MemStore)
	m, err := source.Add(t.Context(), Meta{}, strings.NewReader("content"))
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 32)
	for range 32 {
		wg.Go(func() { _, err := target.Link(t.Context(), m.Digest, source); results <- err })
	}
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		} else if !errors.Is(err, ErrAlreadyExists) {
			t.Fatal(err)
		}
	}
	if successes != 1 {
		t.Fatalf("successful links = %d", successes)
	}
	v, _ := stores.bs.Load(m.Digest)
	blob := v.(*memBlob)
	blob.mu.Lock()
	refs := blob.refs
	blob.mu.Unlock()
	if refs != 2 {
		t.Fatalf("references = %d", refs)
	}
	if err := source.Erase(t.Context(), m.Digest); err != nil {
		t.Fatal(err)
	}
	if err := target.Erase(t.Context(), m.Digest); err != nil {
		t.Fatal(err)
	}
	if _, ok := stores.bs.Load(m.Digest); ok {
		t.Fatal("reference leak")
	}
}

func TestMemWalkLazyLabelsAndMutation(t *testing.T) {
	s := NewMemStores().Use("a").(*MemStore)
	m, err := s.Add(t.Context(), Meta{Labels: Labels{"Version": {"old"}}}, strings.NewReader("content"))
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for info, err := range s.Walk(t.Context()) {
		if err != nil {
			t.Fatal(err)
		}
		count++
		if err := s.Label(t.Context(), m.Digest, Labels{"Version": {"new"}}); err != nil {
			t.Fatal(err)
		}
		labels, err := info.Labels(t.Context())
		if err != nil || labels.Get("Version") != "new" {
			t.Fatalf("lazy labels = %v, %v", labels, err)
		}
		if err := s.Erase(t.Context(), m.Digest); err != nil {
			t.Fatal(err)
		}
	}
	if count != 1 {
		t.Fatalf("entries = %d", count)
	}
}

func TestMemEnumerationCancelLastItem(t *testing.T) {
	stores := NewMemStores()
	s := stores.Use("a").(*MemStore)
	if _, err := s.Add(t.Context(), Meta{}, strings.NewReader("content")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	errs := 0
	for _, err := range s.Walk(ctx) {
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			errs++
		} else {
			cancel()
		}
	}
	if errs != 1 {
		t.Fatalf("Walk errors = %d", errs)
	}
	ctx, cancel = context.WithCancel(t.Context())
	defer cancel()
	errs = 0
	for _, err := range stores.Namespaces(ctx) {
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			errs++
		} else {
			cancel()
		}
	}
	if errs != 1 {
		t.Fatalf("Namespaces errors = %d", errs)
	}
}
