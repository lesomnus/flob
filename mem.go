package flob

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/opencontainers/go-digest"
	"io"
	"iter"
	"sync"
	"sync/atomic"
	"time"
)

var (
	_ Stores = (*MemStores)(nil)
	_ Store  = (*MemStore)(nil)
)

// MemStores is an in-memory [Stores] implementation.
// All stores share a single global namespace: blobs with the same digest reuse
// the same underlying [memBlob] and are reference-counted under a mutex, so no
// GC sweep is required for committed blobs. Abandoned upload stages must still
// be pruned with PruneStages.
type MemStores struct {
	mu     sync.Mutex
	bs     sync.Map // map[Digest]*memBlob
	ss     sync.Map // map[string]*MemStore
	stages sync.Map // map[string]*memStageEntry
	stage  StageConfig
}

func NewMemStores(config ...StageConfig) *MemStores {
	var cfg StageConfig
	if len(config) > 0 {
		cfg = config[0]
	}
	return &MemStores{stage: cfg.normalized()}
}

func (s *MemStores) Use(id string) Store {
	v, _ := s.ss.LoadOrStore(id, &MemStore{g: s, id: id})
	return v.(*MemStore)
}

type MemStore struct {
	g  *MemStores
	id string
	es sync.Map // map[Digest]*memEntry
}

func (s *MemStore) Add(ctx context.Context, m Meta, r io.Reader) (Meta, error) {
	algo := Canonical
	if m.Digest != "" {
		d, err := m.Digest.Sanitize()
		if err != nil {
			return m, err
		}
		m.Digest = d
		algo = d.Algorithm()

		if _, ok := s.es.Load(d); ok {
			return m, ErrAlreadyExists
		}
	}

	data, err := io.ReadAll(r)
	if err != nil {
		return m, fmt.Errorf("read: %w", err)
	}

	d := Digest(algo.FromBytes(data))
	m.Size = int64(len(data))

	if m.Digest == "" {
		m.Digest = d
	} else if m.Digest != d {
		return m, ErrDigestMismatch
	}

	return s.publish(m, data)
}

// publish installs already verified immutable bytes and retains Add's duplicate
// behavior. Staged commits use the same publication path without rehashing.
func (s *MemStore) publish(m Meta, data []byte) (Meta, error) {
	d := m.Digest
	b_new := &memBlob{data: data, refs: 0}
	b := b_new
	for {
		b = b_new

		// Between store and init the blob, it is possible to be seen by another
		// goroutine, so we need to lock the blob until it is fully initialized.
		b.mu.Lock()

		if b_, ok := s.g.bs.LoadOrStore(d, b); !ok {
			// This is the first blob with this digest, so we use it.
			break
		} else {
			// There was already a blob with the same digest, so we use it instead
			// of the new one.
			b.mu.Unlock()

			b = b_.(*memBlob)
			b.mu.Lock()
			if b.refs == 0 {
				// The blob is being deleted while we wait for the lock, so we need
				// to retry.
				b.mu.Unlock()
				continue
			}
			break
		}
	}
	defer b.mu.Unlock()

	e := &memEntry{blob: b}
	e.added.Store(time.Now().UnixNano())
	if m.Labels != nil {
		ls := cloneLabels(m.Labels)
		e.labels.Store(&ls)
	}

	if _, ok := s.es.LoadOrStore(d, e); ok {
		return m, ErrAlreadyExists
	}

	b.refs++

	return m.Clone(), nil
}

func (s *MemStore) Stat(ctx context.Context, d Digest) (Info, error) {
	_, info, err := s.open(d)
	return info, err
}

func (s *MemStore) Open(ctx context.Context, d Digest) (io.ReadSeekCloser, Info, error) {
	entry, info, err := s.open(d)
	if err != nil {
		return nil, nil, err
	}
	return nopCloser{bytes.NewReader(entry.blob.data)}, info, nil
}

func (s *MemStore) open(d Digest) (*memEntry, Info, error) {
	v, ok := s.es.Load(d)
	if !ok {
		return nil, nil, ErrNotExist
	}
	entry := v.(*memEntry)
	info := NewInfo(d, int64(len(entry.blob.data)), time.Unix(0, entry.added.Load()), func(ctx context.Context) (Labels, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		ls := entry.labels.Load()
		if ls == nil {
			return nil, nil
		}
		return *ls, nil
	})
	return entry, info, nil
}

