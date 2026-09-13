package flob

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestOsLinkSharesSourceInode(t *testing.T) {
	stores := NewOsStores(t.TempDir())
	source := stores.Use("source").(OsStore)
	target := stores.Use("target").(OsStore)
	m, err := source.Add(t.Context(), Meta{Labels: Labels{"Owner": {"source"}}}, strings.NewReader("content"))
	if err != nil {
		t.Fatal(err)
	}
	// A source repo's surviving hard link must be enough even if the shared
	// global path was reclaimed during a prior Add/Erase race.
	if err := os.Remove(source.pathToBlob(m.Digest)); err != nil {
		t.Fatal(err)
	}
	if _, err := target.Link(t.Context(), m.Digest, source); err != nil {
		t.Fatal(err)
	}
	a, err := os.Stat(source.pathToRepo(m.Digest, "blob"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.Stat(target.pathToRepo(m.Digest, "blob"))
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(a, b) {
		t.Fatal("Link copied content instead of sharing the source inode")
	}
	if err := source.Erase(t.Context(), m.Digest); err != nil {
		t.Fatal(err)
	}
	r, _, err := target.Open(t.Context(), m.Digest)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	data, err := io.ReadAll(r)
	if err != nil || string(data) != "content" {
		t.Fatalf("linked content = %q, %v", data, err)
	}
	entries, err := os.ReadDir(filepath.Join(stores.Root(), "stage"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("stage leftovers = %v, %v", entries, err)
	}
}

func TestOsLinkLabelsFailureLeavesNoDestination(t *testing.T) {
	stores := NewOsStores(t.TempDir())
	source := stores.Use("source").(OsStore)
	target := stores.Use("target").(OsStore)
	m, err := source.Add(t.Context(), Meta{}, strings.NewReader("content"))
	if err != nil {
		t.Fatal(err)
	}
	labels := source.pathToRepo(m.Digest, "labels")
	if err := os.Remove(labels); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(labels, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := target.Link(t.Context(), m.Digest, source); err == nil {
		t.Fatal("Link ignored invalid labels")
	}
	if _, err := target.Stat(t.Context(), m.Digest); !errors.Is(err, ErrNotExist) {
		t.Fatalf("destination Stat = %v", err)
	}
}

func TestOsLinkSourceEraseRace(t *testing.T) {
	stores := NewOsStores(t.TempDir())
	source := stores.Use("source").(OsStore)
	for range 30 {
		m, err := source.Add(t.Context(), Meta{}, strings.NewReader("content"))
		if err != nil {
			t.Fatal(err)
		}
		target := stores.Use("target").(OsStore)
		var linkErr, eraseErr error
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Go(func() { <-start; _, linkErr = target.Link(t.Context(), m.Digest, source) })
		wg.Go(func() { <-start; eraseErr = source.Erase(t.Context(), m.Digest) })
		close(start)
		wg.Wait()
		if eraseErr != nil {
			t.Fatal(eraseErr)
		}
		if linkErr != nil && !errors.Is(linkErr, ErrNotExist) {
			t.Fatalf("Link race error: %v", linkErr)
		}
		if linkErr == nil {
			r, _, err := target.Open(t.Context(), m.Digest)
			if err != nil {
				t.Fatalf("successful Link lost content: %v", err)
			}
			data, err := io.ReadAll(r)
			r.Close()
			if err != nil || string(data) != "content" {
				t.Fatalf("linked content = %q, %v", data, err)
			}
		}
		if err := target.Erase(t.Context(), m.Digest); err != nil {
			t.Fatal(err)
		}
	}
}
