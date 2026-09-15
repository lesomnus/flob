package flob

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Info describes a blob with an eagerly available identity and lazily loaded size,
// addition time, and labels. Information from Stat and Open already knows the size,
// so Size does no I/O for it; information from a Walk may load the size on first
// use. Stat, Size, Added, and Labels do not provide a coherent snapshot: the blob
// may change or disappear between calls. The first successful load of each value
// is memoized; failed loads may be retried with a new context. Labels returns
// defensive copies.
type Info interface {
	Digest() Digest
	Size(context.Context) (int64, error)
	// Added reports when this store last created or changed its entry for the
	// digest, including label updates. Adding a digest the store already holds
	// does not change it. Precision depends on the backend. It returns
	// [errors.ErrUnsupported] when the store does not know the time.
	Added(context.Context) (time.Time, error)
	Labels(context.Context) (Labels, error)
}

// NewInfo constructs blob information with a known size and addition time for
// store implementations. A zero added time is reported as [errors.ErrUnsupported].
// It calls loader only when Labels is requested, using that call's context, and
// serializes loads. The first successful result (including nil labels) is cached.
// Failures are not cached. A nil loader represents a blob with no labels. Returned
// labels never share storage with the loader's result or another Labels call.
func NewInfo(d Digest, size int64, added time.Time, loader func(context.Context) (Labels, error)) Info {
	return &blobInfo{digest: d, size: size, added: added, loader: loader}
}

// NewLazyInfo constructs blob information whose size and addition time are also
// loaded on demand, for inventories that learn only the digest. The size and added
// loaders follow the caching rules of the labels loader described in [NewInfo]; a
// nil added loader, like a zero loaded time, is reported as [errors.ErrUnsupported].
// All loads are serialized with each other, so the loaders may share unsynchronized
// state.
func NewLazyInfo(d Digest, size func(context.Context) (int64, error), added func(context.Context) (time.Time, error), labels func(context.Context) (Labels, error)) Info {
	return &blobInfo{digest: d, sizeLoader: size, addedLoader: added, loader: labels}
}

type blobInfo struct {
	digest      Digest
	mu          sync.Mutex
	size        int64
	sizeLoader  func(context.Context) (int64, error) // nil once the size is known
	added       time.Time
	addedLoader func(context.Context) (time.Time, error) // nil once the time is known
	loader      func(context.Context) (Labels, error)
	loaded      bool
	labels      Labels
}

func (i *blobInfo) Digest() Digest { return i.digest }
func (i *blobInfo) Size(ctx context.Context) (int64, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.sizeLoader != nil {
		size, err := i.sizeLoader(ctx)
		if err != nil {
			return 0, err
		}
		i.size = size
		i.sizeLoader = nil
	}
	return i.size, nil
}
func (i *blobInfo) Added(ctx context.Context) (time.Time, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.addedLoader != nil {
		added, err := i.addedLoader(ctx)
		if err != nil {
			return time.Time{}, err
		}
		i.added = added
		i.addedLoader = nil
	}
	if i.added.IsZero() {
		return time.Time{}, errors.ErrUnsupported
	}
	return i.added, nil
}
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
	size, err := info.Size(ctx)
	if err != nil {
		return Meta{}, err
	}
	labels, err := info.Labels(ctx)
	if err != nil {
		return Meta{}, err
	}
	return Meta{Digest: info.Digest(), Size: size, Labels: labels}, nil
}
