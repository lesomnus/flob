package flob

// S3-backed [Stores] that talks to any S3-compatible object store over plain
// HTTP (no AWS SDK dependency; requests are signed with SigV4, see sigv4.go).
//
// Layout inside a single bucket:
//
//	<prefix>blob/<algo>/<hex>            the one shared copy of each blob (dedup)
//	<prefix>refs/<algo>/<hex>/<store>   per-store reference marker; labels + size
//	                                    are kept in the marker's x-amz-meta-* headers
//
// A blob is content-addressed, so identical content from any store resolves to
// the same blob/ key and is uploaded once. Visibility is per store: a blob is
// observable from a store only if that store's refs/ marker exists, and every
// existence decision (Get/Open/Label/Add's dup check) reads the marker, never
// the shared blob/ object. Erase removes the marker and, when no marker remains
// for the digest, best-effort removes the shared blob.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

var (
	_ Stores    = (*S3Stores)(nil)
	_ Store     = (*S3Store)(nil)
	_ Presigner = (*S3Store)(nil)
)

// metaSizeKey is the reserved x-amz-meta suffix that records a blob's size on the
// reference marker (the marker body itself is empty). Labels whose name collides
// with this reserved suffix are not representable; see [S3Stores] docs.
const metaSizeKey = "Flob-Size"

const metaPrefix = "X-Amz-Meta-"

// S3Config configures an [S3Stores].
type S3Config struct {
	// Endpoint is the base URL of the S3 service, e.g. "https://s3.us-east-1.amazonaws.com"
	// or "http://localhost:9000" for MinIO. If empty, it defaults to the AWS
	// virtual-hosted endpoint derived from Region.
	Endpoint string
	// Region is the AWS region, e.g. "us-east-1". Required for signing; defaults
	// to "us-east-1" when empty.
	Region string
	// Bucket is the bucket that holds every store. Required.
	Bucket string
	// Prefix is an optional key prefix within the bucket, letting several flob
	// deployments share one bucket. A trailing "/" is added if missing.
	Prefix string
	// Credentials authenticate requests. Required for private buckets.
	Credentials Credentials
	// UsePathStyle selects path-style addressing (host/bucket/key) instead of
	// virtual-hosted style (bucket.host/key). Required for MinIO and most
	// S3-compatible servers.
	UsePathStyle bool
	// PublicEndpoint is the base URL clients use to reach the object store
	// directly for presigned downloads. It defaults to Endpoint; set it when the
	// server reaches S3 over an internal endpoint but clients need a different,
	// publicly reachable one (e.g. a CDN or public hostname).
	PublicEndpoint string
	// Client is the HTTP client used for all requests. Defaults to
	// [http.DefaultClient].
	Client *http.Client

	// now is injectable for deterministic tests; defaults to time.Now.
	now func() time.Time
}

// S3Stores is a content-addressable [Stores] backed by a single S3 bucket.
type S3Stores struct {
	cl        *http.Client
	signer    signer
	scheme    string
	host      string
	pubScheme string // scheme for presigned (client-facing) URLs
	pubHost   string // host for presigned (client-facing) URLs
	bucket    string
	prefix    string
	pathStyle bool
}

// NewS3Stores builds an [S3Stores] from cfg.
func NewS3Stores(cfg S3Config) (*S3Stores, error) {
	if cfg.Bucket == "" {
		return nil, errors.New("s3: bucket is required")
	}
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}

	scheme := "https"
	host := "s3." + cfg.Region + ".amazonaws.com"
	if cfg.Endpoint != "" {
		u, err := parseEndpoint(cfg.Endpoint)
		if err != nil {
			return nil, fmt.Errorf("s3: parse endpoint: %w", err)
		}
		scheme, host = u.Scheme, u.Host
	}

	pubScheme, pubHost := scheme, host
	if cfg.PublicEndpoint != "" {
		u, err := parseEndpoint(cfg.PublicEndpoint)
		if err != nil {
			return nil, fmt.Errorf("s3: parse public endpoint: %w", err)
		}
		pubScheme, pubHost = u.Scheme, u.Host
	}

	cl := cfg.Client
	if cl == nil {
		cl = http.DefaultClient
	}
	now := cfg.now
	if now == nil {
		now = time.Now
	}
	prefix := cfg.Prefix
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}

	return &S3Stores{
		cl:        cl,
		signer:    signer{creds: cfg.Credentials, region: cfg.Region, service: "s3", now: now},
		scheme:    scheme,
		host:      host,
		pubScheme: pubScheme,
		pubHost:   pubHost,
		bucket:    cfg.Bucket,
		prefix:    prefix,
		pathStyle: cfg.UsePathStyle,
	}, nil
}

