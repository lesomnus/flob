package flob

import (
	"context"
	"crypto/rand"
	"encoding"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"iter"
	"reflect"
	"time"

	"github.com/opencontainers/go-digest"
)

type Stores interface {
	Use(id string) Store
}

type Store interface {
	// Add adds a new blob to the store with [Meta], reading the content from r.
	// On success, it returns the complete [Meta] with the computed Digest.
	// Returned [Meta] may have additional fields set by the store, such as "Content-Type".
	// If a blob with the same digest already exists, it returns partial [Meta] with digest and [ErrAlreadyExists].
	// If m.Digest is set and [ErrAlreadyExists] is returned, r is not consumed so integrity of the existing blob is not verified.
	// If m.Digest is set, its algorithm is used to compute the digest; otherwise [Canonical] is used.
	// If m.Digest is set and if it does not match the computed digest, it returns [ErrDigestMismatch].
	// It may block until the blob is fully read from r even if the context is canceled, so it is caller's
	// responsibility to close r when the context is canceled.
	Add(ctx context.Context, m Meta, r io.Reader) (Meta, error)
	// Stat checks existence and retrieves blob information without requiring label access.
	// It returns [ErrNotExist] if the blob does not exist in this store.
	Stat(ctx context.Context, d Digest) (Info, error)
	// Open opens the blob with the given digest for reading without loading labels.
	// Labels can be requested separately through the returned [Info].
	// It returns [ErrNotExist] if the blob does not exist.
	Open(ctx context.Context, d Digest) (io.ReadSeekCloser, Info, error)
	// Label updates the labels of the blob with the given digest.
	// It returns [ErrNotExist] if the blob does not exist.
	Label(ctx context.Context, d Digest, labels Labels) error
	// Erase removes the blob with the given digest from the store.
	// It does not return [ErrNotExist] even if the blob does not exist.
	Erase(ctx context.Context, d Digest) error
}

// Presigner is an optional capability a [Store] may implement. Instead of
// streaming a blob's bytes, it hands out a short-lived direct URL to download it
// (e.g. an S3 presigned URL), letting a server redirect clients straight to the
// backing object store. A [Store] that does not implement Presigner is served by
// streaming through [Store.Open] as usual.
type Presigner interface {
	// PresignOpen returns a direct download URL for the blob with the given
	// digest — valid for approximately ttl — together with its [Meta]. It returns
	// [ErrNotExist] if this store has no such blob. The URL grants bearer access
	// to the blob for its lifetime, so ttl should be kept short. Like [Store.Open],
	// visibility is scoped to the store: a blob added only to another store yields
	// [ErrNotExist].
	PresignOpen(ctx context.Context, d Digest, ttl time.Duration) (url string, m Meta, err error)
}

// storeUnwrapper is implemented by a [Store] decorator to expose the store it
// wraps, so optional capabilities such as [Presigner] can be discovered through a
// chain of decorators. It mirrors the errors.Unwrap convention. A decorator that
// only observes or augments the [Store] methods (tracing, metrics, ...) should
// implement it so it does not hide capabilities of the store beneath it.
type storeUnwrapper interface {
	Unwrap() Store
}

// AsPresigner returns the first [Presigner] in s's decorator chain, following any
// Unwrap() Store methods (see [storeUnwrapper]), or false if none is found. Use
// this instead of a bare type assertion so a store wrapped in tracing/metrics
// decorators still exposes presign support.
func AsPresigner(s Store) (Presigner, bool) {
	for s != nil {
		if p, ok := s.(Presigner); ok {
			return p, true
		}
		u, ok := s.(storeUnwrapper)
		if !ok {
			return nil, false
		}
		s = u.Unwrap()
	}
	return nil, false
}

type Meta struct {
	Digest Digest
	Labels Labels
	Size   int64
}

func (m *Meta) Clone() Meta {
	m_ := *m
	m_.Labels = cloneLabels(m.Labels)
	return m_
}

