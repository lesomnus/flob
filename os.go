package flat

// /
// ├─ stage/
// │  └─ (random)/
// │      ├- blob
// │      └- labels
// ├─ locks/
// │  └─ xxxxx...
// ├─ blobs/
// │  └─ xx/
// │     └─ xx/
// │        └─ xxxx...
// └─ index/
//    └─ (id)/
//       └─ xx/
//          └─ xx/
//             └─ xxxx.../
//                ├- blob
//                └- labels

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/gofrs/flock"
)

var (
	_ Stores = OsStores{}
	_ Store  = OsStore{}
)

type OsStores struct {
	root string
	lock NamedLock
}

func NewOsStores(root string) OsStores {
	return OsStores{root, OsFileLocker{filepath.Join(root, "locks")}}
}

func (i OsStores) Root() string {
	return i.root
}

func (i OsStores) Use(id string) Store {
	return OsStore{
		root: i.root,
		repo: filepath.Join(i.root, "index", id),
		lock: i.lock,
	}
}

type OsStore struct {
	root string
	repo string
	lock NamedLock
}

func (s OsStore) Add(ctx context.Context, m Meta, r io.Reader) (Meta, error) {
	if m.Digest != "" {
		// Digest is provided, so check if the blob already exists.
		d, err := m.Digest.Sanitize()
		if err != nil {
			return m, err
		}
		m.Digest = d

		pb := s.pathToRepo(m.Digest, "blob")
		if err := s.checkDup(pb); err != nil {
			return m, err
		}

		// Blob with the given digest does not exist, so proceed to add it.
	}

	// Write to a local temp file first, not to the store root since the store root
	// may be on a different device and we need to compute the digest while writing.
	tf, err := os.CreateTemp("", "flat-*")
	if err != nil {
		return m, fmt.Errorf("create temp: %w", err)
	}

	tp := tf.Name()
	defer os.Remove(tp)
	defer tf.Close()

	h := Hash()
	n, err := io.Copy(io.MultiWriter(tf, h), r)
	if err != nil {
		return m, fmt.Errorf("write temp blob: %w", err)
	}
	if _, err := tf.Seek(0, io.SeekStart); err != nil {
		return m, fmt.Errorf("seek temp blob: %w", err)
	}

	m.Size = n

	d := Digest(fmt.Sprintf("%x", h.Sum(nil)))
	pb := s.pathToRepo(d, "blob")
	if m.Digest == "" {
		m.Digest = d
		// Now we have the digest, so check if the blob already exists to avoid
		// unnecessary work.
		if err := s.checkDup(pb); err != nil {
			return m, err
		}
	} else if m.Digest != d {
		return m, ErrDigestMismatch
	}

	// We decided to add the blob, so acquire the lock to prevent concurrent Add
	// or Erase with the same digest.
	unlock, err := s.lockBlob(ctx, d)
	if err != nil {
		return m, err
	}
	defer unlock(ctx)

	// Maybe another process added the blob while we were waiting for the lock, so
	// check again.
	if err := s.checkDup(pb); err != nil {
		return m, err
	}

	// We are the only one adding the blob with the given digest, so stage the blob.
	ps := filepath.Join(s.root, "stage")
	if err := os.MkdirAll(ps, 0o755); err != nil {
		return m, fmt.Errorf("mkdir stage: %w", err)
	}

	ps, err = os.MkdirTemp(ps, "")
	if err != nil {
		return m, fmt.Errorf("mkdir temp at stage: %w", err)
	}
	defer os.RemoveAll(ps)

	// Write labels first.
	lf, err := os.Create(filepath.Join(ps, "labels"))
	if err != nil {
		return m, fmt.Errorf("create labels: %w", err)
	}
	if err := writeLabels(lf, m.Labels); err != nil {
		lf.Close()
		return m, fmt.Errorf("write labels: %w", err)
	}
	if err := lf.Close(); err != nil {
		return m, fmt.Errorf("close labels: %w", err)
	}

	pd := s.pathToBlob(d)
	if _, err := os.Stat(pd); err == nil {
		// There is already a blob with the same digest, so we can just make a hard link
		// to the destination path.
		if err := os.Link(pd, filepath.Join(ps, "blob")); err != nil {
			return m, fmt.Errorf("link blob: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return m, fmt.Errorf("stat blob: %w", err)
	} else {
		// No blob in the global namespace, so copy the temp file to staging area.
		psb := filepath.Join(ps, "blob")
		bf, err := os.Create(psb)
		if err != nil {
			return m, fmt.Errorf("create blob: %w", err)
		}
		defer bf.Close()

		if _, err := io.Copy(bf, tf); err != nil {
			return m, fmt.Errorf("copy blob: %w", err)
		}

		// Make a hard link to the global blob path.
		if err := os.MkdirAll(filepath.Dir(pd), 0o755); err != nil {
			return m, fmt.Errorf("mkdir blob dir: %w", err)
		}
		if err := os.Link(psb, pd); err != nil {
			return m, fmt.Errorf("link blob to global path: %w", err)
		}
	}

	// Now the blob is staged, so move it to the destination path atomically.
	pr := filepath.Dir(pb)
	if err := os.MkdirAll(pr, 0o755); err != nil {
		return m, fmt.Errorf("mkdir repo: %w", err)
	}
	if err := os.Rename(ps, pr); err != nil {
		return m, fmt.Errorf("move from stage to repo: %w", err)
	}

	return m, nil
}

func (s OsStore) Get(ctx context.Context, d Digest) (m Meta, err error) {
	_, m, err = s.open(ctx, d)
	return
}

func (s OsStore) Open(ctx context.Context, d Digest) (io.ReadSeekCloser, Meta, error) {
	p, m, err := s.open(ctx, d)
	if err != nil {
		return nil, m, err
	}

	f, err := os.Open(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// The file may be removed after the stat.
			err = ErrNotExist
		}
		return nil, m, fmt.Errorf("open blob: %w", err)
	}

	return f, m, nil
}

func (s OsStore) Label(ctx context.Context, d Digest, labels Labels) error {
	return nil
}

func (s OsStore) Erase(ctx context.Context, d Digest) error {
	pr := s.pathToRepo(d)
	if err := os.RemoveAll(pr); err != nil && !os.IsNotExist(err) {
		return err
	}

	// Do not return error since the cleanup is best-effort and the blob
	// for the tenant already removed so Erase itself is successful.
	s.tryCleanup(ctx, d)

	return nil
}

// Check if the blob was the last one, and if so, remove the global blob.
func (s OsStore) tryCleanup(ctx context.Context, d Digest) (bool, error) {
	pb := s.pathToBlob(d)
	info, err := os.Stat(pb)
	if err != nil {
		return false, fmt.Errorf("stat blob: %w", err)
	}

	n, err := nlink(info.Sys())
	if err != nil {
		return false, fmt.Errorf("nlink: %w", err)
	}
	if n > 1 {
		// There is another link to the blob.
		return false, nil
	}

	unlock, err := s.lockBlob(ctx, d)
	if err != nil {
		return false, fmt.Errorf("lock blob: %w", err)
	}
	defer unlock(ctx)

	// Check nlink again after acquiring the lock to make sure there is still only one link.
	info, err = os.Stat(pb)
	if err != nil {
		return false, fmt.Errorf("stat blob: %w", err)
	}

	n, err = nlink(info.Sys())
	if err != nil {
		return false, fmt.Errorf("nlink: %w", err)
	}
	if n > 1 {
		// There is another link to the blob.
		return false, nil
	}

	// Do.
	if err := os.Remove(pb); err != nil && !os.IsNotExist(err) {
		return false, fmt.Errorf("remove blob: %w", err)
	}

	return true, nil
}

func (s OsStore) open(_ context.Context, d Digest) (pb string, m Meta, err error) {
	pb = s.pathToRepo(d, "blob")
	pl := s.pathToRepo(d, "labels")

	info, err := os.Stat(pb)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			err = ErrNotExist
		} else {
			err = fmt.Errorf("stat: %w", err)
		}
		return
	}

	m.Digest = d
	m.Size = info.Size()

	var lf *os.File
	if lf, err = os.Open(pl); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			err = fmt.Errorf("open labels: %w", err)
			return
		}
	} else {
		defer lf.Close()

		m.Labels, err = readLabels(lf)
		if err != nil {
			err = fmt.Errorf("read labels: %w", err)
			return
		}
	}

	return
}