// parseEndpoint parses an S3 endpoint URL, tolerating a bare "host:port" without
// a scheme by assuming https. This covers both "localhost:9000" (which url.Parse
// accepts with an empty Host) and "10.0.0.5:9000" (which url.Parse rejects because
// a numeric IP is not a valid scheme), so any scheme-less host:port works.
func parseEndpoint(s string) (*url.URL, error) {
	if u, err := url.Parse(s); err == nil && u.Host != "" {
		return u, nil
	}
	return url.Parse("https://" + s)
}

func (s *S3Stores) Use(id string) Store {
	return &S3Store{stores: s, id: id}
}

func (s *S3Stores) blobKey(d Digest) string {
	return s.prefix + "blob/" + d.Algorithm().String() + "/" + d.Encoded()
}

func (s *S3Stores) refKey(d Digest, id string) string {
	return s.prefix + "refs/" + d.Algorithm().String() + "/" + d.Encoded() + "/" + id
}

// presignGet builds a presigned GET URL for an in-bucket key, valid for ttl. It
// uses the public endpoint so the URL is reachable directly by clients, and it
// signs the exact host+path the client will request.
func (s *S3Stores) presignGet(key string, ttl time.Duration) string {
	scheme, host := s.pubScheme, s.pubHost
	rawPath := "/" + key
	if s.pathStyle {
		rawPath = "/" + s.bucket + "/" + key
	} else {
		host = s.bucket + "." + host
	}
	canonicalURI := awsURIEncode(rawPath, false)
	query := s.signer.presignQuery(http.MethodGet, canonicalURI, host, ttl)
	return scheme + "://" + host + canonicalURI + "?" + query
}

// newRequest builds an unsigned request for an in-bucket key ("" addresses the
// bucket itself, used for listing). It sets Opaque and RawQuery to their
// SigV4-encoded forms so the signed request line matches the wire exactly.
func (s *S3Stores) newRequest(ctx context.Context, method, key string, query url.Values, body io.Reader) (*http.Request, error) {
	rawPath := "/" + key
	host := s.host
	if s.pathStyle {
		rawPath = "/" + s.bucket + "/" + key
	} else {
		host = s.bucket + "." + s.host
	}

	req, err := http.NewRequestWithContext(ctx, method, s.scheme+"://"+host+"/", body)
	if err != nil {
		return nil, err
	}
	req.URL.Opaque = awsURIEncode(rawPath, false)
	if len(query) > 0 {
		req.URL.RawQuery = canonicalQuery(query)
	}
	return req, nil
}

// send signs and dispatches req.
func (s *S3Stores) send(req *http.Request, payloadHash string) (*http.Response, error) {
	s.signer.sign(req, payloadHash)
	return s.cl.Do(req)
}

// head issues a HEAD for key. It returns the response (caller closes Body) on
// 200, [ErrNotExist] on 404, and a descriptive error otherwise.
func (s *S3Stores) head(ctx context.Context, key string) (*http.Response, error) {
	req, err := s.newRequest(ctx, http.MethodHead, key, nil, nil)
	if err != nil {
		return nil, err
	}
	res, err := s.send(req, emptyPayloadHash)
	if err != nil {
		return nil, err
	}
	switch res.StatusCode {
	case http.StatusOK:
		return res, nil
	case http.StatusNotFound:
		res.Body.Close()
		return nil, ErrNotExist
	default:
		defer res.Body.Close()
		return nil, statusError("head", res)
	}
}