func (s *MemStore) Label(ctx context.Context, d Digest, labels Labels) error {
	v, ok := s.es.Load(d)
	if !ok {
		return ErrNotExist
	}

	// If Label and Erase race, two orderings are possible:
	// (a) Label writes then Erase removes the entry, so the written labels are lost with the entry
	// (b) Erase removes the entry first, causing Label's Load to miss and return [ErrNotExist].
	// In neither case can a label survive, so the observable invariant is preserved.
	// Serialising Label and Erase is intentionally not implemented.

	entry := v.(*memEntry)
	ls := cloneLabels(labels)
	entry.labels.Store(&ls)
	entry.added.Store(time.Now().UnixNano())

	return nil
}

func (s *MemStore) Erase(ctx context.Context, d Digest) error {
	v, ok := s.es.LoadAndDelete(d)
	if !ok {
		return nil
	}

	entry := v.(*memEntry)
	entry.blob.mu.Lock()
	defer entry.blob.mu.Unlock()

	entry.blob.refs--
	if entry.blob.refs > 0 {
		// There is another reference to this blob.
		return nil
	}

	s.g.bs.Delete(d)

	return nil
}

type memBlob struct {
	mu   sync.Mutex
	data []byte
	refs uint
}

func (b *memBlob) Inc() {
	b.mu.Lock()
	b.refs++
	b.mu.Unlock()
}

// memEntry is a per-repo record that points to a global blob and carries its own labels.
type memEntry struct {
	blob   *memBlob
	labels atomic.Pointer[Labels]
	added  atomic.Int64 // Unix nanoseconds of the last publish, link, or label update.
}

type nopCloser struct{ *bytes.Reader }

func (nopCloser) Close() error {
	return nil
}

var _ Linker = (*MemStore)(nil)

// Link shares the source's immutable blob while creating independent labels.
func (s *MemStore) Link(ctx context.Context, d Digest, from Store) (Meta, error) {
	if err := ctx.Err(); err != nil {
		return Meta{}, err
	}
	d, err := d.Sanitize()
	if err != nil {
		return Meta{}, err
	}
	m := Meta{Digest: d}
	source, ok := unwrapLinkSource(from).(*MemStore)
	if !ok || source == nil || source.g != s.g {
		return m, ErrIncompatibleStore
	}
	v, ok := source.es.Load(d)
	if !ok {
		return m, ErrNotExist
	}
	entry := v.(*memEntry)
	b := entry.blob
	b.mu.Lock()
	defer b.mu.Unlock()
	// Erase removes the entry before taking this lock. Recheck it so a removed
	// source cannot resurrect an unreferenced blob or corrupt reference counts.
	if current, ok := source.es.Load(d); !ok || current != entry {
		return m, ErrNotExist
	}
	if _, ok := s.es.Load(d); ok {
		return m, ErrAlreadyExists
	}
	m.Size = int64(len(b.data))
	if labels := entry.labels.Load(); labels != nil {
		m.Labels = cloneLabels(*labels)
	}
	e := &memEntry{blob: b}
	e.added.Store(time.Now().UnixNano())
	if m.Labels != nil {
		labels := cloneLabels(m.Labels)
		e.labels.Store(&labels)
	}
	if err := ctx.Err(); err != nil {
		return m, err
	}
	if _, loaded := s.es.LoadOrStore(d, e); loaded {
		return Meta{Digest: d}, ErrAlreadyExists
	}
	b.refs++
	return m, nil
}

var _ Walker = (*MemStore)(nil)
var _ Namespacer = (*MemStores)(nil)

