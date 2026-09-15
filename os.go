package flob

// /
// ├─ stage/
// │  └─ (random)/
// │      ├- blob
// │      └- labels
// ├─ locks/
// │  └─ xxxxx...
// ├─ share/
// │  └─ (algo)/
// │     └─ xx/
// │        └─ xx/
// │           └─ xxxx...
// └─ repos/
//    └─ (id)/
//       └─ (algo)/
//          └─ xx/
//             └─ xx/
//                └─ xxxx.../
//                   ├- blob
//                   └- labels

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"iter"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gofrs/flock"
	"github.com/opencontainers/go-digest"
)

var (
	_ Stores = OsStores{}
	_ Store  = OsStore{}
)

type OsStores struct {
	root  string
	lock  NamedLock
	stage StageConfig
}

func NewOsStores(root string, stage ...StageConfig) OsStores {
	var cfg StageConfig
	if len(stage) > 0 {
		cfg = stage[0]
	}
	return OsStores{root: root, lock: NewOsFileLocker(filepath.Join(root, "locks")), stage: cfg.normalized()}
}

func (i OsStores) Root() string {
	return i.root
}

func (i OsStores) Use(id string) Store {
	return OsStore{
		root:      i.root,
		repo:      filepath.Join(i.root, "repos", namespaceSegment(id)),
		lock:      i.lock,
		namespace: id,
		stage:     i.stage,
	}
}

type OsStore struct {
	root      string
	repo      string
	lock      NamedLock
	namespace string
	stage     StageConfig
}

func (s OsStore) Add(ctx context.Context, m Meta, r io.Reader) (Meta, error) {
	algo := Canonical
	if m.Digest != "" {
		// Digest is provided, so check if the blob already exists.
		d, err := m.Digest.Sanitize()
		if err != nil {
			return m, err
		}
		m.Digest = d
		algo = d.Algorithm()

		pb := s.pathToRepo(m.Digest, "blob")
		if err := s.checkDup(pb); err != nil {
			return m, err
		}

		// Blob with the given digest does not exist, so proceed to add it.
	}

	// Write to a temp file first: the digest has to be computed while writing,
	// and what is written cannot be given its name until it is known.
	//
	// Inside the store, not `os.TempDir()`. A scratch container has no /tmp at
	// all -- a robot pulling blobs into an `os` store failed on its first Add
	// with "create temp: no such file or directory", and nothing about that
	// says which directory it meant. Staging here also puts the file on the
	// same filesystem as the copy that follows it, which the store root being
	// on another device is a reason for rather than against.
	pt, err := s.ensureStagePath()
	if err != nil {
		return m, fmt.Errorf("ensure stage path: %w", err)
	}

	tf, err := os.CreateTemp(pt, "flob-*")
	if err != nil {
		return m, fmt.Errorf("create temp: %w", err)
	}

	tp := tf.Name()
	defer os.Remove(tp)
	defer tf.Close()

	h := algo.Hash()
	n, err := io.Copy(io.MultiWriter(tf, h), r)
	if err != nil {
		return m, fmt.Errorf("write temp blob: %w", err)
	}
	if _, err := tf.Seek(0, io.SeekStart); err != nil {
		return m, fmt.Errorf("seek temp blob: %w", err)
	}

	m.Size = n

	d := Digest(fmt.Sprintf("%s:%x", algo, h.Sum(nil)))
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
	ps, err := s.ensureStagePath()
	if err != nil {
		return m, fmt.Errorf("ensure stage path: %w", err)
	}

	ps, err = os.MkdirTemp(ps, "")
	if err != nil {
		return m, fmt.Errorf("mkdir temp at stage: %w", err)
	}

	ok := false
	defer func() {
		if ok {
			// The file is moved to the repo.
			return
		}
		os.RemoveAll(ps)
	}()

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
	if err := os.MkdirAll(filepath.Dir(pr), 0o755); err != nil {
		return m, fmt.Errorf("mkdir repo: %w", err)
	}
	if err := stampEntry(ps); err != nil {
		return m, err
	}
	if err := s.moveStageToRepo(ps, pr); err != nil {
		return m, err
	}

	ok = true
	return m, nil
}

// stampEntry records the publication time on a staged digest directory just before
// it is moved into a repo, for [Info.Added]. Its modification time otherwise dates
// from creating the entries inside it, which can precede a long copy.
func stampEntry(dir string) error {
	now := time.Now()
	if err := os.Chtimes(dir, now, now); err != nil {
		return fmt.Errorf("stamp entry: %w", err)
	}
	return nil
}

