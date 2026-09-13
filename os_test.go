package flob

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/lesomnus/flob/internal/x"
	"github.com/opencontainers/go-digest"
)

func TestOsStore(t *testing.T) {
	new_stores := func(t *testing.T) Stores {
		t.Helper()
		root := t.TempDir()
		return NewOsStores(root)
	}
	new_store := func(t *testing.T) Store {
		t.Helper()
		stores := new_stores(t)
		return stores.Use("test")
	}

	path_to_blob := func(s Store, d Digest) string {
		return s.(OsStore).pathToBlob(d)
	}

	t.Run("contract", func(t *testing.T) {
		testStore(t, new_stores)
	})

	t.Run("single-ref erase removes global blob", func(t *testing.T) {
		ctx, x := x.New(t)
		s := new_store(t)

		m, err := s.Add(ctx, Meta{}, x.Reader())
		x.NoError(err)

		pb := path_to_blob(s, m.Digest)
		n, err := nlink(pb)
		x.NoError(err)
		x.Eq(n, 2)

		err = s.Erase(ctx, m.Digest)
		x.NoError(err)

		// Global blob must be gone after single-ref erase.
		_, err = nlink(pb)
		x.ErrorIs(err, os.ErrNotExist)
	})
	t.Run("cross-repo same blob shares hard link", func(t *testing.T) {
		ctx, x := x.New(t)

		root := t.TempDir()
		stores := NewOsStores(root)
		s1 := stores.Use("store1")
		s2 := stores.Use("store2")

		m1, err := s1.Add(ctx, Meta{}, x.Reader())
		x.NoError(err)

		_, err = s2.Add(ctx, Meta{}, x.Reader())
		x.NoError(err)

		// Both stores reference the same digest; global blob must have nlink == 3
		// (1 for the global namespace + 1 per repo).
		pb := path_to_blob(s1, m1.Digest)
		n, err := nlink(pb)
		x.NoError(err)
		x.Eq(n, 3)
	})
	t.Run("multi-ref erase keeps global blob", func(t *testing.T) {
		ctx, x := x.New(t)

		root := t.TempDir()
		stores := NewOsStores(root)
		s1 := stores.Use("store1")
		s2 := stores.Use("store2")

		m, err := s1.Add(ctx, Meta{}, x.Reader())
		x.NoError(err)
		_, err = s2.Add(ctx, Meta{}, x.Reader())
		x.NoError(err)

		// Erase from s1 only.
		x.NoError(s1.Erase(ctx, m.Digest))

		// Global blob must still exist; only s2's repo link remains (nlink == 2).
		pb := path_to_blob(s1, m.Digest)
		n, err := nlink(pb)
		x.NoError(err)
		x.Eq(n, 2)

		// s1 must no longer see the blob.
		_, err = statMeta(ctx, s1, m.Digest)
		x.ErrorIs(err, ErrNotExist)

		// s2 must still see the blob.
		_, err = statMeta(ctx, s2, m.Digest)
		x.NoError(err)
	})
	t.Run("all refs erased removes global blob", func(t *testing.T) {
		ctx, x := x.New(t)

		root := t.TempDir()
		stores := NewOsStores(root)
		s1 := stores.Use("store1")
		s2 := stores.Use("store2")

		m, err := s1.Add(ctx, Meta{}, x.Reader())
		x.NoError(err)
		_, err = s2.Add(ctx, Meta{}, x.Reader())
		x.NoError(err)

		err = s1.Erase(ctx, m.Digest)
		x.NoError(err)
		err = s2.Erase(ctx, m.Digest)
		x.NoError(err)

		// Global blob must be gone after all refs erased.
		pb := path_to_blob(s1, m.Digest)
		_, err = nlink(pb)
		x.ErrorIs(err, os.ErrNotExist)
	})

	t.Run("malformed digest does not panic", func(t *testing.T) {
		ctx, x := x.New(t)
		s := new_store(t)

		// A digest without the "algo:" separator would panic in go-digest when building a
		// path, so the store must reject it defensively instead.
		bad := Digest("deadbeef")

		_, err := statMeta(ctx, s, bad)
		x.ErrorIs(err, ErrNotExist)

		_, _, err = s.Open(ctx, bad)
		x.ErrorIs(err, ErrNotExist)

		err = s.Label(ctx, bad, Labels{"A": {"b"}})
		x.ErrorIs(err, ErrNotExist)

		// Erase never reports "not exist", so an invalid digest is a no-op success.
		err = s.Erase(ctx, bad)
		x.NoError(err)
	})

	t.Run("add recovers from a leftover repo directory", func(t *testing.T) {
		ctx, x := x.New(t)
		root := t.TempDir()
		s := NewOsStores(root).Use("test").(OsStore)

		d := DigestFromBytes(x.Data())

		// Simulate a crash mid-Erase (RemoveAll unlinks blob, then labels, then the dir):
		// a repo digest directory that holds a labels file but no blob. checkDup only looks
		// at the blob file, so Add reaches the final rename onto this pre-existing directory.
		pr := s.pathToRepo(d)
		x.NoError(os.MkdirAll(pr, 0o755))
		x.NoError(os.WriteFile(filepath.Join(pr, "labels"), []byte("X-Foo: bar\r\n\r\n"), 0o644))

		// The user's Add must still succeed.
		m, err := s.Add(ctx, Meta{}, x.Reader())
		x.NoError(err)
		x.Eq(d, m.Digest)

		r, _, err := s.Open(ctx, d)
		x.NoError(err)
		defer r.Close()
		got, err := io.ReadAll(r)
		x.NoError(err)
		x.Eq(x.Data(), got)
	})

	t.Run("label existence check matches stat and open", func(t *testing.T) {
		ctx, x := x.New(t)
		root := t.TempDir()
		s := NewOsStores(root).Use("test").(OsStore)

		d := DigestFromBytes(x.Data())

		// An orphan repo directory that has a labels file but no blob (e.g. a crash mid-Erase).
		// Stat/Open define existence by the blob file, so they report ErrNotExist; Label must
		// use the same criterion instead of merely checking that the directory exists.
		pr := s.pathToRepo(d)
		x.NoError(os.MkdirAll(pr, 0o755))
		x.NoError(os.WriteFile(filepath.Join(pr, "labels"), []byte("X-Foo: bar\r\n\r\n"), 0o644))

		_, err := statMeta(ctx, s, d)
		x.ErrorIs(err, ErrNotExist)

		_, _, err = s.Open(ctx, d)
		x.ErrorIs(err, ErrNotExist)

		// Label must agree: no blob => ErrNotExist (previously it succeeded on the stray dir).
		err = s.Label(ctx, d, Labels{"A": {"b"}})
		x.ErrorIs(err, ErrNotExist)

		// Once the blob genuinely exists, Label succeeds.
		_, err = s.Add(ctx, Meta{}, x.Reader())
		x.NoError(err)
		x.NoError(s.Label(ctx, d, Labels{"A": {"b"}}))
	})

	t.Run("concurrent add of same content across stores dedupes", func(t *testing.T) {
		ctx, x := x.New(t)
		root := t.TempDir()
		stores := NewOsStores(root)

		const n = 16
		var wg sync.WaitGroup
		errs := make([]error, n)
		digs := make([]Digest, n)
		for i := range n {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				m, err := stores.Use(fmt.Sprintf("store-%d", i)).Add(ctx, Meta{}, x.Reader())
				errs[i] = err
				digs[i] = m.Digest
			}(i)
		}
		wg.Wait()

		for i := range n {
			x.NoError(errs[i])
			x.Eq(digs[0], digs[i])
		}

		// One global inode shared by every repo: nlink == n repo links + 1 global link.
		pb := stores.Use("store-0").(OsStore).pathToBlob(digs[0])
		got, err := nlink(pb)
		x.NoError(err)
		x.Eq(n+1, got)

		// Every repo can read the content back.
		for i := range n {
			r, _, err := stores.Use(fmt.Sprintf("store-%d", i)).Open(ctx, digs[0])
			x.NoError(err)
			data, err := io.ReadAll(r)
			r.Close()
			x.NoError(err)
			x.Eq(x.Data(), data)
		}
	})

	t.Run("concurrent add and erase on the same repo always succeeds", func(t *testing.T) {
		ctx, x := x.New(t)
		root := t.TempDir()
		s := NewOsStores(root).Use("test")

		d := DigestFromBytes(x.Data())

		const workers = 8
		const iters = 250
		var wg sync.WaitGroup
		addErrs := make(chan error, workers*iters)
		for range workers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for range iters {
					// Add must always succeed (or report the benign ErrAlreadyExists),
					// never fail with a leftover-directory error, even while a concurrent
					// Erase is removing the same repo entry.
					if _, err := s.Add(ctx, Meta{}, x.Reader()); err != nil && !errors.Is(err, ErrAlreadyExists) {
						addErrs <- err
					}
					_ = s.Erase(ctx, d)
				}
			}()
		}
		wg.Wait()
		close(addErrs)

		for err := range addErrs {
			t.Fatalf("Add failed under concurrency (must always succeed): %v", err)
		}

		// The store remains usable afterwards.
		if _, err := s.Add(ctx, Meta{}, x.Reader()); err != nil && !errors.Is(err, ErrAlreadyExists) {
			t.Fatalf("final Add failed: %v", err)
		}
	})
}