func (s *MemStore) Walk(ctx context.Context) iter.Seq2[Info, error] {
	return func(yield func(Info, error) bool) {
		if err := ctx.Err(); err != nil {
			yield(nil, err)
			return
		}
		s.es.Range(func(key, value any) bool {
			if err := ctx.Err(); err != nil {
				yield(nil, err)
				return false
			}
			entry := value.(*memEntry)
			info := NewInfo(key.(Digest), int64(len(entry.blob.data)), time.Unix(0, entry.added.Load()), func(ctx context.Context) (Labels, error) {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				ls := entry.labels.Load()
				if ls == nil {
					return nil, nil
				}
				return *ls, nil
			})
			if !yield(info, nil) {
				return false
			}
			if err := ctx.Err(); err != nil {
				yield(nil, err)
				return false
			}
			return true
		})
	}
}

func (s *MemStores) Namespaces(ctx context.Context) iter.Seq2[string, error] {
	return func(yield func(string, error) bool) {
		if err := ctx.Err(); err != nil {
			yield("", err)
			return
		}
		s.ss.Range(func(key, value any) bool {
			if err := ctx.Err(); err != nil {
				yield("", err)
				return false
			}
			nonempty := false
			value.(*MemStore).es.Range(func(_, _ any) bool { nonempty = true; return false })
			if nonempty && !yield(key.(string), nil) {
				return false
			}
			if err := ctx.Err(); err != nil {
				yield("", err)
				return false
			}
			return true
		})
	}
}

var _ Stager = (*MemStore)(nil)
var _ StageCleaner = (*MemStores)(nil)

type memStageEntry struct {
	lock   chan struct{}
	record stageRecord
	data   []byte
}
type memStage struct {
	store *MemStore
	id    string
}

