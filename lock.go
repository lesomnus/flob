package flat

import (
	"context"
)

type NamedLock interface {
	New(name string) (Locker, error)
}

type Locker interface {
	Lock(ctx context.Context) error
	TryLock(ctx context.Context) (bool, error)
	// Unlock releases the lock.
	// It should be called after Lock returns successfully.
	// The lock should be eventually released even if it returns an error,
	// so the caller should not retry Unlock if it returns an error.
	Unlock(ctx context.Context) error
}
