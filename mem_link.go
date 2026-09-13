package flob

import "context"

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