// moveStageToRepo atomically moves the fully-staged directory ps onto the repo digest
// directory pr.
//
// os.Rename onto an already-existing directory fails (EEXIST/ENOTEMPTY), so a leftover pr
// — from a crash mid-Erase (RemoveAll unlinks blob, then labels, then the dir) or from a
// raced concurrent Add/Erase/Label on the same digest — would otherwise make the user's Add
// fail permanently. Because we hold the per-digest blob lock and checkDup already confirmed
// pr has no valid blob, any pre-existing pr is orphan state we are entitled to replace: we
// remove it and retry so the Add always succeeds.
func (s OsStore) moveStageToRepo(ps, pr string) error {
	const attempts = 5
	var err error
	for range attempts {
		if err = os.Rename(ps, pr); err == nil {
			return nil
		}
		if _, statErr := os.Stat(pr); statErr != nil {
			// pr does not exist, so the failure is not a leftover-collision; do not retry.
			return fmt.Errorf("move from stage to repo: %w", err)
		}
		if rmErr := os.RemoveAll(pr); rmErr != nil {
			return fmt.Errorf("remove leftover repo dir: %w", rmErr)
		}
	}
	return fmt.Errorf("move from stage to repo after %d attempts: %w", attempts, err)
}

func (s OsStore) Stat(ctx context.Context, d Digest) (Info, error) {
	_, info, err := s.open(ctx, d)
	return info, err
}

func (s OsStore) Open(ctx context.Context, d Digest) (io.ReadSeekCloser, Info, error) {
	p, info, err := s.open(ctx, d)
	if err != nil {
		return nil, nil, err
	}
	f, err := os.Open(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// The file may be removed after the stat.
			err = ErrNotExist
		}
		return nil, info, fmt.Errorf("open blob: %w", err)
	}
	return f, info, nil
}

func (s OsStore) Label(ctx context.Context, d Digest, labels Labels) error {
	d, err := d.Sanitize()
	if err != nil {
		// An invalid digest cannot correspond to any stored blob.
		return ErrNotExist
	}

	// Check if the blob exists first to avoid unnecessary work. Existence is defined by the
	// presence of the `blob` file — the same criterion Stat/Open/open use — not by the repo
	// directory, so an orphan labels-only directory is treated as "not exist" consistently.
	p := s.pathToRepo(d, "blob")
	if _, err := os.Stat(p); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ErrNotExist
		}
		return fmt.Errorf("stat: %w", err)
	}

	// We don't need to acquire the lock here since rename is atomic.

	ps, err := s.ensureStagePath()
	if err != nil {
		return fmt.Errorf("ensure stage path: %w", err)
	}

	f, err := os.CreateTemp(ps, "")
	if err != nil {
		return fmt.Errorf("create temp at stage: %w", err)
	}

	ok := false
	defer func(p string) {
		if ok {
			// The file is moved to the repo.
			return
		}
		os.Remove(p)
	}(f.Name())

	defer f.Close()

	if err := writeLabels(f, labels); err != nil {
		return fmt.Errorf("write labels: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close labels: %w", err)
	}

	pl := s.pathToRepo(d, "labels")
	if err := os.Rename(f.Name(), pl); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ErrNotExist
		}
		return fmt.Errorf("move labels to repo: %w", err)
	}

	return nil
}

// Erase removes the blob entry for this store and best-effort cleans up the
// global blob.
//
// Consistency with concurrent [OsStore.Add] - Erase does not hold the blob lock
// during RemoveAll, so an Add that is simultaneously moving its staged directory
// into the repo may race. Two orderings are possible:
//
//	(a) Rename happens before RemoveAll: the entry is immediately erased, which is
//	    equivalent to the user deleting the blob right after uploading it
//	(b) RemoveAll happens before Rename: the entry is created after Erase returns,
//	    giving the appearance the blob was added after the deletion.
//
// In case (b) the global blob (pd) may also have been removed by [tryCleanup]
// before Rename completes, leaving the repo entry's hard link as the sole surviving
// inode — an orphan.
func (s OsStore) Erase(ctx context.Context, d Digest) error {
	d, err := d.Sanitize()
	if err != nil {
		// An invalid digest cannot correspond to any stored blob; treat as a no-op so the
		// user's Erase request always succeeds (Erase never reports "not exist" anyway).
		return nil
	}

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

	n, err := nlink(pb)
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
	n, err = nlink(pb)
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

func (s OsStore) open(_ context.Context, d Digest) (string, Info, error) {
	d, err := d.Sanitize()
	if err != nil {
		return "", nil, ErrNotExist
	}
	pb := s.pathToRepo(d, "blob")
	fi, err := os.Stat(pb)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil, ErrNotExist
		}
		return "", nil, fmt.Errorf("stat: %w", err)
	}
	size := fi.Size()
	info := NewLazyInfo(d, func(context.Context) (int64, error) { return size, nil }, s.added(d), func(ctx context.Context) (Labels, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		lf, err := os.Open(s.pathToRepo(d, "labels"))
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, nil
			}
			return nil, fmt.Errorf("open labels: %w", err)
		}
		defer lf.Close()
		labels, err := readLabels(lf)
		if err != nil {
			return nil, fmt.Errorf("read labels: %w", err)
		}
		return labels, nil
	})
	return pb, info, nil
}

