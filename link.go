package flob

import "context"

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