// exists reports whether key is present, mapping 404 to (false, nil).
func (s *S3Stores) exists(ctx context.Context, key string) (bool, error) {
	res, err := s.head(ctx, key)
	if err != nil {
		if errors.Is(err, ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	res.Body.Close()
	return true, nil
}

// putBlob uploads the shared blob. payloadHash is the blob's hex digest, which is
// exactly its SHA-256, so no re-hash is needed for signing.
func (s *S3Stores) putBlob(ctx context.Context, d Digest, body io.Reader, size int64) error {
	req, err := s.newRequest(ctx, http.MethodPut, s.blobKey(d), nil, body)
	if err != nil {
		return err
	}
	req.ContentLength = size
	res, err := s.send(req, d.Encoded())
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode/100 != 2 {
		return statusError("put blob", res)
	}
	return nil
}

// putRef writes the per-store reference marker with labels and size in its
// metadata. The marker overwrites any previous one, so it doubles as the Label
// operation.
func (s *S3Stores) putRef(ctx context.Context, d Digest, id string, labels Labels, size int64) error {
	req, err := s.newRequest(ctx, http.MethodPut, s.refKey(d, id), nil, nil)
	if err != nil {
		return err
	}
	req.ContentLength = 0
	setLabelMeta(req.Header, labels)
	req.Header.Set(metaPrefix+metaSizeKey, strconv.FormatInt(size, 10))

	res, err := s.send(req, emptyPayloadHash)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode/100 != 2 {
		return statusError("put ref", res)
	}
	return nil
}

// deleteKey removes key. A missing key is not an error.
func (s *S3Stores) deleteKey(ctx context.Context, key string) error {
	req, err := s.newRequest(ctx, http.MethodDelete, key, nil, nil)
	if err != nil {
		return err
	}
	res, err := s.send(req, emptyPayloadHash)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	switch res.StatusCode {
	case http.StatusOK, http.StatusNoContent, http.StatusNotFound:
		return nil
	default:
		return statusError("delete", res)
	}
}

// S3Store is a single namespaced store within an [S3Stores].
type S3Store struct {
	stores *S3Stores
	id     string
}

func (s *S3Store) Add(ctx context.Context, m Meta, r io.Reader) (Meta, error) {
	g := s.stores

	if m.Digest != "" {
		d, err := m.Digest.Sanitize()
		if err != nil {
			return m, err
		}
		m.Digest = d

		// Duplicate check is scoped to this store: only its reference counts.
		if ok, err := g.exists(ctx, g.refKey(d, s.id)); err != nil {
			return m, err
		} else if ok {
			return m, ErrAlreadyExists
		}
	}

	// Buffer to a temp file while hashing so the digest is known before upload
	// and can be reused as the blob's payload hash for signing.
	tf, err := os.CreateTemp("", "flob-s3-*")
	if err != nil {
		return m, fmt.Errorf("create temp: %w", err)
	}
	tp := tf.Name()
	defer os.Remove(tp)
	defer tf.Close()

	h := Canonical.Hash()
	n, err := io.Copy(io.MultiWriter(tf, h), r)
	if err != nil {
		return m, fmt.Errorf("buffer blob: %w", err)
	}
	m.Size = n

	d := Digest(fmt.Sprintf("sha256:%x", h.Sum(nil)))
	if m.Digest == "" {
		m.Digest = d
		if ok, err := g.exists(ctx, g.refKey(d, s.id)); err != nil {
			return m, err
		} else if ok {
			return m, ErrAlreadyExists
		}
	} else if m.Digest != d {
		return m, ErrDigestMismatch
	}

	// Ensure the shared blob exists. HEAD first so content already present from
	// another store is not re-uploaded.
	if ok, err := g.exists(ctx, g.blobKey(d)); err != nil {
		return m, err
	} else if !ok {
		if _, err := tf.Seek(0, io.SeekStart); err != nil {
			return m, fmt.Errorf("seek temp: %w", err)
		}
		if err := g.putBlob(ctx, d, tf, n); err != nil {
			return m, err
		}
	}

	if err := g.putRef(ctx, d, s.id, m.Labels, n); err != nil {
		return m, err
	}

	return m.Clone(), nil
}

// Stat implements [Stater] with a HEAD of this store's reference marker.
func (s *S3Store) Stat(ctx context.Context, d Digest) (int64, error) {
	d, err := d.Sanitize()
	if err != nil {
		return 0, ErrNotExist
	}
	res, err := s.stores.head(ctx, s.stores.refKey(d, s.id))
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()

	// Reference markers are empty; their metadata holds the blob's size.
	size, _ := strconv.ParseInt(res.Header.Get(metaPrefix+metaSizeKey), 10, 64)
	return size, nil
}

func (s *S3Store) Get(ctx context.Context, d Digest) (Meta, error) {
	d, err := d.Sanitize()
	if err != nil {
		return Meta{}, ErrNotExist
	}

	res, err := s.stores.head(ctx, s.stores.refKey(d, s.id))
	if err != nil {
		return Meta{}, err
	}
	defer res.Body.Close()

	labels, size := metaToLabels(res.Header)
	return Meta{Digest: d, Size: size, Labels: labels}, nil
}

func (s *S3Store) Open(ctx context.Context, d Digest) (io.ReadSeekCloser, Meta, error) {
	d, err := d.Sanitize()
	if err != nil {
		return nil, Meta{}, ErrNotExist
	}

	// Gate on this store's reference for isolation, and read labels/size from it.
	hres, err := s.stores.head(ctx, s.stores.refKey(d, s.id))
	if err != nil {
		return nil, Meta{}, err
	}
	labels, size := metaToLabels(hres.Header)
	hres.Body.Close()

	// Fetch the shared bytes. The blob is downloaded into memory to satisfy
	// io.ReadSeekCloser, matching HttpStore.Open.
	req, err := s.stores.newRequest(ctx, http.MethodGet, s.stores.blobKey(d), nil, nil)
	if err != nil {
		return nil, Meta{}, err
	}
	res, err := s.stores.send(req, emptyPayloadHash)
	if err != nil {
		return nil, Meta{}, err
	}
	defer res.Body.Close()
	switch res.StatusCode {
	case http.StatusOK:
		// ok
	case http.StatusNotFound:
		// Reference exists but shared blob is gone (a lost dedup race); the
		// content is not readable, so report it as missing.
		return nil, Meta{}, ErrNotExist
	default:
		return nil, Meta{}, statusError("get blob", res)
	}

	data, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, Meta{}, fmt.Errorf("read blob: %w", err)
	}
	return nopCloser{bytes.NewReader(data)}, Meta{Digest: d, Size: size, Labels: labels}, nil
}

// PresignOpen implements [Presigner]: it returns a short-lived direct download
// URL for the shared blob, gated on this store's reference marker so visibility
// isolation is preserved exactly as in [S3Store.Open].
func (s *S3Store) PresignOpen(ctx context.Context, d Digest, ttl time.Duration) (string, Meta, error) {
	d, err := d.Sanitize()
	if err != nil {
		return "", Meta{}, ErrNotExist
	}

	// The presigned URL points at the shared blob/ object, so this HEAD of the
	// per-store marker is what stops a store from handing out a URL for content
	// it never added.
	hres, err := s.stores.head(ctx, s.stores.refKey(d, s.id))
	if err != nil {
		return "", Meta{}, err
	}
	labels, size := metaToLabels(hres.Header)
	hres.Body.Close()

	loc := s.stores.presignGet(s.stores.blobKey(d), ttl)
	return loc, Meta{Digest: d, Size: size, Labels: labels}, nil
}

func (s *S3Store) Label(ctx context.Context, d Digest, labels Labels) error {
	d, err := d.Sanitize()
	if err != nil {
		return ErrNotExist
	}

	// Existence is defined by this store's reference; read its size so the
	// rewritten marker keeps it.
	hres, err := s.stores.head(ctx, s.stores.refKey(d, s.id))
	if err != nil {
		return err
	}
	_, size := metaToLabels(hres.Header)
	hres.Body.Close()

	return s.stores.putRef(ctx, d, s.id, labels, size)
}

func (s *S3Store) Erase(ctx context.Context, d Digest) error {
	d, err := d.Sanitize()
	if err != nil {
		// An invalid digest cannot correspond to any stored blob; Erase never
		// reports "not exist", so treat it as a successful no-op.
		return nil
	}

	// Remove only this store's reference. The shared blob is deliberately NOT
	// reclaimed here.
	//
	// S3 has no atomic "delete this object only if no other object references
	// it". An inline reclamation (LIST refs -> if none, DELETE blob) is a
	// check-then-act that cannot be made safe without a lock: a concurrent Add
	// for the same digest in another store may have already observed the blob
	// (HEAD 200) and skipped its upload but not yet written its reference marker,
	// so the LIST sees zero references and the DELETE destroys content that the
	// in-flight Add is about to (successfully) return. Because the marker is an
	// empty pointer object — unlike the OS backend's per-store hard link, which
	// physically holds the bytes — that content would be lost, not merely
	// unlinked. Reclaiming here would therefore violate the "correctness is
	// preserved; only reclaimable disk space is at risk" invariant.
	//
	// Reclamation is instead deferred to an out-of-band, grace-period sweep (see
	// s3.md), matching the OS backend's deferral of orphan collection: a leak of
	// reclaimable space is accepted in exchange for never destroying committed
	// content.
	return s.stores.deleteKey(ctx, s.stores.refKey(d, s.id))
}

// setLabelMeta writes each label as an x-amz-meta-<key> header. Multi-valued
// labels are joined with "," (HTTP list semantics); values must be US-ASCII and
// the combined metadata must stay under S3's 2 KiB limit.
func setLabelMeta(h http.Header, labels Labels) {
	for key, values := range labels {
		if key == "" {
			continue
		}
		h.Set(metaPrefix+key, strings.Join(values, ","))
	}
}

// metaToLabels reconstructs labels and the blob size from response headers,
// stripping the x-amz-meta- prefix and consuming the reserved size entry.
func metaToLabels(h http.Header) (Labels, int64) {
	var labels Labels
	var size int64
	for name, values := range h {
		rest, ok := strings.CutPrefix(name, metaPrefix)
		if !ok {
			continue
		}
		if strings.EqualFold(rest, metaSizeKey) {
			if len(values) > 0 {
				size, _ = strconv.ParseInt(values[0], 10, 64)
			}
			continue
		}
		if labels == nil {
			labels = make(Labels)
		}
		labels[rest] = append(labels[rest], values...)
	}
	return labels, size
}

// canonicalQuery renders q as a SigV4 canonical query string: entries encoded and
// sorted by key then value.
func canonicalQuery(q url.Values) string {
	type kv struct{ k, v string }
	pairs := make([]kv, 0, len(q))
	for k, vs := range q {
		for _, v := range vs {
			pairs = append(pairs, kv{awsURIEncode(k, true), awsURIEncode(v, true)})
		}
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].k != pairs[j].k {
			return pairs[i].k < pairs[j].k
		}
		return pairs[i].v < pairs[j].v
	})
	var b strings.Builder
	for i, p := range pairs {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(p.k)
		b.WriteByte('=')
		b.WriteString(p.v)
	}
	return b.String()
}

// statusError builds an error from a non-2xx S3 response, including a snippet of
// the body (S3 returns an XML <Error> document).
func statusError(op string, res *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(res.Body, 2048))
	msg := strings.TrimSpace(string(body))
	if msg == "" {
		return fmt.Errorf("s3 %s: %s", op, res.Status)
	}
	return fmt.Errorf("s3 %s: %s: %s", op, res.Status, msg)
}
