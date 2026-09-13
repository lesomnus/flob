# flob

A content-addressable storage (CAS) backed purely by the filesystem — no database required.
Blobs are identified by their digest and organized under isolated 1-depth namespaced stores.
`Add` uses SHA-256 by default, or the algorithm of a supplied `Meta.Digest` (SHA-256, SHA-384, or SHA-512).

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
  AWS SDK dependency; blobs are deduplicated by digest key and stores are isolated
  by per-store reference markers (see [`s3.md`](./s3.md)).

## Read-through cache

Use `NewCacheStores(primary, origin)` for a cache shared across namespaces, or
`NewCacheStore(primary, origin)` for one namespace. Concurrent misses for the
same namespace and digest share the first cache fill, including across repeated
`Use(id)` calls. The first caller streams immediately; later callers wait for the
primary write to commit and can cancel their own wait independently. An aborted
or failed fill releases waiters to fetch from the origin themselves.

Cache writes are best-effort. Read the entire blob and close its reader to allow
caching to finish. Size probes and rereads of a prefix are supported; incomplete
reads and reads that skip a gap abort the fill. Leader cancellation also releases
waiters and closes its source reader. Backends must honor operation contexts and
allow `Close` to interrupt outstanding reads.

`CacheStores` and `CacheStore` now implement their interfaces as pointers.
Use the constructors or `&CacheStores{...}` / `&CacheStore{...}`. Do not copy them
or change their `Primary` or `Origin` fields after first use. Coordination is
instance-local, and completed flights retain neither digest nor namespace keys.

## Link a blob between namespaces

`Linker` adds an existing blob to another namespace without reading, hashing, or
copying the blob content. The filesystem, memory, and S3 backends support it:

```go
source := stores.Use("source")
destination := stores.Use("destination")
linker, ok := flob.AsLinker(destination)
if !ok {
    return flob.ErrUnimplemented
}
meta, err := linker.Link(ctx, digest, source)
```

The source must contain the digest in its own namespace; global blob presence
alone is insufficient. `Link` copies its labels into independent destination
metadata. Later label changes or deletion of the source do not affect the linked
blob. An existing destination returns `ErrAlreadyExists` with partial metadata
and keeps its labels. A missing source returns `ErrNotExist`, even if the
destination already exists. Invalid digests return a validation error.

Source and destination must share a backing pool: the same `MemStores` or
`S3Stores` instance, or the same cleaned absolute filesystem root path. Other
combinations return `ErrIncompatibleStore`; there is no automatic copy fallback.
OS uses a staged hard link and atomic publication; memory shares the immutable
blob and increments its reference count; S3 reads the source reference metadata
and conditionally creates an empty destination reference.

`AsLinker` follows `Unwrap` through decorators. Source decorators are also
unwrapped, so cache/fallback stores participate through their primary only;
origin-only blobs cannot be linked through them. Decorator policies on `Add`
(such as `AllowDuplicates`) do not change `Link` behavior. HTTP does not expose a
link endpoint or `Linker` capability.

## Namespace IDs

`Use(id)` identifies a namespace by the exact bytes of `id`. OS directory names,
S3 reference suffixes, and HTTP path segments use one shared encoding. Nonempty
ASCII names containing only letters, digits, `_`, `-`, and `.` stay unchanged,
except names ending in `.`, and Windows device names such as `NUL` or `CON.txt`.
All other IDs become `~` followed by unpadded URL-safe base64 of their bytes.
The prefix is reserved: an ID beginning with `~` is itself encoded. For example,
`a/b` becomes `~YS9i`, `..` becomes `~Li4`, and the empty ID becomes `~`.
Namespaces therefore occupy single segments under `repos/` or S3 `refs/`.

Storage limits still apply. On filesystems with a 255-byte filename limit,
encoded IDs can contain at most 190 bytes before encoding; preserved names can
contain at most 255 bytes. Full path limits may impose smaller bounds, particularly
on Windows. OS namespace isolation requires a case-sensitive filesystem; names
that differ only by case are distinct IDs. S3's total key-length limit also
includes the configured prefix and digest. Oversized names fail through backend
operations; `Use` does not promise that every length can be stored.

