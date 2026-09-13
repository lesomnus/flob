package flob

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

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
	m := Meta{Digest: d, Size: info.Size()}
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
	if err := s.moveStageToRepo(stage, destination); err != nil {
		return m, err
	}
	return m.Clone(), nil
}