// Linker is an optional capability for adding a reference to an existing blob
// without reading or copying its content. The source and destination must share
// a compatible backing pool: the same filesystem root, MemStores instance, or
// S3Stores instance. Link copies source labels independently into the destination.
type Linker interface {
	// Link adds d to the destination using from's existing reference. It returns
	// ErrNotExist if the source does not contain d, including when the destination
	// already contains it. Otherwise an existing destination returns
	// ErrAlreadyExists with partial metadata and without changing its labels.
	// Invalid digests return the
	// validation error from Digest.Sanitize; incompatible sources return
	// ErrIncompatibleStore.
	// Source decorators exposing Unwrap() Store are followed to the underlying
	// store. CacheStore and FallbackStore therefore expose only their primary
	// store as the source; their read fallbacks do not apply.
	Link(ctx context.Context, d Digest, from Store) (Meta, error)
}

// AsLinker returns the first Linker in s's decorator chain, following Unwrap()
// Store methods, or false if none is found. CacheStore and FallbackStore expose
// their primary store. Decorator Add policies, including duplicate handling and
// digest preparation, are not automatically applied to Link.
func AsLinker(s Store) (Linker, bool) {
	for s != nil {
		if l, ok := s.(Linker); ok {
			return l, true
		}
		u, ok := s.(storeUnwrapper)
		if !ok {
			return nil, false
		}
		s = u.Unwrap()
	}
	return nil, false
}

func unwrapLinkSource(s Store) Store {
	for s != nil {
		u, ok := s.(storeUnwrapper)
		if !ok {
			return s
		}
		s = u.Unwrap()
	}
	return nil
}

// Walker optionally inventories blobs physically held in a store's namespace.
// Walk yields lazy Info values without promising order or a coherent snapshot.
// Concurrent changes may be omitted. A failure is yielded once, then iteration
// stops. Breaking iteration stops further work; cancellation yields ctx.Err().
type Walker interface {
	Walk(context.Context) iter.Seq2[Info, error]
}

// Namespacer optionally inventories namespaces containing at least one valid
// blob reference. Merely calling Use does not create an enumerable namespace.
// Namespaces has the same ordering, snapshot, cancellation, and error semantics
// as Walker.Walk.
type Namespacer interface {
	Namespaces(context.Context) iter.Seq2[string, error]
}

// AsWalker follows Unwrap() Store to discover physical inventory support.
// For cache/fallback decorators this inventories the primary, not a union.
func AsWalker(s Store) (Walker, bool) {
	for s != nil {
		if w, ok := s.(Walker); ok {
			return w, true
		}
		u, ok := s.(interface{ Unwrap() Store })
		if !ok {
			break
		}
		s = u.Unwrap()
	}
	return nil, false
}

// AsNamespacer follows Unwrap() Stores to discover namespace inventory support.
func AsNamespacer(s Stores) (Namespacer, bool) {
	for s != nil {
		if n, ok := s.(Namespacer); ok {
			return n, true
		}
		u, ok := s.(interface{ Unwrap() Stores })
		if !ok {
			break
		}
		s = u.Unwrap()
	}
	return nil, false
}

// Stager optionally provides namespace-scoped, resumable uploads. Begin fixes
// the digest algorithm (empty selects Canonical). Resume does not extend expiry.
// OS and S3 stages survive process restarts; memory stages live with their pool.
type Stager interface {
	Begin(context.Context, digest.Algorithm) (Stage, error)
	Resume(context.Context, string) (Stage, error)
}

