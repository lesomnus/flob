package flob

import (
	"context"
	"iter"
)

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