// added reads the digest directory's modification time. Add, Link, and staged
// commits stamp it when publishing, and Label's rename of the labels file into it
// updates it.
func (s OsStore) added(d Digest) func(context.Context) (time.Time, error) {
	return func(ctx context.Context) (time.Time, error) {
		if err := ctx.Err(); err != nil {
			return time.Time{}, err
		}
		fi, err := os.Stat(s.pathToRepo(d))
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return time.Time{}, ErrNotExist
			}
			return time.Time{}, fmt.Errorf("stat entry: %w", err)
		}
		return fi.ModTime(), nil
	}
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
	v := d.Encoded()
	return filepath.Join(s.root, "share", d.Algorithm().String(), v[0:2], v[2:4], v[4:])
}

func (s OsStore) pathToRepo(d Digest, elem ...string) string {
	v := d.Encoded()
	parts := make([]string, 0, 5+len(elem))
	parts = append(parts, s.repo, d.Algorithm().String(), v[0:2], v[2:4], v[4:])
	parts = append(parts, elem...)
	return filepath.Join(parts...)
}

func (s OsStore) ensureStagePath() (string, error) {
	ps := filepath.Join(s.root, "stage")
	if err := os.MkdirAll(ps, 0o755); err != nil {
		return "", fmt.Errorf("mkdir stage: %w", err)
	}

	return ps, nil
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

func NewOsFileLocker(root string) OsFileLocker {
	return OsFileLocker{root: root}
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

var _ Linker = OsStore{}

// Link publishes a hard link to a blob held by another namespace under the same
// store root. It reads labels but never reads or hashes the blob's content.
func (s OsStore) Link(ctx context.Context, d Digest, from Store) (Meta, error) {
	if err := ctx.Err(); err != nil {
		return Meta{}, err
	}
	d, err := d.Sanitize()
	if err != nil {
		return Meta{}, err
	}
	var source OsStore
	switch from := unwrapLinkSource(from).(type) {
	case OsStore:
		source = from
	case *OsStore:
		if from == nil {
			return Meta{}, ErrIncompatibleStore
		}
		source = *from
	default:
		return Meta{}, ErrIncompatibleStore
	}
	root, err := filepath.Abs(s.root)
	if err != nil {
		return Meta{}, fmt.Errorf("resolve destination root: %w", err)
	}
	sourceRoot, err := filepath.Abs(source.root)
	if err != nil {
		return Meta{}, fmt.Errorf("resolve source root: %w", err)
	}
	if root != sourceRoot || s.lock == nil || source.lock == nil {
		return Meta{}, ErrIncompatibleStore
	}
	unlock, err := s.lockBlob(ctx, d)
	if err != nil {
		return Meta{}, err
	}
	defer unlock(ctx)

	info, err := source.Stat(ctx, d)
	if err != nil {
		return Meta{}, err
	}
	size, err := info.Size(ctx)
	if err != nil {
		return Meta{}, err
	}
	m := Meta{Digest: d, Size: size}
	if err := s.checkDup(s.pathToRepo(d, "blob")); err != nil {
		return m, err
	}
	m.Labels, err = info.Labels(ctx)
	if err != nil {
		return m, err
	}
	stageRoot, err := s.ensureStagePath()
	if err != nil {
		return m, fmt.Errorf("ensure stage path: %w", err)
	}
	stage, err := os.MkdirTemp(stageRoot, "link-*")
	if err != nil {
		return m, fmt.Errorf("create link stage: %w", err)
	}
	defer os.RemoveAll(stage)

	labels, err := os.Create(filepath.Join(stage, "labels"))
	if err != nil {
		return m, fmt.Errorf("create labels: %w", err)
	}
	if err := writeLabels(labels, m.Labels); err != nil {
		labels.Close()
		return m, fmt.Errorf("write labels: %w", err)
	}
	if err := labels.Close(); err != nil {
		return m, fmt.Errorf("close labels: %w", err)
	}
	// Pin the source inode before publishing. A concurrent Erase either removes
	// the source first (and Link fails), or leaves these bytes alive via stage.
	if err := os.Link(source.pathToRepo(d, "blob"), filepath.Join(stage, "blob")); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return m, ErrNotExist
		}
		return m, fmt.Errorf("link source blob: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return m, err
	}
	destination := s.pathToRepo(d)
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return m, fmt.Errorf("mkdir destination: %w", err)
	}
	if err := stampEntry(stage); err != nil {
		return m, err
	}
	if err := s.moveStageToRepo(stage, destination); err != nil {
		return m, err
	}
	return m.Clone(), nil
}

var (
	_ Walker     = OsStore{}
	_ Namespacer = OsStores{}
)