func (s *MemStore) Begin(ctx context.Context, algo digest.Algorithm) (Stage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r, err := newStageRecord(s.id, algo, s.g.stage)
	if err != nil {
		return nil, err
	}
	e := &memStageEntry{lock: make(chan struct{}, 1), record: r}
	if _, exists := s.g.stages.LoadOrStore(r.ID, e); exists {
		return nil, ErrStageConflict
	}
	return &memStage{store: s, id: r.ID}, nil
}
func (s *MemStore) Resume(ctx context.Context, id string) (Stage, error) {
	h := &memStage{store: s, id: id}
	if _, err := h.Stat(ctx); err != nil {
		return nil, err
	}
	return h, nil
}
func (s *memStage) ID() string { return s.id }
func (s *memStage) acquire(ctx context.Context, allowExpired ...bool) (*memStageEntry, func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if !validStageID(s.id) {
		return nil, nil, ErrNotExist
	}
	value, ok := s.store.g.stages.Load(s.id)
	if !ok {
		return nil, nil, ErrNotExist
	}
	e := value.(*memStageEntry)
	select {
	case e.lock <- struct{}{}:
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
	release := func() { <-e.lock }
	value, ok = s.store.g.stages.Load(s.id)
	if !ok || value != e || e.record.Namespace != namespaceSegment(s.store.id) {
		release()
		return nil, nil, ErrNotExist
	}
	if err := ctx.Err(); err != nil {
		release()
		return nil, nil, err
	}
	if err := e.record.validate(); err != nil {
		release()
		return nil, nil, err
	}
	if e.record.State != StageCommitting && e.record.expired(s.store.g.stage.clock()) && !(len(allowExpired) > 0 && allowExpired[0]) {
		release()
		return nil, nil, ErrStageExpired
	}
	return e, release, nil
}
func (s *memStage) Stat(ctx context.Context) (StageInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, s.store.g.stage.normalized().OperationTimeout)
	defer cancel()
	e, release, err := s.acquire(ctx)
	if err != nil {
		return StageInfo{}, err
	}
	defer release()
	return e.record.info(), nil
}
func (s *memStage) Append(ctx context.Context, offset int64, r io.Reader) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, s.store.g.stage.normalized().OperationTimeout)
	defer cancel()
	e, release, err := s.acquire(ctx)
	if err != nil {
		return 0, err
	}
	defer release()
	current := e.record.Offset
	if e.record.State != StageActive {
		return current, ErrStageClosed
	}
	if offset != current {
		return current, ErrOffsetMismatch
	}
	hash, err := stageHashRestore(e.record.Algorithm, e.record.Hash)
	if err != nil {
		return current, err
	}
	var incoming bytes.Buffer
	if _, err := io.Copy(io.MultiWriter(&incoming, hash), stageContextReader{ctx, r}); err != nil {
		return current, err
	}
	if err := ctx.Err(); err != nil {
		return current, err
	}
	checkpoint, err := stageHashState(hash)
	if err != nil {
		return current, err
	}
	e.data = append(e.data, incoming.Bytes()...)
	e.record.Offset = int64(len(e.data))
	e.record.Hash = checkpoint
	e.record.ExpiresAt = s.store.g.stage.clock().Add(s.store.g.stage.normalized().TTL)
	return e.record.Offset, nil
}
func (s *memStage) Commit(ctx context.Context, m Meta) (Meta, error) {
	ctx, cancel := context.WithTimeout(ctx, s.store.g.stage.normalized().OperationTimeout)
	defer cancel()
	e, release, err := s.acquire(ctx)
	if err != nil {
		return Meta{}, err
	}
	defer release()
	if e.record.State == StageAborted {
		return Meta{}, ErrStageClosed
	}
	prepared, err := e.record.prepareCommit(m)
	if err != nil {
		return Meta{}, err
	}
	switch e.record.State {
	case StageCommitted:
		if !stageCommitMatches(prepared, e.record.Commit) {
			return Meta{}, ErrStageConflict
		}
		return e.record.Result.Clone(), nil
	case StageCommitting:
		if !stageCommitMatches(prepared, e.record.Commit) {
			return Meta{}, ErrStageConflict
		}
	case StageActive:
		e.record.State = StageCommitting
		e.record.Commit = prepared
	}
	return s.complete(ctx, e)
}
func (s *memStage) complete(ctx context.Context, e *memStageEntry) (Meta, error) {
	if err := ctx.Err(); err != nil {
		return Meta{}, err
	}
	result, err := s.store.publish(e.record.Commit, e.data)
	if errors.Is(err, ErrAlreadyExists) {
		info, statErr := s.store.Stat(ctx, e.record.Commit.Digest)
		if statErr != nil {
			return Meta{}, statErr
		}
		result, err = infoMeta(ctx, info)
	}
	if err != nil {
		return Meta{}, err
	}
	e.record.Result = result.Clone()
	e.record.State = StageCommitted
	e.record.ExpiresAt = s.store.g.stage.clock().Add(s.store.g.stage.normalized().Retention)
	e.data = nil
	return result.Clone(), nil
}
func (s *memStage) Abort(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, s.store.g.stage.normalized().OperationTimeout)
	defer cancel()
	e, release, err := s.acquire(ctx, true)
	if errors.Is(err, ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer release()
	switch e.record.State {
	case StageCommitted, StageAborted:
		return nil
	case StageCommitting:
		_, err := s.complete(ctx, e)
		return err
	}
	e.record.State = StageAborted
	e.record.ExpiresAt = s.store.g.stage.clock().Add(s.store.g.stage.normalized().Retention)
	e.data = nil
	return nil
}
func (s *MemStores) PruneStages(ctx context.Context) (int, error) {
	removed := 0
	var result error
	s.stages.Range(func(key, value any) bool {
		if err := ctx.Err(); err != nil {
			result = err
			return false
		}
		e := value.(*memStageEntry)
		select {
		case e.lock <- struct{}{}:
		default:
			return true
		}
		defer func() { <-e.lock }()
		current, ok := s.stages.Load(key)
		if !ok || current != e {
			return true
		}
		if e.record.State == StageCommitting {
			namespace, err := namespaceID(e.record.Namespace)
			if err != nil {
				result = errors.Join(result, ErrStageFormat)
				return true
			}
			store := s.Use(namespace).(*MemStore)
			op, cancel := context.WithTimeout(ctx, s.stage.normalized().OperationTimeout)
			_, err = (&memStage{store: store, id: e.record.ID}).complete(op, e)
			cancel()
			if err != nil {
				result = errors.Join(result, err)
				return true
			}
		}
		if e.record.expired(s.stage.clock()) {
			s.stages.Delete(key)
			e.data = nil
			removed++
		}
		return true
	})
	if err := ctx.Err(); err != nil {
		return removed, errors.Join(result, err)
	}
	return removed, result
}
