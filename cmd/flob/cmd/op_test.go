package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/lesomnus/flob"
)

func write(t *testing.T, dir, name, body string) ReaderResolver {
	t.Helper()

	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return ReaderResolver(p)
}

func TestAddAll(t *testing.T) {
	ctx := context.Background()

	t.Run("one invocation adds every file", func(t *testing.T) {
		dir := t.TempDir()
		files := []ReaderResolver{
			write(t, dir, "a", "alpha"),
			write(t, dir, "b", "bravo"),
			write(t, dir, "c", "charlie"),
		}

		store := flob.NewMemStores().Use("_")
		rs := addAll(ctx, store, strings.NewReader(""), files, 2)

		if len(rs) != len(files) {
			t.Fatalf("got %d results, want %d", len(rs), len(files))
		}
		for i, r := range rs {
			if r.err != nil {
				t.Fatalf("file %d: %v", i, r.err)
			}
			if r.digest == "" {
				t.Fatalf("file %d: no digest", i)
			}
			if _, err := store.Stat(ctx, r.digest); err != nil {
				t.Fatalf("file %d: not in the store: %v", i, err)
			}
		}
	})

	t.Run("results come back in the order the files were given", func(t *testing.T) {
		dir := t.TempDir()
		var files []ReaderResolver
		var want []flob.Digest
		for i := range 8 {
			body := fmt.Sprintf("blob-%d", i)
			files = append(files, write(t, dir, fmt.Sprint(i), body))
			want = append(want, flob.DigestFromBytes([]byte(body)))
		}

		// A caller pairing digests with names reads them positionally, so
		// finishing out of order must not reorder the output.
		rs := addAll(ctx, flob.NewMemStores().Use("_"), strings.NewReader(""), files, 4)
		for i, r := range rs {
			if r.digest != want[i] {
				t.Fatalf("result %d: got %s want %s", i, r.digest, want[i])
			}
		}
	})

	t.Run("a file that is already there is reported, not hidden", func(t *testing.T) {
		dir := t.TempDir()
		f := write(t, dir, "a", "alpha")
		store := flob.NewMemStores().Use("_")

		rs := addAll(ctx, store, strings.NewReader(""), []ReaderResolver{f}, 1)
		if rs[0].err != nil {
			t.Fatal(rs[0].err)
		}

		rs = addAll(ctx, store, strings.NewReader(""), []ReaderResolver{f}, 1)
		if !errors.Is(rs[0].err, flob.ErrAlreadyExists) {
			t.Fatalf("want already-exists, got %v", rs[0].err)
		}
		if rs[0].digest == "" {
			t.Fatal("the digest is still the answer even when nothing was written")
		}
	})

	t.Run("one bad file does not lose the others", func(t *testing.T) {
		dir := t.TempDir()
		files := []ReaderResolver{
			write(t, dir, "a", "alpha"),
			ReaderResolver(filepath.Join(dir, "does-not-exist")),
			write(t, dir, "c", "charlie"),
		}

		// A publish run wants every failure in one pass, not one per re-run.
		rs := addAll(ctx, flob.NewMemStores().Use("_"), strings.NewReader(""), files, 3)
		if rs[0].err != nil || rs[2].err != nil {
			t.Fatalf("the good files failed: %v %v", rs[0].err, rs[2].err)
		}
		if rs[1].err == nil {
			t.Fatal("the missing file was not reported")
		}
		if !strings.Contains(rs[1].err.Error(), "does-not-exist") {
			t.Fatalf("the error does not say which file: %v", rs[1].err)
		}
	})

	t.Run("no more than parallel are in flight", func(t *testing.T) {
		dir := t.TempDir()
		var files []ReaderResolver
		for i := range 12 {
			files = append(files, write(t, dir, fmt.Sprint(i), fmt.Sprintf("blob-%d", i)))
		}

		store := newCountingStore(flob.NewMemStores().Use("_"), 3)
		addAll(ctx, store, strings.NewReader(""), files, 3)

		if store.peak != 3 {
			t.Fatalf("peak concurrency %d, want 3", store.peak)
		}
	})
}

// countingStore records how many Adds overlap, and holds each one until enough
// have arrived to prove they do. A sequential implementation never reaches the
// limit and the test times out, which is the point.
type countingStore struct {
	flob.Store

	limit int
	mu    sync.Mutex
	cond  *sync.Cond

	inFlight int
	peak     int
	released bool
}

func newCountingStore(s flob.Store, limit int) *countingStore {
	c := &countingStore{Store: s, limit: limit}
	c.cond = sync.NewCond(&c.mu)
	return c
}

func (s *countingStore) Add(ctx context.Context, m flob.Meta, r io.Reader) (flob.Meta, error) {
	s.mu.Lock()
	s.inFlight++
	if s.inFlight > s.peak {
		s.peak = s.inFlight
	}
	// Release once, and stay released: the pool refills as workers finish, so
	// a later arrival may well be alone at the barrier.
	if s.inFlight >= s.limit {
		s.released = true
	}
	s.cond.Broadcast()
	for !s.released {
		s.cond.Wait()
	}
	s.mu.Unlock()

	m, err := s.Store.Add(ctx, m, r)

	s.mu.Lock()
	s.inFlight--
	s.mu.Unlock()
	return m, err
}

func TestAddOutcome(t *testing.T) {
	fail := func(name string) addResult {
		return addResult{err: fmt.Errorf("%s: %w", name, errors.New("connection reset"))}
	}
	ok := addResult{digest: "sha256:aa"}
	dup := addResult{digest: "sha256:bb", err: fmt.Errorf("b: %w", flob.ErrAlreadyExists)}

	t.Run("all added is success", func(t *testing.T) {
		if err := addOutcome([]addResult{ok, ok}); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("all already there is reported as such", func(t *testing.T) {
		err := addOutcome([]addResult{dup, dup})
		if !errors.Is(err, flob.ErrAlreadyExists) {
			t.Fatalf("got %v, want ErrAlreadyExists", err)
		}
	})

	t.Run("one added among duplicates is success", func(t *testing.T) {
		// Publishing a model that shares blobs with a published one lands
		// here, and it is not a re-run: something was written.
		if err := addOutcome([]addResult{dup, ok, dup}); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("every failure is reported, not just the first", func(t *testing.T) {
		err := addOutcome([]addResult{fail("a"), ok, fail("c"), dup})
		if err == nil {
			t.Fatal("expected an error")
		}
		for _, name := range []string{"a", "c"} {
			if !strings.Contains(err.Error(), name+":") {
				t.Fatalf("%q is missing from: %v", name, err)
			}
		}
	})

	t.Run("a failure outweighs the duplicates", func(t *testing.T) {
		// Otherwise a run where everything else was already there would exit
		// 3, and a caller that treats 3 as "nothing to do" would call a failed
		// publish a successful one.
		err := addOutcome([]addResult{dup, fail("b"), dup})
		if err == nil {
			t.Fatal("expected an error")
		}
		if errors.Is(err, flob.ErrAlreadyExists) {
			t.Fatalf("reported as already-exists: %v", err)
		}
	})
}
