package flob

import (
	"context"
	"sync"
)

// Info describes a blob with eagerly available identity and size and lazily
// loaded labels. Stat and Labels do not provide a coherent snapshot: labels may
// change between the two calls. The first successful label load is memoized;
// failed loads may be retried with a new context. Labels returns defensive copies.
type Info interface {
	Digest() Digest
	Size() int64
	Labels(context.Context) (Labels, error)
}

// NewInfo constructs blob information for store implementations. It calls loader
// only when Labels is requested, using that call's context, and serializes loads.
// The first successful result (including nil labels) is cached. Failures are not
// cached. A nil loader represents a blob with no labels. Returned labels never
// share storage with the loader's result or another Labels call.
func NewInfo(d Digest, size int64, loader func(context.Context) (Labels, error)) Info {
	return &blobInfo{digest: d, size: size, loader: loader}
}

type blobInfo struct {
	digest Digest
	size   int64
	mu     sync.Mutex
	loader func(context.Context) (Labels, error)
	loaded bool
	labels Labels
}

func (i *blobInfo) Digest() Digest { return i.digest }
func (i *blobInfo) Size() int64    { return i.size }
func (i *blobInfo) Labels(ctx context.Context) (Labels, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if !i.loaded {
		if i.loader != nil {
			labels, err := i.loader(ctx)
			if err != nil {
				return nil, err
			}
			i.labels = cloneLabels(labels)
		}
		i.loaded = true
		i.loader = nil
	}
	return cloneLabels(i.labels), nil
}

func infoMeta(ctx context.Context, info Info) (Meta, error) {
	labels, err := info.Labels(ctx)
	if err != nil {
		return Meta{}, err
	}
	return Meta{Digest: info.Digest(), Size: info.Size(), Labels: labels}, nil
}