**Migration:** stores containing only preserved plain names need no migration.
If any existing ID requires encoding, plan and export data **before upgrading**.
Old and new layouts cannot safely be mixed: for example, a legacy raw namespace
named `~Lg` occupies the new location for the ID `.`. Reading that location after
upgrading would expose the old namespace through a different ID. This affects
both OS directories and S3 reference suffixes, especially existing `~`-prefixed
names. Names previously stored raw that now require encoding include slash,
empty, dot, device, and `~`-prefixed IDs.

For these layouts, export blobs and labels using the old version, then import
through `Use(originalID)` into a **fresh store root or S3 prefix** using the new
version before switching traffic. Keep the old data separately until verified.
An in-place migration requires an offline, collision-aware relocation of all
affected namespace references while preserving blob links and metadata; do not
simply rename one entry while leaving conflicting legacy entries accessible.
There is no automatic migration or fallback to unsafe raw paths. Update custom
HTTP clients to use the encoded segment; `HttpStores` handles it automatically.
Back up legacy layouts before migration, since old traversal IDs may have written
outside `repos/` or the store root.

## Enumeration

Memory, OS, and S3 backends expose optional `Walker` and `Namespacer`
capabilities. `Walk(ctx)` yields `Info` and errors; `Namespaces(ctx)` yields IDs
of namespaces containing at least one valid blob reference. Calling `Use` alone
or erasing the last blob does not leave an enumerable namespace.

```go
if walker, ok := flob.AsWalker(store); ok {
    for info, err := range walker.Walk(ctx) {
        if err != nil { return err }
        fmt.Println(info.Digest(), info.Size())
        // info.Labels(ctx) loads labels only if needed.
    }
}
if namespaces, ok := flob.AsNamespacer(stores); ok {
    for id, err := range namespaces.Namespaces(ctx) {
        if err != nil { return err }
        fmt.Println(id)
    }
}
```

These are physical inventories with no ordering or snapshot guarantee during
concurrent writes. Invalid layout entries are skipped. A failure yields one
terminal error; cancellation stops work and reports the context error. Breaking
the loop stops further I/O. `AsWalker` and `AsNamespacer` follow decorator
`Unwrap` methods; cache and fallback inventories describe their primary storage,
not a union with the origin or secondary. HTTP does not expose enumeration.

## Blob information and lazy labels

`Store.Stat` checks existence and returns an `Info` with the blob's digest and
size. `Store.Open` returns the same kind of information alongside its reader.
Labels are loaded only when requested:

```go
info, err := store.Stat(ctx, digest)
if err != nil {
    return err
}
size := info.Size()
labels, err := info.Labels(ctx)
```

Filesystem `Stat` and `Open` do not open the labels file. S3 and HTTP already
receive labels with their metadata response, so accessing them needs no extra
request. Missing blobs return `ErrNotExist` from `Stat` or `Open` immediately;
label-loading errors are reported by `Labels`.

Each `Info` caches its first successful labels load and returns independent
copies. Failed loads can be retried, and each attempt uses the context supplied
to `Labels`. Digest, size, and labels are not guaranteed to describe one atomic
snapshot: the blob or labels may change between the initial lookup and the
first labels access. Obtain a new `Info` to refresh a successful labels result.
Cache and fallback stores keep labels bound to the store that supplied the info.

This is a breaking API change: `Get`, the optional `Stater`, and `AsStater` have
been removed. Replace `Get` with `Stat`, use `Digest()` and `Size()`, and call
`Labels(ctx)` when needed. Store implementations must implement `Stat` and
return `Info` from `Open`; `NewInfo` provides a loader with the caching behavior
above. `Meta` remains the data type used by `Add` and `PresignOpen`.

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
  `Stat`/`HEAD` (over HTTP, this is a `200 OK` with only the `ETag` set — see `http.md`).
- **Consistent existence semantics.** "Does this blob exist?" is answered by the presence of
  the `blob` file everywhere (`Stat`, `Open`, `Label`), so an orphaned labels-only directory
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

## Resumable staged uploads

Memory, OS, and S3 stores implement the optional `Stager` capability. A stage
belongs to one namespace and stays invisible to `Open`, `Walk`, and `Namespaces`
until committed. OS and S3 persist stages across process restarts; memory stages
last only as long as their pool. Existing `Add` behavior is unchanged. HTTP does
not expose staging endpoints; an application such as a registry maps its upload
protocol to these Go APIs.