// Stage is a handle, not an open file or connection. Each operation acquires and
// releases its own resources. IDs are opaque and usable only in their namespace.
type Stage interface {
	ID() string
	Stat(context.Context) (StageInfo, error)
	// Append atomically publishes all of r, or none of it, at expectedOffset.
	// The returned position is the new durable offset. A lost response leaves
	// the outcome uncertain: use Stat before retrying. Conflicting writers may
	// have consumed input before detecting a conflict. Callers must arrange to
	// interrupt a blocking input reader when the operation context is canceled.
	Append(ctx context.Context, expectedOffset int64, r io.Reader) (int64, error)
	// Commit verifies the digest and freezes labels before making the blob
	// visible. Size is derived from stored bytes. Empty Digest uses the computed
	// digest. Retrying the same commit returns its recorded result; different
	// parameters conflict. An existing blob is success with its existing labels.
	Commit(context.Context, Meta) (Meta, error)
	// Abort is idempotent. A commit already in progress is recovered first;
	// aborting or pruning its stage never deletes a published blob.
	Abort(context.Context) error
}

type StageState string

const (
	StageActive     StageState = "active"
	StageCommitting StageState = "committing"
	StageCommitted  StageState = "committed"
	StageAborted    StageState = "aborted"
)

// StageInfo is a fresh snapshot; obtaining it does not extend the stage lifetime.
type StageInfo struct {
	Algorithm digest.Algorithm
	Offset    int64
	State     StageState
	ExpiresAt time.Time
}

// StageConfig controls server-owned upload lifetimes. Nonpositive durations use
// defaults: 24 hours idle TTL, 24 hours terminal receipt retention, 15 minutes
// per operation. Only a successful Append renews an active stage's TTL.
// Configuration must remain fixed after constructing a pool.
type StageConfig struct {
	TTL              time.Duration
	Retention        time.Duration
	OperationTimeout time.Duration
	now              func() time.Time
}

func (c StageConfig) normalized() StageConfig {
	if c.TTL <= 0 {
		c.TTL = 24 * time.Hour
	}
	if c.Retention <= 0 {
		c.Retention = 24 * time.Hour
	}
	if c.OperationTimeout <= 0 {
		c.OperationTimeout = 15 * time.Minute
	}
	return c
}
func (c StageConfig) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

// StageCleaner is a pool-wide maintenance capability. The host invokes it on a
// schedule; flob does not start a background sweeper. PruneStages removes expired
// stages and abandoned temporary data, skipping active operations and recovering
// interrupted commits before deletion. The count is removed stage records;
// cleanup may have made progress even when it returns an error.
type StageCleaner interface {
	PruneStages(context.Context) (int, error)
}

func AsStager(s Store) (Stager, bool) {
	for s != nil {
		if st, ok := s.(Stager); ok {
			return st, true
		}
		u, ok := s.(storeUnwrapper)
		if !ok {
			break
		}
		s = u.Unwrap()
	}
	return nil, false
}
func AsStageCleaner(s Stores) (StageCleaner, bool) {
	for s != nil {
		if st, ok := s.(StageCleaner); ok {
			return st, true
		}
		u, ok := s.(interface{ Unwrap() Stores })
		if !ok {
			break
		}
		s = u.Unwrap()
	}
	return nil, false
}

// Versioned checkpoint shared by implementations. A checkpoint describes only
// committed append bytes; unreferenced attempt data is never part of its hash.
type stageRecord struct {
	Version   int
	ID        string
	Namespace string // Canonical namespaceSegment encoding preserves arbitrary bytes in JSON.
	Algorithm digest.Algorithm
	Offset    int64
	Hash      []byte
	State     StageState
	ExpiresAt time.Time
	Commit    Meta
	Result    Meta
}

