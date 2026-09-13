# flob

A content-addressable storage (CAS) backed purely by the filesystem — no database required.
Blobs are identified by their SHA-256 digest and organized under isolated 1-depth namespaced stores.

## Usage

```go
import "github.com/lesomnus/flob"

func main() {
	stores := flob.NewOsStores("/path/to/storage")

	// Store "foo" and "bar" are independent namespaces but data are deduplicated 
	// and shared across them if the same content is added.
	store_foo := stores.Use("foo")
	store_bar := stores.Use("bar")

	ctx := context.Background()
	meta, _ := store_foo.Add(ctx, []byte("hello world"))

	r, _, _ := store_foo.Open(ctx, meta.Digest) 
	io.ReadAll(r) // "hello world"

	_, _, err := store_bar.Open(ctx, meta.Digest)
	err // flob.ErrNotExist

	// same content, same digest, no duplicate storage.
	store_bar.Add(ctx, []byte("hello world")) 
}

```

## CLI

`cmd/flob` is a separate module (`github.com/lesomnus/flob/cmd/flob`) wrapping
the same stores. Its commands, configuration, and **exit codes** — `3` for
"already there", `4` for "not there", both of which a script wants to tell from
a real failure — are documented in [`cmd/flob/README.md`](./cmd/flob/README.md).

## Backends

The same `Stores`/`Store` interface has several backends, all with identical
semantics (cross-store dedup, per-store visibility):

- **`OsStores`** — filesystem CAS (described below).
- **`MemStores`** — in-memory, for tests and caches.
- **`HttpStores`** / `HttpHandler` — client/server over HTTP (see [`http.md`](./http.md)).
- **`S3Stores`** — any S3-compatible bucket (AWS S3, MinIO) over plain HTTP with no
  AWS SDK dependency; blobs are deduplicated by SHA-256 key and stores are isolated
  by per-store reference markers (see [`s3.md`](./s3.md)).

## Existence and size

`Stater` is an optional capability for checking existence and size without
retrieving labels. Discover it through decorators with `AsStater`:

```go
var size int64
var err error
if st, ok := flob.AsStater(store); ok {
    size, err = st.Stat(ctx, digest)
} else {
    var meta flob.Meta
    meta, err = store.Get(ctx, digest)
    size = meta.Size
}
```

The filesystem, memory, and S3 backends implement it. Filesystem `Stat` reads
only the blob's file information; S3 sends a HEAD for the store's reference
marker. Missing blobs return `ErrNotExist`, including blobs only present in
another store. `CacheStore` and `FallbackStore` preserve their usual read
fallbacks, using `Get` for children without this capability.

## Design & Consistency

`flob` keeps no database. The filesystem layout *is* the index:

```
<root>/share/<algo>/xx/xx/<rest>          the one physical copy of each blob
<root>/repos/<id>/<algo>/xx/xx/<rest>/    a per-store entry (hard link to the blob + labels)
```

Deduplication and reference counting are delegated to the filesystem itself:

- **Dedup** — a repo's `blob` is a hard link to the shared `share/` inode, so the same
  content added to many stores costs one inode.
- **Reference counting** — the shared inode's *hard-link count* is the reference count. On
  `Erase`, `flob` removes the repo link and, if the shared inode has no remaining links,
  removes it too. No separate refcount table, and no GC sweep, is required.
- **Atomicity** — a blob is fully assembled in `stage/` and moved into place with a single
  `os.Rename`, so a partially written entry is never observable.
- **Isolation** — an `Add`/`Erase` for a given digest is serialized by a per-digest file
  lock (`locks/`), so concurrent writers of the same content do not corrupt each other.

### Deliberate tolerance of leaks under concurrency

The top design priority is that **a user's `Add`/`Erase` request always succeeds and a
failure is never surfaced to the user** — even under heavy concurrency. To keep the
implementation simple and lock-free on the read path, `flob` deliberately accepts a small
amount of *storage leakage* rather than paying for perfect bookkeeping:

- **Partial deduplication.** If two processes add the same brand-new blob at the exact
  moment its shared inode is being cleaned up, they may end up with two physical copies of
  the content instead of one. Both copies are correct and fully readable; only the disk
  saving is temporarily lost.
- **Orphaned blobs/labels.** An `Erase` that races with a concurrent `Add` (or a crash
  midway through `Erase`, which unlinks `blob`, then `labels`, then the directory) can leave
  an orphaned inode or a labels file behind. It wastes space but is invisible to every read
  operation and never causes a wrong result.

These outcomes are **intentional trade-offs, not bugs**: correctness (right content for a
digest, request always succeeds) is preserved, and the only cost is reclaimable disk space.

### Other accepted concurrency behaviors

Beyond storage leakage, a few observable-but-benign concurrency behaviors are deliberately
accepted rather than serialized away, for the same "keep it simple, never fail the user"
reason:

- **Duplicate add returns partial metadata.** When you `Add` content whose digest already
  exists, the existing blob is intentionally *not* re-read, so the returned `Meta` may carry
  `Size == 0`. The stored content is untouched and authoritative; fetch the real size with
  `Get`/`HEAD` (over HTTP, this is a `200 OK` with only the `ETag` set — see `http.md`).
- **Consistent existence semantics.** "Does this blob exist?" is answered by the presence of
  the `blob` file everywhere (`Get`, `Open`, `Label`), so an orphaned labels-only directory
  is uniformly treated as *not existing*; `Label` on such a stray entry returns `ErrNotExist`
  just like a read would.
- **In-memory store `Label`/`Erase` races are not serialized.** For `MemStore`, a `Label`
  concurrent with an `Erase` may or may not leave the label behind depending on ordering, but
  every ordering preserves the observable invariant (a label never outlives its blob, and a
  live blob is never left in a corrupt state). Serialization is intentionally omitted.

### Why there is no garbage collector (yet)

A background GC that reclaims orphaned inodes is the obvious next step, but it is
**intentionally deferred**. A naive sweeper cannot easily distinguish an orphan from an
inode that a slow, in-flight `Add` has created but not yet committed (linked into a repo).
Collecting such an inode would corrupt a legitimate, succeeding upload — exactly the kind of
user-visible failure the design refuses to introduce. Until the sweeper can prove an inode
is safe to reclaim (e.g. via staging generations or age/liveness bookkeeping), tolerating
the leak is preferred over risking a live upload.

### Platform note

On non-Linux platforms the hard-link count is not read (see `nlink.go`), so `Erase` always
believes it is removing the last reference and deletes the shared `share/` inode eagerly.
Per-store hard links keep the content fully readable, so this is a *dedup degradation*
(subsequent adds re-copy the content), never data loss.
