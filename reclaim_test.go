package flob

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

func read(t *testing.T, s Store, d Digest) []byte {
	t.Helper()

	r, _, err := s.Open(context.Background(), d)
	if err != nil {
		t.Fatalf("open %s: %v", d, err)
	}
	defer r.Close()

	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read %s: %v", d, err)
	}
	return b
}

func TestReclaimRefusesNoGrace(t *testing.T) {
	ctx := context.Background()
	s3, _ := newMockS3Stores(t)
	for name, r := range map[string]Reclaimer{
		"mem": NewMemStores(),
		"os":  NewOsStores(t.TempDir()),
		"s3":  s3,
	} {
		if _, err := r.Reclaim(ctx, 0); !errors.Is(err, errNoGrace) {
			t.Errorf("%s: a grace of nothing: %v", name, err)
		}
	}
}

// TestS3ReclaimPutsAsideAndThenRemoves is the case flob#47 is about: an erased
// blob's bytes stayed in the bucket for as long as the bucket did.
func TestS3ReclaimPutsAsideAndThenRemoves(t *testing.T) {
	ctx := context.Background()
	stores, mock := newMockS3Stores(t)

	data := []byte("what an erasure has to make go")
	m, err := stores.Use("a").Add(ctx, Meta{}, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if err := stores.Use("a").Erase(ctx, m.Digest); err != nil {
		t.Fatal(err)
	}

	blob, trash := stores.blobKey(m.Digest), stores.trashKey(m.Digest)
	if !mock.has(blob) {
		t.Fatal("Erase took the bytes inline, so this proves nothing")
	}

	// Not yet as old as the grace: it may be a slow Add's.
	if n, err := stores.Reclaim(ctx, time.Hour); err != nil || n != 0 || !mock.has(blob) {
		t.Fatalf("a blob younger than the grace was taken: %d, %v", n, err)
	}

	mock.age(blob, 2*time.Hour)
	if n, err := stores.Reclaim(ctx, time.Hour); err != nil || n != 0 {
		t.Fatalf("first sweep: %d, %v", n, err)
	}
	if mock.has(blob) || !mock.has(trash) {
		t.Fatal("the first sweep did not put the blob aside")
	}

	// Aside for less than the grace: still there.
	if n, err := stores.Reclaim(ctx, time.Hour); err != nil || n != 0 || !mock.has(trash) {
		t.Fatalf("what was put aside went before the grace: %d, %v", n, err)
	}

	mock.age(trash, 2*time.Hour)
	if n, err := stores.Reclaim(ctx, time.Hour); err != nil || n != 1 {
		t.Fatalf("second sweep: %d, %v", n, err)
	}
	if mock.has(blob) || mock.has(trash) {
		t.Fatal("a copy of the bytes is still in the bucket")
	}
}

// TestS3ReclaimKeepsWhatARacingAddReferenced is the race that kept Erase from
// deleting inline: an Add finds the blob, skips its upload, and writes its
// reference after the sweep has listed the references.
func TestS3ReclaimKeepsWhatARacingAddReferenced(t *testing.T) {
	ctx := context.Background()
	stores, mock := newMockS3Stores(t)

	data := []byte("found by an Add that skipped its upload")
	m, err := stores.Use("a").Add(ctx, Meta{}, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if err := stores.Use("a").Erase(ctx, m.Digest); err != nil {
		t.Fatal(err)
	}

	blob, trash := stores.blobKey(m.Digest), stores.trashKey(m.Digest)
	mock.age(blob, 2*time.Hour)
	if _, err := stores.Reclaim(ctx, time.Hour); err != nil {
		t.Fatal(err)
	}

	// The reference the racing Add writes after the sweep listed them.
	if err := stores.putRef(ctx, m.Digest, "b", nil, int64(len(data))); err != nil {
		t.Fatal(err)
	}
	if got := read(t, stores.Use("b"), m.Digest); !bytes.Equal(got, data) {
		t.Fatalf("a committed reference read %q while its blob was aside", got)
	}

	if p, ok := AsPresigner(stores.Use("b")); ok {
		loc, _, err := p.PresignOpen(ctx, m.Digest, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(loc, "/trash/") {
			t.Fatalf("a presigned URL for a blob put aside points at %s", loc)
		}
	}

	mock.age(trash, 2*time.Hour)
	if n, err := stores.Reclaim(ctx, time.Hour); err != nil || n != 0 {
		t.Fatalf("a referenced blob was reclaimed: %d, %v", n, err)
	}
	if !mock.has(blob) || mock.has(trash) {
		t.Fatal("the referenced blob was not put back")
	}
	if got := read(t, stores.Use("b"), m.Digest); !bytes.Equal(got, data) {
		t.Fatalf("read %q after it was put back", got)
	}
}

// TestS3ReclaimLeavesReferencedBlobs.
func TestS3ReclaimLeavesReferencedBlobs(t *testing.T) {
	ctx := context.Background()
	stores, mock := newMockS3Stores(t)

	m, err := stores.Use("a").Add(ctx, Meta{}, strings.NewReader("still wanted"))
	if err != nil {
		t.Fatal(err)
	}
	mock.age(stores.blobKey(m.Digest), 2*time.Hour)

	if n, err := stores.Reclaim(ctx, time.Hour); err != nil || n != 0 {
		t.Fatalf("%d, %v", n, err)
	}
	if !mock.has(stores.blobKey(m.Digest)) {
		t.Fatal("a referenced blob was put aside")
	}
}

// TestS3ReclaimCopiesALargeBlobInParts.
func TestS3ReclaimCopiesALargeBlobInParts(t *testing.T) {
	ctx := context.Background()
	stores, mock := newMockS3Stores(t)

	limit, part := s3CopyLimit, s3CopyPart
	s3CopyLimit, s3CopyPart = 6<<20, 5<<20
	t.Cleanup(func() { s3CopyLimit, s3CopyPart = limit, part })

	data := bytes.Repeat([]byte("0123456789abcdef"), (11<<20)/16)
	m, err := stores.Use("a").Add(ctx, Meta{}, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if err := stores.Use("a").Erase(ctx, m.Digest); err != nil {
		t.Fatal(err)
	}

	mock.age(stores.blobKey(m.Digest), 2*time.Hour)
	if _, err := stores.Reclaim(ctx, time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := stores.putRef(ctx, m.Digest, "b", nil, int64(len(data))); err != nil {
		t.Fatal(err)
	}
	if got := read(t, stores.Use("b"), m.Digest); !bytes.Equal(got, data) {
		t.Fatal("a blob copied aside in parts does not read back whole")
	}
}

// TestOsReclaimTakesOnlyOrphans.
func TestOsReclaimTakesOnlyOrphans(t *testing.T) {
	if !nlinkKnown {
		t.Skip("link counts are not read on this platform")
	}

	ctx := context.Background()
	stores := NewOsStores(t.TempDir())
	s := stores.Use("a").(OsStore)

	orphan, err := s.Add(ctx, Meta{}, strings.NewReader("left behind by a crash"))
	if err != nil {
		t.Fatal(err)
	}
	kept, err := s.Add(ctx, Meta{}, strings.NewReader("still in a store"))
	if err != nil {
		t.Fatal(err)
	}

	// What a crash between an Erase and its cleanup leaves: the entry gone,
	// the shared link not.
	if err := os.RemoveAll(s.pathToRepo(orphan.Digest)); err != nil {
		t.Fatal(err)
	}

	old := time.Now().Add(-2 * time.Hour)
	for _, d := range []Digest{orphan.Digest, kept.Digest} {
		if err := os.Chtimes(s.pathToBlob(d), old, old); err != nil {
			t.Fatal(err)
		}
	}

	n, err := stores.Reclaim(ctx, time.Hour)
	if err != nil || n != 1 {
		t.Fatalf("%d, %v", n, err)
	}
	if _, err := os.Stat(s.pathToBlob(orphan.Digest)); !os.IsNotExist(err) {
		t.Fatal("the orphan is still there")
	}
	if got := read(t, s, kept.Digest); string(got) != "still in a store" {
		t.Fatalf("a blob a store holds read %q", got)
	}

	t.Run("and leaves one younger than the grace", func(t *testing.T) {
		young, err := s.Add(ctx, Meta{}, strings.NewReader("just written"))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.RemoveAll(s.pathToRepo(young.Digest)); err != nil {
			t.Fatal(err)
		}

		if n, err := stores.Reclaim(ctx, time.Hour); err != nil || n != 0 {
			t.Fatalf("%d, %v", n, err)
		}
		if _, err := os.Stat(s.pathToBlob(young.Digest)); err != nil {
			t.Fatal("a blob younger than the grace was taken")
		}
	})
}
