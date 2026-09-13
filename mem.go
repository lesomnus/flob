package flob

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
)

var (
	_ Stores = (*MemStores)(nil)
	_ Store  = (*MemStore)(nil)
)

// MemStores is an in-memory [Stores] implementation.
// All stores share a single global namespace: blobs with the same digest reuse
// the same underlying [memBlob] and are reference-counted under a mutex, so no
// GC sweep is ever required.
type MemStores struct {
	mu sync.Mutex
	bs sync.Map // map[Digest]*memBlob
	ss sync.Map // map[string]*MemStore
}

func NewMemStores() *MemStores {
	return &MemStores{}
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
	info := NewInfo(d, int64(len(entry.blob.data)), func(ctx context.Context) (Labels, error) {
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
}

type nopCloser struct{ *bytes.Reader }

func (nopCloser) Close() error {
	return nil
}