func newStageRecord(namespace string, algo digest.Algorithm, cfg StageConfig) (stageRecord, error) {
	if algo == "" {
		algo = Canonical
	}
	h, err := stageHashRestore(algo, nil)
	if err != nil {
		return stageRecord{}, err
	}
	state, err := stageHashState(h)
	if err != nil {
		return stageRecord{}, err
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return stageRecord{}, err
	}
	return stageRecord{Version: 1, ID: hex.EncodeToString(id[:]), Namespace: namespaceSegment(namespace), Algorithm: algo, Hash: state, State: StageActive, ExpiresAt: cfg.clock().Add(cfg.normalized().TTL)}, nil
}
func validStageID(id string) bool {
	if len(id) != 32 {
		return false
	}
	for _, c := range id {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
func stageHashRestore(algo digest.Algorithm, state []byte) (hash.Hash, error) {
	switch algo {
	case digest.SHA256, digest.SHA384, digest.SHA512:
	default:
		return nil, fmt.Errorf("%w: algorithm %q", ErrInvalidDigest, algo)
	}
	h := algo.Hash()
	if len(state) > 0 {
		u, ok := h.(encoding.BinaryUnmarshaler)
		if !ok {
			return nil, ErrStageFormat
		}
		if err := u.UnmarshalBinary(state); err != nil {
			return nil, fmt.Errorf("%w: hash checkpoint: %v", ErrStageFormat, err)
		}
	}
	return h, nil
}
func stageHashState(h hash.Hash) ([]byte, error) {
	m, ok := h.(encoding.BinaryMarshaler)
	if !ok {
		return nil, ErrStageFormat
	}
	return m.MarshalBinary()
}
func (r stageRecord) info() StageInfo {
	return StageInfo{Algorithm: r.Algorithm, Offset: r.Offset, State: r.State, ExpiresAt: r.ExpiresAt}
}
func (r stageRecord) expired(now time.Time) bool { return !now.Before(r.ExpiresAt) }
func (r stageRecord) prepareCommit(m Meta) (Meta, error) {
	if err := r.validate(); err != nil {
		return Meta{}, err
	}
	h, err := stageHashRestore(r.Algorithm, r.Hash)
	if err != nil {
		return Meta{}, err
	}
	computed := Digest(fmt.Sprintf("%s:%x", r.Algorithm, h.Sum(nil)))
	if m.Digest != "" {
		d, err := m.Digest.Sanitize()
		if err != nil {
			return Meta{}, err
		}
		if d != computed {
			return Meta{}, ErrDigestMismatch
		}
	}
	m = m.Clone()
	m.Digest = computed
	m.Size = r.Offset
	return m, nil
}
func stageCommitMatches(a, b Meta) bool {
	return a.Digest == b.Digest && a.Size == b.Size && reflect.DeepEqual(a.Labels, b.Labels)
}

// Readers cannot be forcibly interrupted through io.Reader alone. Network
// handlers should close their input on cancellation, as with Store.Add.
type stageContextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r stageContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.r.Read(p)
	if canceled := r.ctx.Err(); canceled != nil {
		return n, canceled
	}
	return n, err
}

// Version 1 uses Go's SHA-2 binary checkpoint format. Its final uint64 is the
// number of input bytes, and must agree with the manifest's published offset.
func (r stageRecord) validate() error {
	namespace, err := namespaceID(r.Namespace)
	if err != nil || namespaceSegment(namespace) != r.Namespace {
		return ErrStageFormat
	}
	if r.Version != 1 || !validStageID(r.ID) || r.Offset < 0 || len(r.Hash) < 8 {
		return ErrStageFormat
	}
	h, err := stageHashRestore(r.Algorithm, r.Hash)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrStageFormat, err)
	}
	if binary.BigEndian.Uint64(r.Hash[len(r.Hash)-8:]) != uint64(r.Offset) {
		return ErrStageFormat
	}
	switch r.State {
	case StageActive, StageCommitting, StageCommitted, StageAborted:
	default:
		return ErrStageFormat
	}
	if r.ExpiresAt.IsZero() {
		return ErrStageFormat
	}
	if r.State == StageCommitting || r.State == StageCommitted {
		computed := Digest(fmt.Sprintf("%s:%x", r.Algorithm, h.Sum(nil)))
		if r.Commit.Digest != computed || r.Commit.Size != r.Offset {
			return ErrStageFormat
		}
		if r.State == StageCommitted && (r.Result.Digest != computed || r.Result.Size != r.Offset) {
			return ErrStageFormat
		}
	}
	return nil
}
