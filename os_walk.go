package flob

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"iter"
	"os"
	"path/filepath"
	"strings"
)

var (
	_ Walker     = OsStore{}
	_ Namespacer = OsStores{}
)

// Walk inventories regular blob files in this namespace. Labels are loaded
// only if requested through the returned Info. Symlinks are not followed.
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
			fi, err := entry.Info()
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					return nil
				}
				return err
			}
			if !fi.Mode().IsRegular() {
				return nil
			}
			info := NewInfo(d, fi.Size(), func(ctx context.Context) (Labels, error) {
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
