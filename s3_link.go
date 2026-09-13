package flob

import (
	"context"
	"net/http"
	"strconv"
)

var _ Linker = (*S3Store)(nil)

// Link creates a reference to a blob visible in from without transferring its
// content. Both namespaces must belong to the same S3Stores instance.
func (s *S3Store) Link(ctx context.Context, d Digest, from Store) (Meta, error) {
	if err := ctx.Err(); err != nil {
		return Meta{}, err
	}
	d, err := d.Sanitize()
	if err != nil {
		return Meta{}, err
	}
	source, ok := unwrapLinkSource(from).(*S3Store)
	if !ok || source == nil || source.stores != s.stores {
		return Meta{}, ErrIncompatibleStore
	}
	// Check the source namespace, not merely the shared blob's existence.
	res, err := s.stores.head(ctx, s.stores.refKey(d, source.id))
	if err != nil {
		return Meta{}, err
	}
	labels, size := metaToLabels(res.Header)
	res.Body.Close()
	m := Meta{Digest: d, Labels: labels, Size: size}
	req, err := s.stores.newRequest(ctx, http.MethodPut, s.stores.refKey(d, s.id), nil, nil)
	if err != nil {
		return m, err
	}
	req.ContentLength = 0
	req.Header.Set("If-None-Match", "*")
	setLabelMeta(req.Header, labels)
	req.Header.Set(metaPrefix+metaSizeKey, strconv.FormatInt(size, 10))
	res, err = s.stores.send(req, emptyPayloadHash)
	if err != nil {
		return m, err
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusPreconditionFailed {
		return Meta{Digest: d}, ErrAlreadyExists
	}
	if res.StatusCode/100 != 2 {
		// A concurrent delete can cause S3 to return 409. Surface it so callers may
		// retry the entire Link, including the source visibility check.
		return m, statusError("link ref", res)
	}
	return m.Clone(), nil
}