// TestOsStoreAddWithoutASystemTempDir is the container case: `FROM scratch` has
// no /tmp, and an Add that staged there failed before it read a byte.
func TestOsStoreAddWithoutASystemTempDir(t *testing.T) {
	// TMPDIR at a path that does not exist is what os.TempDir() answers with in
	// an image that never had one.
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "not-here"))

	root := t.TempDir()
	s := NewOsStores(root).Use("_")

	data := []byte("bytes a robot asked for")
	m, err := s.Add(t.Context(), Meta{}, strings.NewReader(string(data)))
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if m.Digest != DigestFromBytes(data) {
		t.Fatalf("digest: got %s", m.Digest)
	}
	if m.Size != int64(len(data)) {
		t.Fatalf("size: got %d want %d", m.Size, len(data))
	}

	if _, err := statMeta(t.Context(), s, m.Digest); err != nil {
		t.Fatalf("stat: %v", err)
	}
}

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

func TestOsWalkSkipsMalformedAndSymlinkEntries(t *testing.T) {
	stores := NewOsStores(t.TempDir())
	s := stores.Use("valid").(OsStore)
	m, err := s.Add(t.Context(), Meta{}, strings.NewReader("content"))
	if err != nil {
		t.Fatal(err)
	}
	bad := []string{
		filepath.Join(s.repo, "sha256", "00", "00", "bad", "blob"),
		filepath.Join(s.repo, "unknown", "00", "00", strings.Repeat("0", 60), "blob"),
		filepath.Join(s.repo, "sha256", "000", "0", strings.Repeat("0", 60), "blob"),
	}
	for _, path := range bad {
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("invalid"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	orphan := stores.Use("orphan").(OsStore).pathToRepo(m.Digest, "labels")
	if err := os.MkdirAll(filepath.Dir(orphan), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(orphan, nil, 0600); err != nil {
		t.Fatal(err)
	}
	// Neither an aliased namespace nor a symlink pretending to be a blob is
	// inventory. Walk must not escape through filesystem links.
	if err := os.Symlink(s.repo, filepath.Join(stores.Root(), "repos", "alias")); err != nil {
		t.Fatal(err)
	}
	link := stores.Use("symlink").(OsStore).pathToRepo(m.Digest, "blob")
	if err := os.MkdirAll(filepath.Dir(link), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(s.pathToRepo(m.Digest, "blob"), link); err != nil {
		t.Fatal(err)
	}
	count := 0
	for info, err := range s.Walk(t.Context()) {
		if err != nil {
			t.Fatal(err)
		}
		if info.Digest() != m.Digest {
			t.Fatalf("unexpected digest %s", info.Digest())
		}
		count++
	}
	if count != 1 {
		t.Fatalf("Walk count = %d", count)
	}
	var ids []string
	for id, err := range stores.Namespaces(t.Context()) {
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if len(ids) != 1 || ids[0] != "valid" {
		t.Fatalf("namespaces = %q", ids)
	}
}

func TestOsWalkDoesNotReadLabels(t *testing.T) {
	s := NewOsStores(t.TempDir()).Use("namespace").(OsStore)
	m, err := s.Add(t.Context(), Meta{}, strings.NewReader("content"))
	if err != nil {
		t.Fatal(err)
	}
	path := s.pathToRepo(m.Digest, "labels")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	var result Info
	for info, err := range s.Walk(t.Context()) {
		if err != nil {
			t.Fatal(err)
		}
		result = info
	}
	if result == nil || result.Size() != 7 {
		t.Fatalf("Info = %v", result)
	}
	if _, err := result.Labels(t.Context()); err == nil {
		t.Fatal("Labels ignored read failure")
	}
	if err := s.Erase(t.Context(), m.Digest); err != nil {
		t.Fatal(err)
	}
	if _, err := result.Labels(t.Context()); !errors.Is(err, ErrNotExist) {
		t.Fatalf("deleted labels error = %v", err)
	}
}

func osStageTestBegin(t *testing.T, stores OsStores, namespace string, algo digest.Algorithm) *osStage {
	t.Helper()
	stage, err := stores.Use(namespace).(Stager).Begin(t.Context(), algo)
	if err != nil {
		t.Fatal(err)
	}
	return stage.(*osStage)
}
func osStageTestRecord(t *testing.T, stage *osStage, change func(*stageRecord)) {
	t.Helper()
	locked, err := stage.lock(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer locked.close()
	change(&locked.record)
	if _, err := osStageWriteRecord(t.Context(), locked.dir, locked.record); err != nil {
		t.Fatal(err)
	}
}

func TestOsStageRestartAndCrashTail(t *testing.T) {
	for _, algo := range []digest.Algorithm{digest.SHA256, digest.SHA384, digest.SHA512} {
		t.Run(string(algo), func(t *testing.T) {
			root := t.TempDir()
			stores := NewOsStores(root)
			stage := osStageTestBegin(t, stores, "../namespace", algo)
			if offset, err := stage.Append(t.Context(), 0, strings.NewReader("first")); err != nil || offset != 5 {
				t.Fatalf("Append = %d, %v", offset, err)
			}
			path := filepath.Join(root, "uploads", stage.ID(), "blob")
			file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
			if err != nil {
				t.Fatal(err)
			}
			file.WriteString("uncommitted crash tail")
			file.Sync()
			file.Close()
			reopened := NewOsStores(root)
			resumed, err := reopened.Use("../namespace").(Stager).Resume(t.Context(), stage.ID())
			if err != nil {
				t.Fatal(err)
			}
			if stat, err := os.Stat(path); err != nil || stat.Size() != 5 {
				t.Fatalf("recovered file size = %v, %v", stat, err)
			}
			if offset, err := resumed.Append(t.Context(), 5, strings.NewReader("second")); err != nil || offset != 11 {
				t.Fatalf("resumed Append = %d, %v", offset, err)
			}
			before, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			m, err := resumed.Commit(t.Context(), Meta{Labels: Labels{"Owner": {"source"}}})
			if err != nil {
				t.Fatal(err)
			}
			if m.Digest != Digest(algo.FromString("firstsecond")) {
				t.Fatalf("digest = %s", m.Digest)
			}
			destination := reopened.Use("../namespace").(OsStore).pathToRepo(m.Digest, "blob")
			after, err := os.Stat(destination)
			if err != nil {
				t.Fatal(err)
			}
			if !os.SameFile(before, after) {
				t.Fatal("commit copied the staged blob instead of publishing its inode")
			}
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("terminal stage retained data link: %v", err)
			}
			receipt, err := NewOsStores(root).Use("../namespace").(Stager).Resume(t.Context(), stage.ID())
			if err != nil {
				t.Fatal(err)
			}
			result, err := receipt.Commit(t.Context(), Meta{Labels: Labels{"Owner": {"source"}}})
			if err != nil || result.Digest != m.Digest {
				t.Fatalf("restart receipt = %#v, %v", result, err)
			}
		})
	}
}

func TestOsStageInterruptedCommitRecovery(t *testing.T) {
	for _, operation := range []string{"commit", "abort", "prune"} {
		t.Run(operation, func(t *testing.T) {
			root := t.TempDir()
			stores := NewOsStores(root)
			stage := osStageTestBegin(t, stores, "a/b\xff", Canonical)
			if _, err := stage.Append(t.Context(), 0, strings.NewReader("content")); err != nil {
				t.Fatal(err)
			}
			// A non-directory at the share root forces publication to fail after the
			// durable committing manifest, without depending on Unix permission checks.
			if err := os.WriteFile(filepath.Join(root, "share"), []byte("block"), 0o600); err != nil {
				t.Fatal(err)
			}
			labels := Labels{"Owner": {"frozen"}}
			if _, err := stage.Commit(t.Context(), Meta{Labels: labels}); !errors.Is(err, ErrStageFormat) {
				t.Fatalf("pending Commit = %v", err)
			}
			info, err := stage.Stat(t.Context())
			if err != nil || info.State != StageCommitting {
				t.Fatalf("pending state = %#v, %v", info, err)
			}
			if _, err := stage.Commit(t.Context(), Meta{Labels: Labels{"Owner": {"different"}}}); !errors.Is(err, ErrStageConflict) {
				t.Fatalf("different pending commit = %v", err)
			}
			if err := os.Remove(filepath.Join(root, "share")); err != nil {
				t.Fatal(err)
			}
			stage.store = NewOsStores(root).Use("a/b\xff").(OsStore)
			switch operation {
			case "commit":
				if _, err := stage.Commit(t.Context(), Meta{Labels: labels}); err != nil {
					t.Fatal(err)
				}
			case "abort":
				if err := stage.Abort(t.Context()); err != nil {
					t.Fatal(err)
				}
			case "prune":
				if n, err := stores.PruneStages(t.Context()); err != nil || n != 0 {
					t.Fatalf("recover Prune = %d, %v", n, err)
				}
			}
			m, err := statMeta(t.Context(), stage.store, DigestFromBytes([]byte("content")))
			if err != nil || m.Labels.Get("Owner") != "frozen" {
				t.Fatalf("recovered blob = %#v, %v", m, err)
			}
			info, err = stage.Stat(t.Context())
			if err != nil || info.State != StageCommitted {
				t.Fatalf("recovered state = %#v, %v", info, err)
			}
		})
	}
}

func TestOsStageRecoversPublishedCommit(t *testing.T) {
	root := t.TempDir()
	stores := NewOsStores(root)
	stage := osStageTestBegin(t, stores, "t", Canonical)
	if _, err := stage.Append(t.Context(), 0, strings.NewReader("content")); err != nil {
		t.Fatal(err)
	}
	labels := Labels{"Owner": {"frozen"}}
	osStageTestRecord(t, stage, func(record *stageRecord) {
		m, err := record.prepareCommit(Meta{Labels: labels})
		if err != nil {
			t.Fatal(err)
		}
		record.Commit = m
		record.State = StageCommitting
	})
	// Simulate publication completing before its receipt was made durable.
	d := DigestFromBytes([]byte("content"))
	target := stage.store.pathToRepo(d)
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(root, "uploads", stage.ID(), "blob"), filepath.Join(target, "blob")); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(filepath.Join(target, "labels"))
	if err != nil {
		t.Fatal(err)
	}
	writeLabels(f, labels)
	f.Close()
	m, err := stage.Commit(t.Context(), Meta{Labels: labels})
	if err != nil || m.Digest != d {
		t.Fatalf("recover published = %#v, %v", m, err)
	}
	if err := stage.Abort(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := stage.store.Stat(t.Context(), d); err != nil {
		t.Fatalf("Abort removed published blob: %v", err)
	}
}

type osStageBlockingReader struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (r *osStageBlockingReader) Read([]byte) (int, error) {
	r.once.Do(func() { close(r.entered) })
	<-r.release
	return 0, io.EOF
}

func TestOsStageLockedOperationAndPrune(t *testing.T) {
	var now atomic.Int64
	now.Store(time.Now().UnixNano())
	cfg := StageConfig{TTL: time.Minute, OperationTimeout: time.Hour, now: func() time.Time { return time.Unix(0, now.Load()) }}
	stores := NewOsStores(t.TempDir(), cfg)
	stage := osStageTestBegin(t, stores, "t", Canonical)
	reader := &osStageBlockingReader{entered: make(chan struct{}), release: make(chan struct{})}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := stage.Append(ctx, 0, reader); done <- err }()
	<-reader.entered
	now.Add(int64(2 * time.Hour))
	if n, err := stores.PruneStages(t.Context()); err != nil || n != 0 {
		t.Fatalf("Prune active operation = %d, %v", n, err)
	}
	wait, cancelWait := context.WithTimeout(t.Context(), 25*time.Millisecond)
	if _, err := stage.Stat(wait); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("locked Stat = %v", err)
	}
	cancelWait()
	cancel()
	close(reader.release)
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Append = %v", err)
	}
	if n, err := stores.PruneStages(t.Context()); err != nil || n != 1 {
		t.Fatalf("Prune released operation = %d, %v", n, err)
	}
	if err := stage.Abort(t.Context()); err != nil {
		t.Fatalf("Abort pruned stage = %v", err)
	}
}

func TestOsStageLockProcessHelper(t *testing.T) {
	root, id := os.Getenv("FLOB_STAGE_LOCK_TEST_ROOT"), os.Getenv("FLOB_STAGE_LOCK_TEST_ID")
	if root == "" {
		return
	}
	stage := &osStage{store: NewOsStores(root).Use("t").(OsStore), id: id}
	locked, err := stage.lock(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer locked.close()
	fmt.Println("stage locked")
	time.Sleep(time.Hour)
}
func TestOsStageProcessDeathReleasesLock(t *testing.T) {
	root := t.TempDir()
	stage := osStageTestBegin(t, NewOsStores(root), "t", Canonical)
	command := exec.Command(os.Args[0], "-test.run=^TestOsStageLockProcessHelper$")
	command.Env = append(os.Environ(), "FLOB_STAGE_LOCK_TEST_ROOT="+root, "FLOB_STAGE_LOCK_TEST_ID="+stage.ID())
	output, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { command.Process.Kill(); command.Wait() }()
	if line, err := bufio.NewReader(output).ReadString('\n'); err != nil || line != "stage locked\n" {
		t.Fatalf("helper = %q, %v", line, err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Millisecond)
	if _, err := stage.Stat(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cross-process lock = %v", err)
	}
	cancel()
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	command.Wait()
	if _, err := NewOsStores(root).Use("t").(Stager).Resume(t.Context(), stage.ID()); err != nil {
		t.Fatalf("resume after process death = %v", err)
	}
}

func TestOsStageMalformedFiles(t *testing.T) {
	for _, mode := range []string{"manifest symlink", "blob symlink", "lock symlink", "stage symlink", "offset mismatch", "short blob", "unknown field", "trailing json", "blob hardlink", "destination symlink"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			stores := NewOsStores(root)
			stage := osStageTestBegin(t, stores, "t", Canonical)
			outside := filepath.Join(t.TempDir(), "outside")
			if err := os.WriteFile(outside, []byte("untouched"), 0o600); err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(root, "uploads", stage.ID())
			switch mode {
			case "manifest symlink", "blob symlink", "lock symlink":
				name := strings.TrimSuffix(mode, " symlink")
				os.Remove(filepath.Join(dir, name))
				if err := os.Symlink(outside, filepath.Join(dir, name)); err != nil {
					t.Fatal(err)
				}
			case "stage symlink":
				os.RemoveAll(dir)
				if err := os.Symlink(filepath.Dir(outside), dir); err != nil {
					t.Fatal(err)
				}
			case "offset mismatch":
				osStageTestRecord(t, stage, func(record *stageRecord) { record.Offset++ })
			case "short blob":
				if _, err := stage.Append(t.Context(), 0, strings.NewReader("content")); err != nil {
					t.Fatal(err)
				}
				os.Truncate(filepath.Join(dir, "blob"), 1)
			case "unknown field", "trailing json":
				path := filepath.Join(dir, "manifest")
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if mode == "unknown field" {
					data = append([]byte(`{"Unknown":1,`), data[1:]...)
				} else {
					data = append(data, []byte(" {}")...)
				}
				os.WriteFile(path, data, 0o600)
			case "blob hardlink":
				os.Remove(filepath.Join(dir, "blob"))
				if err := os.Link(outside, filepath.Join(dir, "blob")); err != nil {
					t.Fatal(err)
				}
			case "destination symlink":
				if _, err := stage.Append(t.Context(), 0, strings.NewReader("content")); err != nil {
					t.Fatal(err)
				}
				dest := stage.store.pathToRepo(DigestFromBytes([]byte("content")))
				os.MkdirAll(filepath.Dir(dest), 0o755)
				if err := os.Symlink(filepath.Dir(outside), dest); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "destination symlink" {
				if _, err := stage.Commit(t.Context(), Meta{}); !errors.Is(err, ErrStageFormat) {
					t.Fatalf("unsafe Commit = %v", err)
				}
			} else {
				if _, err := stores.Use("t").(Stager).Resume(t.Context(), stage.ID()); !errors.Is(err, ErrStageFormat) {
					t.Fatalf("unsafe Resume = %v", err)
				}
			}
			data, err := os.ReadFile(outside)
			if err != nil || string(data) != "untouched" {
				t.Fatalf("outside changed = %q, %v", data, err)
			}
		})
	}
}

func TestOsStagePruneOrphansAndMalformedRecords(t *testing.T) {
	for _, mode := range []string{"missing lock", "existing lock", "active lock", "corrupt manifest", "young orphan"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			cfg := StageConfig{TTL: time.Minute, OperationTimeout: time.Minute}
			stores := NewOsStores(root, cfg)
			record, err := newStageRecord("t", Canonical, cfg)
			if err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(root, "uploads", record.ID)
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if mode != "missing lock" {
				if err := os.WriteFile(filepath.Join(dir, "lock"), nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "corrupt manifest" {
				os.WriteFile(filepath.Join(dir, "manifest"), []byte("broken JSON"), 0o600)
			}
			if mode != "young orphan" {
				old := time.Now().Add(-time.Hour)
				os.Chtimes(dir, old, old)
			}
			var lock *flock.Flock
			if mode == "active lock" {
				lock = flock.New(filepath.Join(dir, "lock"))
				if err := lock.Lock(); err != nil {
					t.Fatal(err)
				}
				defer lock.Close()
			}
			removed, err := stores.PruneStages(t.Context())
			if mode == "missing lock" || mode == "existing lock" {
				if err != nil || removed != 1 {
					t.Fatalf("Prune orphan = %d, %v", removed, err)
				}
				return
			}
			if mode == "corrupt manifest" {
				if !errors.Is(err, ErrStageFormat) {
					t.Fatalf("corrupt Prune = %d, %v", removed, err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if removed != 0 {
				t.Fatalf("removed protected stage = %d", removed)
			}
			if _, err := os.Stat(dir); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestOsStageUploadsSymlink(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "uploads")); err != nil {
		t.Fatal(err)
	}
	if _, err := NewOsStores(root).Use("t").(Stager).Begin(t.Context(), Canonical); !errors.Is(err, ErrStageFormat) {
		t.Fatalf("Begin through symlink = %v", err)
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 0 {
		t.Fatalf("outside entries = %v, %v", entries, err)
	}
}

// Ensure a syntactically valid manifest with a foreign namespace remains
// inaccessible even if the caller knows its opaque ID.
func TestOsStageManifestNamespaceIsolation(t *testing.T) {
	stores := NewOsStores(t.TempDir())
	stage := osStageTestBegin(t, stores, "source", Canonical)
	data, err := os.ReadFile(filepath.Join(stores.root, "uploads", stage.ID(), "manifest"))
	if err != nil {
		t.Fatal(err)
	}
	var record stageRecord
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	if record.Namespace != "source" {
		t.Fatalf("namespace = %q", record.Namespace)
	}
	if _, err := stores.Use("other").(Stager).Resume(t.Context(), stage.ID()); !errors.Is(err, ErrNotExist) {
		t.Fatalf("cross-namespace Resume = %v", err)
	}
}

func TestOsStagePruneAbandonedAttempts(t *testing.T) {
	stores := NewOsStores(t.TempDir())
	stage := osStageTestBegin(t, stores, "t", Canonical)
	if _, err := stage.Append(t.Context(), 0, strings.NewReader("content")); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(stores.root, "uploads", stage.ID())
	if err := os.WriteFile(filepath.Join(dir, "manifest.next"), []byte("unfinished"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "publish"), 0o700); err != nil {
		t.Fatal(err)
	}
	if n, err := stores.PruneStages(t.Context()); err != nil || n != 0 {
		t.Fatalf("Prune attempts = %d, %v", n, err)
	}
	for _, name := range []string{"manifest.next", "publish"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("abandoned %s = %v", name, err)
		}
	}
	m, err := stage.Commit(t.Context(), Meta{})
	if err != nil {
		t.Fatal(err)
	}
	// Simulate process death between committed receipt fsync and private-link cleanup.
	if err := os.Link(stage.store.pathToRepo(m.Digest, "blob"), filepath.Join(dir, "blob")); err != nil {
		t.Fatal(err)
	}
	if n, err := stores.PruneStages(t.Context()); err != nil || n != 0 {
		t.Fatalf("Prune terminal data = %d, %v", n, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "blob")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("terminal data = %v", err)
	}
	if _, err := stage.store.Stat(t.Context(), m.Digest); err != nil {
		t.Fatalf("published blob lost: %v", err)
	}
}

func TestOsStageOperationTimeout(t *testing.T) {
	stores := NewOsStores(t.TempDir(), StageConfig{OperationTimeout: 20 * time.Millisecond})
	stage := osStageTestBegin(t, stores, "t", Canonical)
	lock := flock.New(filepath.Join(stores.root, "uploads", stage.ID(), "lock"))
	if err := lock.Lock(); err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if _, err := stage.Stat(t.Context()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("operation timeout = %v", err)
	}
	if err := lock.Unlock(); err != nil {
		t.Fatal(err)
	}
	if _, err := stage.Stat(t.Context()); err != nil {
		t.Fatalf("lock not reusable after timeout: %v", err)
	}
}