// checkDup checks if the blob with the given path already exists.
// It returns [ErrAlreadyExists] if it exists.
func (s OsStore) checkDup(p string) error {
	if _, err := os.Stat(p); err == nil {
		return ErrAlreadyExists
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat: %w", err)
	}
	return nil
}

func (s OsStore) pathToBlob(d Digest) string {
	v := string(d)
	return filepath.Join(s.root, "blobs", v[0:2], v[2:4], v[4:])
}

func (s OsStore) pathToRepo(d Digest, elem ...string) string {
	v := string(d)
	parts := make([]string, 0, 4+len(elem))
	parts = append(parts, s.repo, v[0:2], v[2:4], v[4:])
	parts = append(parts, elem...)
	return filepath.Join(parts...)
}

func (s OsStore) lockBlob(ctx context.Context, d Digest) (func(ctx context.Context) error, error) {
	lock, err := s.lock.New(string(d))
	if err != nil {
		return nil, fmt.Errorf("create lock: %w", err)
	}
	if err := lock.Lock(ctx); err != nil {
		return nil, fmt.Errorf("lock: %w", err)
	}

	return lock.Unlock, nil
}

var _ NamedLock = OsFileLocker{}

type OsFileLocker struct {
	root string // "/locks"
}

func (l OsFileLocker) New(name string) (Locker, error) {
	p := filepath.Join(l.root, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir locks: %w", err)
	}

	return OsFileLock{flock.New(p)}, nil
}

var _ Locker = OsFileLock{}

type OsFileLock struct {
	lock *flock.Flock
}

func (l OsFileLock) Lock(ctx context.Context) error {
	_, err := l.lock.TryLockContext(ctx, 100*time.Millisecond)
	return err
}

func (l OsFileLock) TryLock(ctx context.Context) (bool, error) {
	return l.lock.TryLock()
}

func (l OsFileLock) Unlock(ctx context.Context) error {
	return l.lock.Unlock()
}