// Walk inventories regular blob files in this namespace from directory entries,
// without a stat per blob. Size and labels are loaded only if requested through
// the returned Info. Symlinks are not followed.
func (s OsStore) Walk(ctx context.Context) iter.Seq2[Info, error] {
	return func(yield func(Info, error) bool) {
		err := filepath.WalkDir(s.repo, func(path string, entry fs.DirEntry, err error) error {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					return nil
				}
				return err
			}
			rel, err := filepath.Rel(s.repo, path)
			if err != nil {
				return err
			}
			if rel == "." {
				return nil
			}
			parts := strings.Split(rel, string(filepath.Separator))
			if entry.IsDir() {
				if len(parts) > 4 {
					return fs.SkipDir
				}
				return nil
			}
			if len(parts) != 5 || parts[4] != "blob" || len(parts[1]) != 2 || len(parts[2]) != 2 {
				return nil
			}
			d := Digest(parts[0] + ":" + parts[1] + parts[2] + parts[3])
			if clean, err := d.Sanitize(); err != nil || clean != d {
				return nil
			}
			if !entry.Type().IsRegular() {
				return nil
			}
			info := NewLazyInfo(d, func(ctx context.Context) (int64, error) {
				current, err := s.Stat(ctx, d)
				if err != nil {
					return 0, err
				}
				return current.Size(ctx)
			}, s.added(d), func(ctx context.Context) (Labels, error) {
				current, err := s.Stat(ctx, d)
				if err != nil {
					return nil, err
				}
				return current.Labels(ctx)
			})
			if !yield(info, nil) {
				return fs.SkipAll
			}
			return ctx.Err()
		})
		if err != nil {
			yield(nil, err)
		}
	}
}

// Namespaces enumerates canonical namespace directories containing at least one
// valid blob. Merely calling Use or leaving an empty directory does not count.
func (s OsStores) Namespaces(ctx context.Context) iter.Seq2[string, error] {
	return func(yield func(string, error) bool) {
		if err := ctx.Err(); err != nil {
			yield("", err)
			return
		}
		dir, err := os.Open(filepath.Join(s.root, "repos"))
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				yield("", err)
			}
			return
		}
		defer dir.Close()
		for {
			if err := ctx.Err(); err != nil {
				yield("", err)
				return
			}
			entries, readErr := dir.ReadDir(128)
			for _, entry := range entries {
				if err := ctx.Err(); err != nil {
					yield("", err)
					return
				}
				if !entry.IsDir() {
					continue
				}
				id, err := namespaceID(entry.Name())
				if err != nil || namespaceSegment(id) != entry.Name() {
					continue
				}
				store := s.Use(id).(OsStore)
				found := false
				for _, err := range store.Walk(ctx) {
					if err != nil {
						yield("", err)
						return
					}
					found = true
					break
				}
				if found {
					if !yield(id, nil) {
						return
					}
					if err := ctx.Err(); err != nil {
						yield("", err)
						return
					}
				}
			}
			if readErr != nil {
				if !errors.Is(readErr, io.EOF) {
					yield("", readErr)
				}
				return
			}
		}
	}
}

var (
	_ Stager       = OsStore{}
	_ StageCleaner = OsStores{}
	_ Stage        = (*osStage)(nil)
)

