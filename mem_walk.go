package flob

import (
	"context"
	"iter"
)

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
			info := NewInfo(key.(Digest), int64(len(entry.blob.data)), func(ctx context.Context) (Labels, error) {
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
