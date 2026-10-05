package flob

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// osAdoptTestSource writes content to a file outside any namespace, on the
// store's filesystem, as another registry's blob would be.
func osAdoptTestSource(t *testing.T, root, name, content string) string {
	t.Helper()
	dir := filepath.Join(root, "foreign")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func osAdoptTestSame(t *testing.T, a, b string) bool {
	t.Helper()
	ai, err := os.Stat(a)
	if err != nil {
		t.Fatal(err)
	}
	bi, err := os.Stat(b)
	if err != nil {
		t.Fatal(err)
	}
	return os.SameFile(ai, bi)
}

// osAdoptTestNothingPublished checks that a refused adopt left no entry, no
// shared blob, and no stage behind.
func osAdoptTestNothingPublished(t *testing.T, store OsStore, d Digest) {
	t.Helper()
	if _, err := store.Stat(t.Context(), d); !errors.Is(err, ErrNotExist) {
		t.Errorf("Stat = %v; want ErrNotExist", err)
	}
	if _, err := os.Lstat(store.pathToBlob(d)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("shared blob exists: %v", err)
	}
	stage, err := store.ensureStagePath()
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(stage)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("stage left behind: %v", entries)
	}
}

func TestOsAdopt(t *testing.T) {
	const content = "content"
	d := DigestFromBytes([]byte(content))

	t.Run("a digest the store does not hold", func(t *testing.T) {
		root := t.TempDir()
		store := NewOsStores(root).Use("t").(OsStore)
		source := osAdoptTestSource(t, root, "blob", content)
		added := time.Date(2024, 3, 1, 12, 0, 0, 0, time.UTC)
		labels := Labels{"Media-Type": {"application/octet-stream"}}

		m, err := store.Adopt(t.Context(), Meta{Digest: d, Labels: labels}, source, AdoptOptions{Added: added})
		if err != nil {
			t.Fatal(err)
		}
		if m.Digest != d || m.Size != int64(len(content)) || m.Labels.Get("Media-Type") != "application/octet-stream" {
			t.Fatalf("Adopt = %#v", m)
		}

		// The source's inode is the store's copy and the entry's blob.
		if !osAdoptTestSame(t, source, store.pathToBlob(d)) {
			t.Error("shared blob is not the source's inode")
		}
		if !osAdoptTestSame(t, source, store.pathToRepo(d, "blob")) {
			t.Error("entry blob is not the source's inode")
		}
		if _, err := os.Stat(store.pathToRepo(d, "labels")); err != nil {
			t.Errorf("labels: %v", err)
		}

		info, err := store.Stat(t.Context(), d)
		if err != nil {
			t.Fatal(err)
		}
		if got, err := info.Added(t.Context()); err != nil || !got.Equal(added) {
			t.Errorf("Added = %v, %v; want %v", got, err, added)
		}
		got, err := statMeta(t.Context(), store, d)
		if err != nil || got.Labels.Get("Media-Type") != "application/octet-stream" || got.Size != int64(len(content)) {
			t.Errorf("Stat = %#v, %v", got, err)
		}
	})

	t.Run("zero Added is now", func(t *testing.T) {
		root := t.TempDir()
		store := NewOsStores(root).Use("t").(OsStore)
		source := osAdoptTestSource(t, root, "blob", content)
		before := time.Now().Add(-time.Second)
		if _, err := store.Adopt(t.Context(), Meta{Digest: d}, source, AdoptOptions{}); err != nil {
			t.Fatal(err)
		}
		info, err := store.Stat(t.Context(), d)
		if err != nil {
			t.Fatal(err)
		}
		if got, err := info.Added(t.Context()); err != nil || got.Before(before) {
			t.Errorf("Added = %v, %v; want about now", got, err)
		}
	})

	t.Run("a digest the store already holds", func(t *testing.T) {
		root := t.TempDir()
		stores := NewOsStores(root)
		if _, err := stores.Use("a").Add(t.Context(), Meta{}, strings.NewReader(content)); err != nil {
			t.Fatal(err)
		}
		store := stores.Use("b").(OsStore)
		source := osAdoptTestSource(t, root, "duplicate", content)

		if _, err := store.Adopt(t.Context(), Meta{Digest: d}, source, AdoptOptions{}); err != nil {
			t.Fatal(err)
		}
		if !osAdoptTestSame(t, store.pathToBlob(d), store.pathToRepo(d, "blob")) {
			t.Error("entry blob is not the shared inode")
		}
		if osAdoptTestSame(t, source, store.pathToRepo(d, "blob")) {
			t.Error("the source was linked although the store holds the digest")
		}
		if n, err := nlink(source); err == nil && n != 1 {
			t.Errorf("source has %d links; want 1", n)
		}
	})

	t.Run("adopting twice", func(t *testing.T) {
		root := t.TempDir()
		store := NewOsStores(root).Use("t").(OsStore)
		source := osAdoptTestSource(t, root, "blob", content)
		if _, err := store.Adopt(t.Context(), Meta{Digest: d, Labels: Labels{"Owner": {"first"}}}, source, AdoptOptions{}); err != nil {
			t.Fatal(err)
		}
		other := osAdoptTestSource(t, root, "other", content)
		_, err := store.Adopt(t.Context(), Meta{Digest: d, Labels: Labels{"Owner": {"second"}}}, other, AdoptOptions{})
		if !errors.Is(err, ErrAlreadyExists) {
			t.Fatalf("second Adopt = %v; want ErrAlreadyExists", err)
		}
		if !osAdoptTestSame(t, source, store.pathToRepo(d, "blob")) {
			t.Error("entry blob changed")
		}
		got, err := statMeta(t.Context(), store, d)
		if err != nil || got.Labels.Get("Owner") != "first" {
			t.Errorf("labels changed: %#v, %v", got, err)
		}
	})

	t.Run("verify", func(t *testing.T) {
		root := t.TempDir()
		store := NewOsStores(root).Use("t").(OsStore)
		source := osAdoptTestSource(t, root, "blob", content)
		if _, err := store.Adopt(t.Context(), Meta{Digest: d}, source, AdoptOptions{Verify: true}); err != nil {
			t.Fatal(err)
		}
		if !osAdoptTestSame(t, source, store.pathToRepo(d, "blob")) {
			t.Error("entry blob is not the source's inode")
		}
	})

	t.Run("verify refuses the wrong digest", func(t *testing.T) {
		root := t.TempDir()
		store := NewOsStores(root).Use("t").(OsStore)
		source := osAdoptTestSource(t, root, "blob", "other content")
		_, err := store.Adopt(t.Context(), Meta{Digest: d}, source, AdoptOptions{Verify: true})
		if !errors.Is(err, ErrDigestMismatch) {
			t.Fatalf("Adopt = %v; want ErrDigestMismatch", err)
		}
		osAdoptTestNothingPublished(t, store, d)
		if n, err := nlink(source); err == nil && n != 1 {
			t.Errorf("source has %d links; want 1", n)
		}
	})

	t.Run("verify hashes the source although the store holds the digest", func(t *testing.T) {
		root := t.TempDir()
		stores := NewOsStores(root)
		if _, err := stores.Use("a").Add(t.Context(), Meta{}, strings.NewReader(content)); err != nil {
			t.Fatal(err)
		}
		store := stores.Use("b").(OsStore)
		source := osAdoptTestSource(t, root, "blob", "other content")
		_, err := store.Adopt(t.Context(), Meta{Digest: d}, source, AdoptOptions{Verify: true})
		if !errors.Is(err, ErrDigestMismatch) {
			t.Fatalf("Adopt = %v; want ErrDigestMismatch", err)
		}
		if _, err := store.Stat(t.Context(), d); !errors.Is(err, ErrNotExist) {
			t.Errorf("Stat = %v; want ErrNotExist", err)
		}
	})

	t.Run("not a regular file", func(t *testing.T) {
		for _, mode := range []string{"symlink", "directory", "missing"} {
			t.Run(mode, func(t *testing.T) {
				root := t.TempDir()
				store := NewOsStores(root).Use("t").(OsStore)
				target := osAdoptTestSource(t, root, "blob", content)
				path := filepath.Join(root, "foreign", mode)
				switch mode {
				case "symlink":
					if err := os.Symlink(target, path); err != nil {
						t.Fatal(err)
					}
				case "directory":
					if err := os.Mkdir(path, 0o755); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := store.Adopt(t.Context(), Meta{Digest: d}, path, AdoptOptions{}); err == nil {
					t.Fatal("Adopt succeeded")
				}
				osAdoptTestNothingPublished(t, store, d)
			})
		}
	})

	t.Run("invalid digest", func(t *testing.T) {
		root := t.TempDir()
		store := NewOsStores(root).Use("t").(OsStore)
		source := osAdoptTestSource(t, root, "blob", content)
		if _, err := store.Adopt(t.Context(), Meta{}, source, AdoptOptions{}); err == nil {
			t.Fatal("Adopt without a digest succeeded")
		}
		if _, err := store.Adopt(t.Context(), Meta{Digest: "sha256:zz"}, source, AdoptOptions{}); err == nil {
			t.Fatal("Adopt with an invalid digest succeeded")
		}
	})

	t.Run("canceled", func(t *testing.T) {
		root := t.TempDir()
		store := NewOsStores(root).Use("t").(OsStore)
		source := osAdoptTestSource(t, root, "blob", content)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if _, err := store.Adopt(ctx, Meta{Digest: d}, source, AdoptOptions{Verify: true}); !errors.Is(err, context.Canceled) {
			t.Fatalf("Adopt = %v; want context.Canceled", err)
		}
		osAdoptTestNothingPublished(t, store, d)
	})

	t.Run("an adopted entry is like any other", func(t *testing.T) {
		root := t.TempDir()
		stores := NewOsStores(root)
		store := stores.Use("t").(OsStore)
		source := osAdoptTestSource(t, root, "blob", content)
		if _, err := store.Adopt(t.Context(), Meta{Digest: d, Labels: Labels{"Owner": {"adopted"}}}, source, AdoptOptions{}); err != nil {
			t.Fatal(err)
		}

		r, _, err := store.Open(t.Context(), d)
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(r)
		r.Close()
		if err != nil || string(data) != content {
			t.Fatalf("Open = %q, %v", data, err)
		}

		linked, err := stores.Use("linked").(Linker).Link(t.Context(), d, store)
		if err != nil || linked.Labels.Get("Owner") != "adopted" {
			t.Fatalf("Link = %#v, %v", linked, err)
		}

		// A chunked re-push of the same digest returns the entry.
		stage := osStageTestBegin(t, stores, "t", Canonical)
		if _, err := stage.Append(t.Context(), 0, strings.NewReader(content)); err != nil {
			t.Fatal(err)
		}
		m, err := stage.Commit(t.Context(), Meta{})
		if err != nil || m.Digest != d || m.Labels.Get("Owner") != "adopted" {
			t.Fatalf("Commit = %#v, %v", m, err)
		}

		// Erasing the entry leaves the source's file.
		if err := store.Erase(t.Context(), d); err != nil {
			t.Fatal(err)
		}
		data, err = os.ReadFile(source)
		if err != nil || string(data) != content {
			t.Fatalf("source after Erase = %q, %v", data, err)
		}
	})
}