```go
stores := flob.NewOsStores("./data", flob.StageConfig{
    TTL:              24 * time.Hour,
    Retention:        24 * time.Hour,
    OperationTimeout: 15 * time.Minute,
})
store := stores.Use("registry")
stager, ok := flob.AsStager(store)
if !ok { return flob.ErrUnimplemented }

stage, err := stager.Begin(ctx, flob.Canonical)
if err != nil { return err }
// Persist this ID in the application's upload session before receiving chunks.
id := stage.ID()

next, err := stage.Append(ctx, 0, firstChunk)
if err != nil { return err }

// A subsequent request, or a new process using the same pool, resumes by ID.
stage, err = stager.Resume(ctx, id)
if err != nil { return err }
next, err = stage.Append(ctx, next, secondChunk)
if err != nil { return err }
result, err := stage.Commit(ctx, flob.Meta{Digest: expectedDigest, Labels: labels})
if err != nil { return err }
fmt.Println(result.Digest, result.Size)
```

`Append` publishes an entire input or none of it. It checks the caller's expected
offset and returns the new offset, not a byte count. After an uncertain network
failure, call `stage.Stat(ctx)` to determine the authoritative offset before
retrying. Concurrent writers are serialized or rejected with `ErrStageConflict`
/ `ErrOffsetMismatch`; a losing writer may already have consumed input. Separate
`Resume` handles do not reserve a stage. A handle owns no open connection or file
and needs no `Close`.

The algorithm is fixed at `Begin` (empty means SHA-256; SHA-384 and SHA-512 are
also supported). Hash checkpoints and offsets are persisted together so normal
resume and commit need no complete rehash. A commit with a wrong digest leaves
the active stage intact. `Meta.Size` is derived from stored bytes, and an empty
commit digest uses the calculated digest. A validated commit freezes its digest
and labels; after an interruption, repeat it with the same parameters. Different
parameters conflict. An already present blob counts as success with its existing
metadata, without overwriting labels. The recorded result is returned on retries
even if the blob is subsequently erased. This differs from `Add`, which continues
to return `ErrAlreadyExists` for duplicates.

### Expiry and maintenance

Nonpositive configuration durations select the defaults shown above. Only a
successful `Append` refreshes the idle TTL; `Stat` and `Resume` do not. Expired
stages reject access with `ErrStageExpired`, even before their storage is
reclaimed. Committed and aborted receipts use the separate retention period.
After a receipt is pruned, its ID is no longer resumable. `StageInfo.ExpiresAt`
lets applications report the remaining lifetime.

Use `Abort` to discard an upload. It is idempotent and never removes a published
blob. A commit that has already frozen its parameters is completed/recovered
before abort or cleanup can remove temporary data. A storage outage can postpone
this recovery; a failed cleanup reports the error and can be retried.

```go
// Invoke periodically from the host's scheduler, using a bounded context.
if cleaner, ok := flob.AsStageCleaner(stores); ok {
    removed, err := cleaner.PruneStages(ctx)
    // Report err and retry on a later maintenance pass.
    _ = removed
    _ = err
}
```

The library does not start a background sweeper. Run maintenance even when users
never call `Abort`: clients can disconnect or lose their upload IDs. The return
count is removed stage records, not bytes or temporary objects. Cleanup can make
partial progress before returning an error. Active operations are protected;
OS process locks are released after process death, and S3 leases expire with the
operation timeout and use conditional updates to reject stale writers. Orphaned
attempts are reclaimed after a grace period, so physical reclamation need not
coincide exactly with `ExpiresAt`.

Operation contexts bound storage I/O. As with `Add`, an arbitrary `io.Reader`
cannot be forcibly interrupted: the host must close or otherwise unblock input
on cancellation. For network uploads, connect request cancellation to the input
body's lifetime. A stalled local reader can retain an OS process lock until it
returns or its process exits.

OS stages use versioned manifests and synced append checkpoints in `uploads/`;
commit publishes hard links on the same filesystem. S3 uses immutable chunks and
conditional manifests, and assembles the final object through server-side
multipart copies without local disk or full-content downloads. See
[S3 staged uploads](s3.md#staged-uploads) for capabilities, limits, permissions,
and cleanup requirements. Hash checkpoint format version 1 uses Go's SHA-2
binary state formats; unsupported or corrupt checkpoints return `ErrStageFormat`
rather than silently publishing unverified content.
