package flob

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