// Begin creates a durable upload separate from Add's private temporary files.
// Upload data and SHA-2 checkpoints are synced before publishing each offset.
func (s OsStore) Begin(ctx context.Context, algo digest.Algorithm) (Stage, error) {
	cfg := s.stage.normalized()
	ctx, cancel := context.WithTimeout(ctx, cfg.OperationTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	record, err := newStageRecord(s.namespace, algo, cfg)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(s.root, 0o755); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(s.root)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	if err := osStageMkdir(root, "uploads", 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join("uploads", record.ID)
	if err := root.Mkdir(path, 0o700); err != nil {
		return nil, err
	}
	keep := false
	defer func() {
		if !keep {
			root.RemoveAll(path)
		}
	}()
	dir, err := root.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	for _, name := range []string{"lock", "blob"} {
		f, err := dir.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
		if err != nil {
			return nil, err
		}
		err = f.Sync()
		closeErr := f.Close()
		if err != nil {
			return nil, err
		}
		if closeErr != nil {
			return nil, closeErr
		}
	}
	lock := flock.New(filepath.Join(s.root, path, "lock"), flock.SetFlag(os.O_RDWR))
	if _, err := lock.TryLockContext(ctx, 10*time.Millisecond); err != nil {
		return nil, err
	}
	defer lock.Close()
	if _, err := osStageWriteRecord(ctx, dir, record); err != nil {
		return nil, err
	}
	if err := osStageSyncDir(root, "uploads"); err != nil {
		return nil, err
	}
	keep = true
	return &osStage{store: s, id: record.ID}, nil
}

func (s OsStore) Resume(ctx context.Context, id string) (Stage, error) {
	stage := &osStage{store: s, id: id}
	locked, err := stage.lock(ctx, false)
	if err != nil {
		return nil, err
	}
	defer locked.close()
	if err := locked.checkExpiry(); err != nil {
		return nil, err
	}
	if err := locked.recoverTail(); err != nil {
		return nil, err
	}
	return stage, nil
}

type osStage struct {
	store OsStore
	id    string
}

func (s *osStage) ID() string { return s.id }

type osLockedStage struct {
	root     *os.Root
	dir      *os.Root
	lockFile *flock.Flock
	record   stageRecord
	config   StageConfig
	cancel   context.CancelFunc
	ctx      context.Context
}

func (l *osLockedStage) close() { l.lockFile.Close(); l.dir.Close(); l.root.Close(); l.cancel() }
func (l *osLockedStage) checkExpiry() error {
	if l.record.State != StageCommitting && l.record.expired(l.config.clock()) {
		return ErrStageExpired
	}
	return nil
}
func (s *osStage) lock(ctx context.Context, try bool) (*osLockedStage, error) {
	return openOsStage(ctx, s.store.root, s.id, &s.store.namespace, s.store.stage, try)
}
func openOsStage(ctx context.Context, path, id string, namespace *string, cfg StageConfig, try bool) (*osLockedStage, error) {
	if !validStageID(id) {
		return nil, ErrStageFormat
	}
	cfg = cfg.normalized()
	ctx, cancel := context.WithTimeout(ctx, cfg.OperationTimeout)
	failed := true
	defer func() {
		if failed {
			cancel()
		}
	}()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, osStageError(err)
	}
	defer func() {
		if failed {
			root.Close()
		}
	}()
	for _, name := range []string{"uploads", filepath.Join("uploads", id)} {
		info, err := root.Lstat(name)
		if err != nil {
			return nil, osStageError(err)
		}
		if !info.IsDir() {
			return nil, ErrStageFormat
		}
	}
	dir, err := root.OpenRoot(filepath.Join("uploads", id))
	if err != nil {
		return nil, osStageError(err)
	}
	defer func() {
		if failed {
			dir.Close()
		}
	}()
	if err := osStageRegular(dir, "lock"); err != nil {
		return nil, err
	}
	lock := flock.New(filepath.Join(path, "uploads", id, "lock"), flock.SetFlag(os.O_RDWR))
	defer func() {
		if failed {
			lock.Close()
		}
	}()
	if try {
		acquired, err := lock.TryLock()
		if err != nil {
			return nil, osStageError(err)
		}
		if !acquired {
			return nil, ErrStageConflict
		}
	} else {
		if _, err := lock.TryLockContext(ctx, 10*time.Millisecond); err != nil {
			return nil, osStageError(err)
		}
	}
	record, err := osStageReadRecord(dir)
	if err != nil {
		return nil, err
	}
	if record.ID != id {
		return nil, ErrStageFormat
	}
	if namespace != nil && record.Namespace != namespaceSegment(*namespace) {
		return nil, ErrNotExist
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	failed = false
	return &osLockedStage{root: root, dir: dir, lockFile: lock, record: record, config: cfg, cancel: cancel, ctx: ctx}, nil
}
func osStageError(err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		return ErrNotExist
	}
	return err
}
func osStageRegular(root *os.Root, path string) error {
	info, err := root.Lstat(path)
	if err != nil {
		return osStageError(err)
	}
	if !info.Mode().IsRegular() {
		return ErrStageFormat
	}
	return nil
}
func osStageReadRecord(dir *os.Root) (stageRecord, error) {
	if err := osStageRegular(dir, "manifest"); err != nil {
		return stageRecord{}, err
	}
	f, err := dir.Open("manifest")
	if err != nil {
		return stageRecord{}, err
	}
	defer f.Close()
	const limit = 16 << 20
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return stageRecord{}, err
	}
	if len(data) > limit {
		return stageRecord{}, ErrStageFormat
	}
	var record stageRecord
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil {
		return stageRecord{}, fmt.Errorf("%w: manifest: %v", ErrStageFormat, err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return stageRecord{}, ErrStageFormat
	}
	if err := record.validate(); err != nil {
		return stageRecord{}, err
	}
	if record.State == StageCommitting || record.State == StageCommitted {
		normalized, err := record.prepareCommit(record.Commit)
		if err != nil || !stageCommitMatches(normalized, record.Commit) {
			return stageRecord{}, ErrStageFormat
		}
	}
	if record.State == StageCommitted && (record.Result.Digest != record.Commit.Digest || record.Result.Size != record.Offset) {
		return stageRecord{}, ErrStageFormat
	}
	return record, nil
}

// The bool reports whether rename published the new checkpoint. A subsequent
// directory-fsync failure has an uncertain outcome and must not roll bytes back
// underneath a checkpoint that may already be durable.
func osStageWriteRecord(ctx context.Context, dir *os.Root, record stageRecord) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	data, err := json.Marshal(record)
	if err != nil {
		return false, err
	}
	if len(data) > 16<<20 {
		return false, ErrStageFormat
	}
	if err := dir.Remove("manifest.next"); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	f, err := dir.OpenFile("manifest.next", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return false, err
	}
	defer dir.Remove("manifest.next")
	if _, err := f.Write(data); err != nil {
		f.Close()
		return false, err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return false, err
	}
	if err := f.Close(); err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := dir.Rename("manifest.next", "manifest"); err != nil {
		return false, err
	}
	return true, osStageSyncDir(dir, ".")
}
func osStageSyncDir(root *os.Root, path string) error {
	dir, err := root.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
func osStageMkdir(root *os.Root, path string, mode fs.FileMode) error {
	current := "."
	for _, component := range strings.Split(filepath.Clean(path), string(filepath.Separator)) {
		if component == "." {
			continue
		}
		if component == ".." || component == "" {
			return ErrStageFormat
		}
		parent := current
		current = filepath.Join(current, component)
		info, err := root.Lstat(current)
		if err == nil {
			if !info.IsDir() {
				return ErrStageFormat
			}
			continue
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if err := root.Mkdir(current, mode); err != nil && !errors.Is(err, fs.ErrExist) {
			return err
		}
		info, err = root.Lstat(current)
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return ErrStageFormat
		}
		if err := osStageSyncDir(root, parent); err != nil {
			return err
		}
	}
	return nil
}
func (l *osLockedStage) recoverTail() error {
	if err := l.ctx.Err(); err != nil {
		return err
	}
	if l.record.State == StageCommitted || l.record.State == StageAborted {
		return nil
	}
	if err := osStageRegular(l.dir, "blob"); err != nil {
		return err
	}
	f, err := l.dir.OpenFile("blob", os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if l.record.State == StageActive {
		links, err := nlink(filepath.Join(l.dir.Name(), "blob"))
		if err != nil {
			return err
		}
		if links != 1 {
			return ErrStageFormat
		}
	}
	if info.Size() < l.record.Offset {
		return ErrStageFormat
	}
	if info.Size() > l.record.Offset {
		if err := f.Truncate(l.record.Offset); err != nil {
			return err
		}
		return f.Sync()
	}
	return nil
}

func (s *osStage) Stat(ctx context.Context) (StageInfo, error) {
	locked, err := s.lock(ctx, false)
	if err != nil {
		return StageInfo{}, err
	}
	defer locked.close()
	if err := locked.checkExpiry(); err != nil {
		return StageInfo{}, err
	}
	if err := locked.recoverTail(); err != nil {
		return StageInfo{}, err
	}
	return locked.record.info(), nil
}
func (s *osStage) Append(ctx context.Context, expectedOffset int64, r io.Reader) (int64, error) {
	locked, err := s.lock(ctx, false)
	if err != nil {
		return 0, err
	}
	defer locked.close()
	record := locked.record
	if err := locked.checkExpiry(); err != nil {
		return record.Offset, err
	}
	if record.State != StageActive {
		return record.Offset, ErrStageClosed
	}
	if expectedOffset != record.Offset {
		return record.Offset, ErrOffsetMismatch
	}
	if err := locked.recoverTail(); err != nil {
		return record.Offset, err
	}
	hash, err := stageHashRestore(record.Algorithm, record.Hash)
	if err != nil {
		return record.Offset, err
	}
	file, err := locked.dir.OpenFile("blob", os.O_RDWR, 0)
	if err != nil {
		return record.Offset, err
	}
	defer file.Close()
	if _, err := file.Seek(record.Offset, io.SeekStart); err != nil {
		return record.Offset, err
	}
	rollback := func(err error) (int64, error) {
		truncateErr := file.Truncate(record.Offset)
		syncErr := file.Sync()
		return record.Offset, errors.Join(err, truncateErr, syncErr)
	}
	n, err := io.Copy(io.MultiWriter(file, hash), stageContextReader{ctx: locked.ctx, r: r})
	if err != nil {
		return rollback(err)
	}
	if err := locked.ctx.Err(); err != nil {
		return rollback(err)
	}
	if err := file.Sync(); err != nil {
		return rollback(err)
	}
	next := record
	next.Offset += n
	next.ExpiresAt = locked.config.clock().Add(locked.config.TTL)
	next.Hash, err = stageHashState(hash)
	if err != nil {
		return rollback(err)
	}
	published, err := osStageWriteRecord(locked.ctx, locked.dir, next)
	if err != nil && !published {
		return rollback(err)
	}
	return next.Offset, err
}
func (s *osStage) Commit(ctx context.Context, m Meta) (Meta, error) {
	locked, err := s.lock(ctx, false)
	if err != nil {
		return Meta{}, err
	}
	defer locked.close()
	if err := locked.checkExpiry(); err != nil {
		return Meta{}, err
	}
	if locked.record.State == StageAborted {
		return Meta{}, ErrStageClosed
	}
	prepared, err := locked.record.prepareCommit(m)
	if err != nil {
		return Meta{}, err
	}
	if locked.record.State == StageCommitting || locked.record.State == StageCommitted {
		if !stageCommitMatches(prepared, locked.record.Commit) {
			return Meta{}, ErrStageConflict
		}
		if locked.record.State == StageCommitted {
			return locked.record.Result.Clone(), nil
		}
	} else {
		if err := locked.recoverTail(); err != nil {
			return Meta{}, err
		}
		locked.record.State = StageCommitting
		locked.record.Commit = prepared
		if _, err := osStageWriteRecord(locked.ctx, locked.dir, locked.record); err != nil {
			return Meta{}, err
		}
	}
	return s.recoverCommit(locked)
}
func (s *osStage) recoverCommit(l *osLockedStage) (Meta, error) {
	if err := l.ctx.Err(); err != nil {
		return Meta{}, err
	}
	if err := l.recoverTail(); err != nil {
		return Meta{}, err
	}
	m := l.record.Commit
	unlock, err := s.store.lockBlob(l.ctx, m.Digest)
	if err != nil {
		return Meta{}, err
	}
	defer unlock(context.Background())
	destination, err := filepath.Rel(s.store.root, s.store.pathToRepo(m.Digest))
	if err != nil {
		return Meta{}, err
	}
	shared, err := filepath.Rel(s.store.root, s.store.pathToBlob(m.Digest))
	if err != nil {
		return Meta{}, err
	}
	if err := osStageMkdir(l.root, filepath.Dir(destination), 0o755); err != nil {
		return Meta{}, err
	}
	if info, err := l.root.Lstat(destination); err == nil {
		if !info.IsDir() {
			return Meta{}, ErrStageFormat
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return Meta{}, err
	}
	if err := osStageRegular(l.root, filepath.Join(destination, "blob")); err == nil {
		info, err := s.store.Stat(l.ctx, m.Digest)
		if err != nil {
			return Meta{}, err
		}
		size, err := info.Size(l.ctx)
		if err != nil {
			return Meta{}, err
		}
		if size != l.record.Offset {
			return Meta{}, ErrStageFormat
		}
		if err := osStageRegular(l.root, filepath.Join(destination, "labels")); err != nil {
			return Meta{}, err
		}
		m, err = infoMeta(l.ctx, info)
		if err != nil {
			return Meta{}, err
		}
	} else if !errors.Is(err, ErrNotExist) {
		return Meta{}, err
	} else {
		publish := filepath.Join("uploads", s.id, "publish")
		if err := l.root.RemoveAll(publish); err != nil {
			return Meta{}, err
		}
		if err := l.root.Mkdir(publish, 0o700); err != nil {
			return Meta{}, err
		}
		defer l.root.RemoveAll(publish)
		labels, err := l.root.OpenFile(filepath.Join(publish, "labels"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return Meta{}, err
		}
		if err := writeLabels(labels, m.Labels); err != nil {
			labels.Close()
			return Meta{}, err
		}
		if err := labels.Sync(); err != nil {
			labels.Close()
			return Meta{}, err
		}
		if err := labels.Close(); err != nil {
			return Meta{}, err
		}
		if err := osStageMkdir(l.root, filepath.Dir(shared), 0o755); err != nil {
			return Meta{}, err
		}
		source := filepath.Join("uploads", s.id, "blob")
		if err := osStageRegular(l.root, shared); err == nil {
			source = shared
		} else if !errors.Is(err, ErrNotExist) {
			return Meta{}, err
		}
		if err := l.root.Link(source, filepath.Join(publish, "blob")); err != nil {
			return Meta{}, err
		}
		if source != shared {
			if err := l.root.Link(source, shared); err != nil {
				return Meta{}, err
			}
			if err := osStageSyncDir(l.root, filepath.Dir(shared)); err != nil {
				return Meta{}, err
			}
		}
		if err := osStageSyncDir(l.root, publish); err != nil {
			return Meta{}, err
		}
		if err := l.ctx.Err(); err != nil {
			return Meta{}, err
		}
		// Only an orphan digest directory can remain: a valid blob was checked
		// under the same digest lock used by Add and Link.
		if info, err := l.root.Lstat(destination); err == nil {
			if !info.IsDir() {
				return Meta{}, ErrStageFormat
			}
			if err := l.root.RemoveAll(destination); err != nil {
				return Meta{}, err
			}
		} else if !errors.Is(err, fs.ErrNotExist) {
			return Meta{}, err
		}
		// Stamp the publication time for Info.Added; see stampEntry.
		now := time.Now()
		if err := l.root.Chtimes(publish, now, now); err != nil {
			return Meta{}, err
		}
		if err := l.root.Rename(publish, destination); err != nil {
			return Meta{}, err
		}
	}
	for _, name := range []string{"blob", "labels"} {
		file, err := l.root.Open(filepath.Join(destination, name))
		if err != nil {
			return Meta{}, err
		}
		err = file.Sync()
		closeErr := file.Close()
		if err != nil {
			return Meta{}, err
		}
		if closeErr != nil {
			return Meta{}, closeErr
		}
	}
	if err := osStageSyncDir(l.root, destination); err != nil {
		return Meta{}, err
	}
	if err := osStageSyncDir(l.root, filepath.Dir(destination)); err != nil {
		return Meta{}, err
	}
	l.record.State = StageCommitted
	l.record.Result = m.Clone()
	l.record.ExpiresAt = l.config.clock().Add(l.config.Retention)
	if _, err := osStageWriteRecord(l.ctx, l.dir, l.record); err != nil {
		return Meta{}, err
	}
	// The receipt, not this private data link, is needed for idempotent retries.
	if err := l.dir.Remove("blob"); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return Meta{}, err
	}
	if err := osStageSyncDir(l.dir, "."); err != nil {
		return Meta{}, err
	}
	return m.Clone(), nil
}
func (s *osStage) Abort(ctx context.Context) error {
	locked, err := s.lock(ctx, false)
	if errors.Is(err, ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer locked.close()
	if locked.record.State == StageCommitting {
		_, err := s.recoverCommit(locked)
		return err
	}
	if locked.record.State == StageCommitted || locked.record.State == StageAborted {
		if err := locked.dir.Remove("blob"); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return osStageSyncDir(locked.dir, ".")
	}
	locked.record.State = StageAborted
	locked.record.ExpiresAt = locked.config.clock().Add(locked.config.Retention)
	if _, err := osStageWriteRecord(locked.ctx, locked.dir, locked.record); err != nil {
		return err
	}
	if err := locked.dir.Remove("blob"); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return osStageSyncDir(locked.dir, ".")
}

// PruneStages skips locked uploads, recovers interrupted commits, and removes
// expired records and their private files. It never removes published blobs.
func (s OsStores) PruneStages(ctx context.Context) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	root, err := os.OpenRoot(s.root)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	defer root.Close()
	info, err := root.Lstat("uploads")
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if !info.IsDir() {
		return 0, ErrStageFormat
	}
	dir, err := root.Open("uploads")
	if err != nil {
		return 0, err
	}
	defer dir.Close()
	entries, err := dir.ReadDir(-1)
	if err != nil {
		return 0, err
	}
	removed := 0
	var failures []error
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return removed, errors.Join(append(failures, err)...)
		}
		if !validStageID(entry.Name()) {
			continue
		}
		if !entry.IsDir() {
			failures = append(failures, ErrStageFormat)
			continue
		}
		locked, err := openOsStage(ctx, s.root, entry.Name(), nil, s.stage, true)
		if errors.Is(err, ErrStageConflict) {
			continue
		}
		if errors.Is(err, ErrNotExist) {
			pruned, orphanErr := s.pruneOrphanStage(ctx, root, entry.Name())
			if pruned {
				removed++
			}
			if orphanErr != nil {
				failures = append(failures, orphanErr)
			}
			continue
		}
		if err != nil {
			failures = append(failures, err)
			continue
		}
		if locked.record.State == StageCommitting {
			namespace, _ := namespaceID(locked.record.Namespace) // Validated when opening the manifest.
			stage := &osStage{store: s.Use(namespace).(OsStore), id: entry.Name()}
			_, err = stage.recoverCommit(locked)
		}
		if err == nil && locked.record.expired(locked.config.clock()) {
			err = root.RemoveAll(filepath.Join("uploads", entry.Name()))
			if err == nil {
				removed++
				err = osStageSyncDir(root, "uploads")
			}
		} else if err == nil && locked.record.State != StageCommitting {
			err = locked.recoverTail()
			if err == nil {
				err = locked.dir.RemoveAll("publish")
			}
			names := []string{"manifest.next"}
			if locked.record.State == StageCommitted || locked.record.State == StageAborted {
				names = append(names, "blob")
			}
			for _, name := range names {
				if err != nil {
					break
				}
				if removeErr := locked.dir.Remove(name); removeErr != nil && !errors.Is(removeErr, fs.ErrNotExist) {
					err = removeErr
				}
			}
			if err == nil {
				err = osStageSyncDir(locked.dir, ".")
			}
		}
		locked.close()
		if err != nil {
			failures = append(failures, err)
		}
	}
	return removed, errors.Join(failures...)
}

// A crash before the first manifest leaves no expiry record. Only directories
// beyond both the idle TTL and maximum operation duration are candidates, and
// the stage lock plus a second manifest check protect concurrent Begin calls.
func (s OsStores) pruneOrphanStage(ctx context.Context, root *os.Root, id string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	path := filepath.Join("uploads", id)
	info, err := root.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.IsDir() {
		return false, ErrStageFormat
	}
	cfg := s.stage.normalized()
	if cfg.clock().Before(info.ModTime().Add(cfg.TTL).Add(cfg.OperationTimeout)) {
		return false, nil
	}
	dir, err := root.OpenRoot(path)
	if err != nil {
		return false, err
	}
	defer dir.Close()
	if _, err := dir.Lstat("manifest"); err == nil {
		return false, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	if err := osStageRegular(dir, "lock"); errors.Is(err, ErrNotExist) {
		file, err := dir.OpenFile("lock", os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
		if errors.Is(err, fs.ErrExist) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		file.Close()
	} else if err != nil {
		return false, err
	}
	lock := flock.New(filepath.Join(s.root, path, "lock"), flock.SetFlag(os.O_RDWR))
	defer lock.Close()
	acquired, err := lock.TryLock()
	if err != nil {
		return false, err
	}
	if !acquired {
		return false, nil
	}
	if _, err := dir.Lstat("manifest"); err == nil {
		return false, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := root.RemoveAll(path); err != nil {
		return false, err
	}
	return true, osStageSyncDir(root, "uploads")
}
