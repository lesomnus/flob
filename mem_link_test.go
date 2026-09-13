package flob

import (
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
)

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
