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
// visibility decision (Stat/Open/Label/Add's dup check) reads the marker. Open
// additionally checks the shared object's availability and representation.
// Erase removes only the namespace reference.

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"github.com/opencontainers/go-digest"
	"io"
	"iter"
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
	// Stage controls persistent upload expiry, receipt retention, and leases.
	Stage StageConfig
	// StagePartSize is the Append buffer/chunk size (default 16MiB; 5MiB–5GiB).
	// The chosen size is persisted per stage and uploads support at most 10000 parts.
	StagePartSize int64
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
	stage         StageConfig
	stagePartSize int64
	cl            *http.Client
	signer        signer
	scheme        string
	host          string
	pubScheme     string // scheme for presigned (client-facing) URLs
	pubHost       string // host for presigned (client-facing) URLs
	bucket        string
	prefix        string
	pathStyle     bool
}

// NewS3Stores builds an [S3Stores] from cfg.
func NewS3Stores(cfg S3Config) (*S3Stores, error) {
	if cfg.StagePartSize == 0 {
		cfg.StagePartSize = 16 << 20
	}
	if cfg.StagePartSize < 5<<20 || cfg.StagePartSize > 5<<30 {
		return nil, fmt.Errorf("S3 stage part size must be between 5MiB and 5GiB")
	}
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
		stage:         cfg.Stage.normalized(),
		stagePartSize: cfg.StagePartSize,
		cl:            cl,
		signer:        signer{creds: cfg.Credentials, region: cfg.Region, service: "s3", now: now},
		scheme:        scheme,
		host:          host,
		pubScheme:     pubScheme,
		pubHost:       pubHost,
		bucket:        cfg.Bucket,
		prefix:        prefix,
		pathStyle:     cfg.UsePathStyle,
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
	return s.prefix + "refs/" + d.Algorithm().String() + "/" + d.Encoded() + "/" + namespaceSegment(id)
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

// putBlob uploads the shared blob using its SHA-256 payload hash for signing.
func (s *S3Stores) putBlob(ctx context.Context, d Digest, body io.Reader, size int64, payloadHash string) error {
	req, err := s.newRequest(ctx, http.MethodPut, s.blobKey(d), nil, body)
	if err != nil {
		return err
	}
	req.ContentLength = size
	res, err := s.send(req, payloadHash)
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

	algo := Canonical
	if m.Digest != "" {
		d, err := m.Digest.Sanitize()
		if err != nil {
			return m, err
		}
		m.Digest = d
		algo = d.Algorithm()

		// Duplicate check is scoped to this store: only its reference counts.
		if ok, err := g.exists(ctx, g.refKey(d, s.id)); err != nil {
			return m, err
		} else if ok {
			return m, ErrAlreadyExists
		}
	}

	// Buffer to a temp file while computing the blob digest and the SHA-256
	// payload hash required for signing.
	tf, err := os.CreateTemp("", "flob-s3-*")
	if err != nil {
		return m, fmt.Errorf("create temp: %w", err)
	}
	tp := tf.Name()
	defer os.Remove(tp)
	defer tf.Close()

	h := algo.Hash()
	payloadHash := h
	w := io.MultiWriter(tf, h)
	if algo != Canonical {
		payloadHash = Canonical.Hash()
		w = io.MultiWriter(tf, h, payloadHash)
	}
	n, err := io.Copy(w, r)
	if err != nil {
		return m, fmt.Errorf("buffer blob: %w", err)
	}
	m.Size = n

	d := Digest(fmt.Sprintf("%s:%x", algo, h.Sum(nil)))
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
		if err := g.putBlob(ctx, d, tf, n, fmt.Sprintf("%x", payloadHash.Sum(nil))); err != nil {
			return m, err
		}
	}

	if err := g.putRef(ctx, d, s.id, m.Labels, n); err != nil {
		return m, err
	}

	return m.Clone(), nil
}

func (s *S3Store) Stat(ctx context.Context, d Digest) (Info, error) {
	d, err := d.Sanitize()
	if err != nil {
		return nil, ErrNotExist
	}

	res, err := s.stores.head(ctx, s.stores.refKey(d, s.id))
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	labels, size := metaToLabels(res.Header)
	return NewInfo(d, size, func(context.Context) (Labels, error) { return labels, nil }), nil
}

func (s *S3Store) Open(ctx context.Context, d Digest) (io.ReadSeekCloser, Info, error) {
	d, err := d.Sanitize()
	if err != nil {
		return nil, nil, ErrNotExist
	}

	// Gate on this store's reference for isolation, and read labels/size from it.
	hres, err := s.stores.head(ctx, s.stores.refKey(d, s.id))
	if err != nil {
		return nil, nil, err
	}
	labels, size := metaToLabels(hres.Header)
	hres.Body.Close()

	// The reference authorizes access; a separate blob HEAD preserves Open's
	// missing-object check and captures its representation validator and size.
	blob, err := s.stores.head(ctx, s.stores.blobKey(d))
	if err != nil {
		return nil, nil, err
	}
	defer blob.Body.Close()
	if size < 0 || blob.ContentLength != size || !identityResponse(blob) {
		return nil, nil, fmt.Errorf("blob size %d disagrees with reference size %d", blob.ContentLength, size)
	}
	blobURL := responseURL(blob)
	reader := newHTTPRangeReader(ctx, size, blobURL, blobURL, blob.Header.Get("ETag"), true,
		func(ctx context.Context, _ string, offset int64, etag string) (*http.Response, error) {
			req, err := s.stores.newRequest(ctx, http.MethodGet, s.stores.blobKey(d), nil, nil)
			if err != nil {
				return nil, err
			}
			req.Header.Set("Accept-Encoding", "identity")
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", offset, size-1))
			if etag != "" {
				req.Header.Set("If-Match", etag)
			}
			return s.stores.send(req, emptyPayloadHash)
		})
	return reader, NewInfo(d, size, func(context.Context) (Labels, error) { return labels, nil }), nil
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

var _ Walker = (*S3Store)(nil)
var _ Namespacer = (*S3Stores)(nil)

type s3ListPage struct {
	XMLName               xml.Name `xml:"ListBucketResult"`
	EncodingType          string
	IsTruncated           bool
	NextContinuationToken string
	Contents              []struct{ Key string }
}

// refKeys lists reference objects without reading blob bodies or metadata.
func (s *S3Stores) refKeys(ctx context.Context) iter.Seq2[string, error] {
	return func(yield func(string, error) bool) {
		token := ""
		seen := map[string]bool{}
		for {
			if err := ctx.Err(); err != nil {
				yield("", err)
				return
			}
			q := url.Values{"list-type": {"2"}, "prefix": {s.prefix + "refs/"}, "encoding-type": {"url"}}
			if token != "" {
				q.Set("continuation-token", token)
			}
			req, err := s.newRequest(ctx, http.MethodGet, "", q, nil)
			if err != nil {
				yield("", err)
				return
			}
			res, err := s.send(req, emptyPayloadHash)
			if err != nil {
				yield("", err)
				return
			}
			if res.StatusCode != http.StatusOK {
				err = statusError("list refs", res)
				res.Body.Close()
				yield("", err)
				return
			}
			var page s3ListPage
			err = xml.NewDecoder(res.Body).Decode(&page)
			res.Body.Close()
			if err != nil {
				yield("", fmt.Errorf("decode list refs: %w", err))
				return
			}
			if page.EncodingType != "" && page.EncodingType != "url" {
				yield("", fmt.Errorf("unsupported list encoding %q", page.EncodingType))
				return
			}
			for _, obj := range page.Contents {
				if err := ctx.Err(); err != nil {
					yield("", err)
					return
				}
				key := obj.Key
				if page.EncodingType == "url" {
					key, err = url.PathUnescape(key)
					if err != nil {
						yield("", fmt.Errorf("decode ref key: %w", err))
						return
					}
				}
				if !yield(key, nil) {
					return
				}
				if err := ctx.Err(); err != nil {
					yield("", err)
					return
				}
			}
			if !page.IsTruncated {
				return
			}
			token = page.NextContinuationToken
			if token == "" || seen[token] {
				yield("", errors.New("invalid list continuation token"))
				return
			}
			seen[token] = true
		}
	}
}

func (s *S3Stores) parseRefKey(key string) (Digest, string, bool) {
	rest, ok := strings.CutPrefix(key, s.prefix+"refs/")
	if !ok {
		return "", "", false
	}
	parts := strings.SplitN(rest, "/", 4)
	if len(parts) != 3 {
		return "", "", false
	}
	d, err := Digest(parts[0] + ":" + parts[1]).Sanitize()
	if err != nil || string(d) != parts[0]+":"+parts[1] {
		return "", "", false
	}
	id, err := namespaceID(parts[2])
	if err != nil || namespaceSegment(id) != parts[2] {
		return "", "", false
	}
	return d, id, true
}

func (s *S3Store) Walk(ctx context.Context) iter.Seq2[Info, error] {
	return func(yield func(Info, error) bool) {
		for key, err := range s.stores.refKeys(ctx) {
			if err != nil {
				yield(nil, err)
				return
			}
			d, id, ok := s.stores.parseRefKey(key)
			if !ok || id != s.id {
				continue
			}
			info, err := s.Stat(ctx, d)
			if errors.Is(err, ErrNotExist) {
				continue
			}
			if err != nil {
				yield(nil, err)
				return
			}
			if !yield(info, nil) {
				return
			}
		}
	}
}

func (s *S3Stores) Namespaces(ctx context.Context) iter.Seq2[string, error] {
	return func(yield func(string, error) bool) {
		seen := map[string]bool{}
		for key, err := range s.refKeys(ctx) {
			if err != nil {
				yield("", err)
				return
			}
			_, id, ok := s.parseRefKey(key)
			if !ok || seen[id] {
				continue
			}
			seen[id] = true
			if !yield(id, nil) {
				return
			}
		}
	}
}

// S3 stages keep immutable chunks and a conditionally replaced manifest. The
// manifest ETag is the fencing token; a finite lease bounds abandoned operations.
const s3StageMaxParts = 10000

type s3StageChunk struct {
	Key  string
	Size int64
}
type s3StageManifest struct {
	stageRecord
	PartSize   int64
	Chunks     []s3StageChunk
	Lease      string
	LeaseUntil time.Time
	UploadID   string
}
type s3Stage struct {
	store *S3Store
	id    string
}

func (s *s3Stage) ID() string { return s.id }
func (s *s3Stage) prefix() string {
	return s.store.stores.prefix + "stages/" + namespaceSegment(s.store.id) + "/" + s.id + "/"
}
func (s *s3Stage) key() string { return s.prefix() + "manifest" }
func s3StageNonce() string     { var b [16]byte; rand.Read(b[:]); return fmt.Sprintf("%x", b) }
func (s *s3Stage) read(ctx context.Context) (*s3StageManifest, string, error) {
	g := s.store.stores
	req, err := g.newRequest(ctx, http.MethodGet, s.key(), nil, nil)
	if err != nil {
		return nil, "", err
	}
	res, err := g.send(req, emptyPayloadHash)
	if err != nil {
		return nil, "", err
	}
	defer res.Body.Close()
	if res.StatusCode == 404 {
		return nil, "", ErrNotExist
	}
	if res.StatusCode != 200 {
		return nil, "", statusError("read stage", res)
	}
	var m s3StageManifest
	data, err := io.ReadAll(io.LimitReader(res.Body, (16<<20)+1))
	if err != nil {
		return nil, "", err
	}
	if len(data) > 16<<20 {
		return nil, "", ErrStageFormat
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&m) != nil || decoder.Decode(new(any)) != io.EOF {
		return nil, "", ErrStageFormat
	}
	if m.validate() != nil || m.ID != s.id || m.Namespace != namespaceSegment(s.store.id) {
		return nil, "", ErrStageFormat
	}
	if m.PartSize < 5<<20 || m.PartSize > 5<<30 || len(m.Chunks) > s3StageMaxParts {
		return nil, "", ErrStageFormat
	}
	var total int64
	for i, c := range m.Chunks {
		if !strings.HasPrefix(c.Key, s.prefix()+"chunks/") || c.Size <= 0 || c.Size > m.PartSize || (i < len(m.Chunks)-1 && c.Size != m.PartSize) {
			return nil, "", ErrStageFormat
		}
		total += c.Size
	}
	if total != m.Offset {
		return nil, "", ErrStageFormat
	}
	etag := strongETag(res.Header.Get("ETag"))
	if etag == "" {
		return nil, "", ErrStageFormat
	}
	return &m, etag, nil
}
func (s *s3Stage) save(ctx context.Context, m *s3StageManifest, etag string) (string, error) {
	data, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	g := s.store.stores
	req, err := g.newRequest(ctx, http.MethodPut, s.key(), nil, bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	req.ContentLength = int64(len(data))
	if etag == "" {
		req.Header.Set("If-None-Match", "*")
	} else {
		req.Header.Set("If-Match", etag)
	}
	res, err := g.send(req, fmt.Sprintf("%x", sha256.Sum256(data)))
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	if res.StatusCode == 409 || res.StatusCode == 412 {
		return "", ErrStageConflict
	}
	if res.StatusCode/100 != 2 {
		return "", statusError("save stage", res)
	}
	next := strongETag(res.Header.Get("ETag"))
	if next == "" {
		return "", ErrStageFormat
	}
	return next, nil
}
func (s *s3Stage) acquire(ctx context.Context) (*s3StageManifest, string, error) {
	m, etag, err := s.read(ctx)
	if err != nil {
		return nil, "", err
	}
	now := s.store.stores.stage.clock()
	if m.Lease != "" && now.Before(m.LeaseUntil) {
		return nil, "", ErrStageConflict
	}
	m.Lease = s3StageNonce()
	m.LeaseUntil = now.Add(s.store.stores.stage.OperationTimeout)
	etag, err = s.save(ctx, m, etag)
	return m, etag, err
}
func (s *s3Stage) release(ctx context.Context, m *s3StageManifest, etag string) error {
	m.Lease = ""
	m.LeaseUntil = time.Time{}
	_, err := s.save(ctx, m, etag)
	return err
}
func (s *s3Stage) expired(m *s3StageManifest) bool {
	return !s.store.stores.stage.clock().Before(m.ExpiresAt)
}
func (s *S3Store) Begin(ctx context.Context, algo digest.Algorithm) (Stage, error) {
	ctx, cancel := context.WithTimeout(ctx, s.stores.stage.OperationTimeout)
	defer cancel()
	record, err := newStageRecord(s.id, algo, s.stores.stage)
	if err != nil {
		return nil, err
	}
	st := &s3Stage{store: s, id: record.ID}
	m := &s3StageManifest{stageRecord: record, PartSize: s.stores.stagePartSize}
	if _, err := st.save(ctx, m, ""); err != nil {
		return nil, err
	}
	return st, nil
}
func (s *S3Store) Resume(ctx context.Context, id string) (Stage, error) {
	ctx, cancel := context.WithTimeout(ctx, s.stores.stage.OperationTimeout)
	defer cancel()
	if len(id) != 32 || strings.Trim(id, "0123456789abcdef") != "" {
		return nil, ErrNotExist
	}
	st := &s3Stage{store: s, id: id}
	m, _, err := st.read(ctx)
	if err != nil {
		return nil, err
	}
	if m.State != StageCommitting && st.expired(m) {
		return nil, ErrStageExpired
	}
	return st, nil
}
func (s *s3Stage) Stat(ctx context.Context) (StageInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, s.store.stores.stage.OperationTimeout)
	defer cancel()
	m, _, err := s.read(ctx)
	if err != nil {
		return StageInfo{}, err
	}
	if m.State != StageCommitting && s.expired(m) {
		return StageInfo{}, ErrStageExpired
	}
	return StageInfo{Algorithm: m.Algorithm, Offset: m.Offset, State: m.State, ExpiresAt: m.ExpiresAt}, nil
}
func (s *s3Stage) Append(ctx context.Context, expected int64, r io.Reader) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, s.store.stores.stage.OperationTimeout)
	defer cancel()
	m, etag, err := s.acquire(ctx)
	if err != nil {
		return 0, err
	}
	released := false
	defer func() {
		if !released {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.store.stores.stage.OperationTimeout)
			defer cancel()
			s.release(cleanup, m, etag)
		}
	}()
	if m.State != StageActive {
		return m.Offset, ErrStageClosed
	}
	if s.expired(m) {
		return m.Offset, ErrStageExpired
	}
	if m.Offset != expected {
		return m.Offset, ErrOffsetMismatch
	}
	h, err := stageHashRestore(m.Algorithm, m.Hash)
	if err != nil {
		return m.Offset, err
	}
	chunks := append([]s3StageChunk(nil), m.Chunks...)
	var tail []byte
	if len(chunks) > 0 && chunks[len(chunks)-1].Size < m.PartSize {
		last := chunks[len(chunks)-1]
		chunks = chunks[:len(chunks)-1]
		req, e := s.store.stores.newRequest(ctx, http.MethodGet, last.Key, nil, nil)
		if e != nil {
			return m.Offset, e
		}
		res, e := s.store.stores.send(req, emptyPayloadHash)
		if e != nil {
			return m.Offset, e
		}
		if res.StatusCode != 200 {
			e = statusError("read stage tail", res)
			res.Body.Close()
			return m.Offset, e
		}
		tail, e = io.ReadAll(io.LimitReader(res.Body, m.PartSize))
		res.Body.Close()
		if e != nil {
			return m.Offset, e
		}
		if int64(len(tail)) != last.Size {
			return m.Offset, ErrStageFormat
		}
	}
	source := &s3StageInput{r: stageContextReader{ctx: ctx, r: r}}
	input := io.MultiReader(bytes.NewReader(tail), io.TeeReader(source, h))
	written := int64(0)
	buf := make([]byte, m.PartSize)
	for {
		n, e := io.ReadFull(input, buf)
		if source.err != nil {
			return m.Offset, source.err
		}
		if e != nil && e != io.EOF && e != io.ErrUnexpectedEOF {
			return m.Offset, e
		}
		if err := ctx.Err(); err != nil {
			return m.Offset, err
		}
		if n > 0 {
			if len(chunks) >= s3StageMaxParts {
				return m.Offset, fmt.Errorf("S3 stage exceeds %d parts", s3StageMaxParts)
			}
			key := s.prefix() + "chunks/" + m.Lease + "/" + strconv.Itoa(len(chunks))
			req, err := s.store.stores.newRequest(ctx, http.MethodPut, key, nil, bytes.NewReader(buf[:n]))
			if err != nil {
				return m.Offset, err
			}
			req.ContentLength = int64(n)
			req.Header.Set("If-None-Match", "*")
			res, err := s.store.stores.send(req, fmt.Sprintf("%x", sha256.Sum256(buf[:n])))
			if err != nil {
				return m.Offset, err
			}
			if res.StatusCode/100 != 2 {
				err = statusError("write stage chunk", res)
			}
			res.Body.Close()
			if err != nil {
				return m.Offset, err
			}
			chunks = append(chunks, s3StageChunk{Key: key, Size: int64(n)})
			written += int64(n)
		}
		if e != nil {
			break
		}
	}
	checkpoint, err := stageHashState(h)
	if err != nil {
		return m.Offset, err
	}
	next := *m
	next.Chunks = chunks
	next.Offset = m.Offset + written - int64(len(tail))
	next.Hash = checkpoint
	next.ExpiresAt = s.store.stores.stage.clock().Add(s.store.stores.stage.TTL)
	if err := s.release(ctx, &next, etag); err != nil {
		return m.Offset, err
	}
	released = true
	return next.Offset, nil
}

func (s *s3Stage) Commit(ctx context.Context, wanted Meta) (Meta, error) {
	ctx, cancel := context.WithTimeout(ctx, s.store.stores.stage.OperationTimeout)
	defer cancel()
	m, etag, err := s.acquire(ctx)
	if err != nil {
		return Meta{}, err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.store.stores.stage.OperationTimeout)
		defer cancel()
		s.release(cleanup, m, etag)
	}()
	if m.State != StageCommitting && s.expired(m) {
		return Meta{}, ErrStageExpired
	}
	prepared, err := m.prepareCommit(wanted)
	if err != nil {
		return Meta{}, err
	}
	if m.State == StageCommitted {
		if !stageCommitMatches(m.Commit, prepared) {
			return Meta{}, ErrStageConflict
		}
		return m.Result.Clone(), nil
	}
	if m.State == StageAborted {
		return Meta{}, ErrStageClosed
	}
	if m.State == StageActive {
		if s.expired(m) {
			return Meta{}, ErrStageExpired
		}
		m.State = StageCommitting
		m.Commit = prepared
		etag, err = s.save(ctx, m, etag)
		if err != nil {
			return Meta{}, err
		}
	} else if m.State != StageCommitting {
		return Meta{}, ErrStageFormat
	} else if !stageCommitMatches(m.Commit, prepared) {
		return Meta{}, ErrStageConflict
	}
	result, err := s.publish(ctx, m, &etag)
	if err != nil {
		return Meta{}, err
	}
	m.State = StageCommitted
	m.Result = result.Clone()
	m.ExpiresAt = s.store.stores.stage.clock().Add(s.store.stores.stage.Retention)
	etag, err = s.save(ctx, m, etag)
	if err != nil {
		return Meta{}, err
	}
	return result, nil
}
func (s *s3Stage) publish(ctx context.Context, m *s3StageManifest, etag *string) (Meta, error) {
	g := s.store.stores
	if info, err := s.store.Stat(ctx, m.Commit.Digest); err == nil {
		return infoMeta(ctx, info)
	} else if !errors.Is(err, ErrNotExist) {
		return Meta{}, err
	}
	exists, err := g.exists(ctx, g.blobKey(m.Commit.Digest))
	if err != nil {
		return Meta{}, err
	}
	if !exists {
		if m.Offset == 0 {
			if err := g.putBlob(ctx, m.Commit.Digest, bytes.NewReader(nil), 0, emptyPayloadHash); err != nil {
				return Meta{}, err
			}
		} else if err := s.assemble(ctx, m, etag); err != nil {
			return Meta{}, err
		}
	}
	req, err := g.newRequest(ctx, http.MethodPut, g.refKey(m.Commit.Digest, s.store.id), nil, nil)
	if err != nil {
		return Meta{}, err
	}
	req.ContentLength = 0
	req.Header.Set("If-None-Match", "*")
	setLabelMeta(req.Header, m.Commit.Labels)
	req.Header.Set(metaPrefix+metaSizeKey, strconv.FormatInt(m.Commit.Size, 10))
	res, err := g.send(req, emptyPayloadHash)
	if err != nil {
		return Meta{}, err
	}
	defer res.Body.Close()
	if res.StatusCode == 412 {
		info, err := s.store.Stat(ctx, m.Commit.Digest)
		if err != nil {
			return Meta{}, err
		}
		return infoMeta(ctx, info)
	}
	if res.StatusCode/100 != 2 {
		return Meta{}, statusError("commit stage ref", res)
	}
	return m.Commit.Clone(), nil
}
func (s *s3Stage) assemble(ctx context.Context, m *s3StageManifest, etag *string) error {
	g := s.store.stores
	key := g.blobKey(m.Commit.Digest)
	if m.UploadID == "" {
		req, err := g.newRequest(ctx, http.MethodPost, key, url.Values{"uploads": {""}}, nil)
		if err != nil {
			return err
		}
		res, err := g.send(req, emptyPayloadHash)
		if err != nil {
			return err
		}
		var created struct {
			UploadID string `xml:"UploadId"`
		}
		if res.StatusCode/100 != 2 {
			err = statusError("create stage multipart", res)
		} else {
			err = xml.NewDecoder(res.Body).Decode(&created)
		}
		res.Body.Close()
		if err != nil {
			return err
		}
		if created.UploadID == "" {
			return ErrStageFormat
		}
		m.UploadID = created.UploadID
		*etag, err = s.save(ctx, m, *etag)
		if err != nil {
			return err
		}
	}
	type part struct {
		Number int    `xml:"PartNumber"`
		ETag   string `xml:"ETag"`
	}
	completed := struct {
		XMLName xml.Name `xml:"CompleteMultipartUpload"`
		Parts   []part   `xml:"Part"`
	}{}
	for i, c := range m.Chunks {
		req, err := g.newRequest(ctx, http.MethodPut, key, url.Values{"uploadId": {m.UploadID}, "partNumber": {strconv.Itoa(i + 1)}}, nil)
		if err != nil {
			return err
		}
		req.Header.Set("X-Amz-Copy-Source", awsURIEncode("/"+g.bucket+"/"+c.Key, false))
		res, err := g.send(req, emptyPayloadHash)
		if err != nil {
			return err
		}
		var copied struct {
			XMLName xml.Name `xml:"CopyPartResult"`
			ETag    string
		}
		if res.StatusCode == 404 {
			res.Body.Close()
			m.UploadID = ""
			*etag, err = s.save(ctx, m, *etag)
			if err != nil {
				return err
			}
			return ErrStageConflict
		}
		if res.StatusCode/100 != 2 {
			err = statusError("copy stage part", res)
		} else {
			err = xml.NewDecoder(res.Body).Decode(&copied)
		}
		res.Body.Close()
		if err != nil {
			return err
		}
		if copied.ETag == "" {
			return ErrStageFormat
		}
		completed.Parts = append(completed.Parts, part{Number: i + 1, ETag: copied.ETag})
	}
	data, err := xml.Marshal(completed)
	if err != nil {
		return err
	}
	req, err := g.newRequest(ctx, http.MethodPost, key, url.Values{"uploadId": {m.UploadID}}, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.ContentLength = int64(len(data))
	res, err := g.send(req, fmt.Sprintf("%x", sha256.Sum256(data)))
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode/100 != 2 {
		return statusError("complete stage multipart", res)
	}
	var result struct {
		XMLName xml.Name `xml:"CompleteMultipartUploadResult"`
	}
	if err := xml.NewDecoder(res.Body).Decode(&result); err != nil {
		return fmt.Errorf("complete stage multipart: %w", err)
	}
	return nil
}
func (s *s3Stage) Abort(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, s.store.stores.stage.OperationTimeout)
	defer cancel()
	defer func() { s.cleanTerminal(ctx) }()
	m, etag, err := s.acquire(ctx)
	if err != nil {
		if errors.Is(err, ErrNotExist) {
			return nil
		}
		return err
	}
	if m.State == StageCommitting {
		// Publish the frozen operation before reporting closure; never delete an
		// in-flight commit's chunks or its durable recovery record.
		result, e := s.publish(ctx, m, &etag)
		if e != nil {
			s.release(ctx, m, etag)
			return e
		}
		m.State = StageCommitted
		m.Result = result
		m.ExpiresAt = s.store.stores.stage.clock().Add(s.store.stores.stage.Retention)
		if err := s.release(ctx, m, etag); err != nil {
			return err
		}
		return nil
	}
	if m.State == StageCommitted {
		s.release(ctx, m, etag)
		return nil
	}
	if m.State != StageAborted {
		m.State = StageAborted
		m.ExpiresAt = s.store.stores.stage.clock().Add(s.store.stores.stage.Retention)
	}
	return s.release(ctx, m, etag)
}

type s3StageListedKey struct {
	Key          string
	LastModified time.Time
}

func (g *S3Stores) stageKeys(ctx context.Context, only ...string) ([]s3StageListedKey, error) {
	prefix := g.prefix + "stages/"
	if len(only) > 0 {
		prefix = only[0]
	}
	var keys []s3StageListedKey
	token := ""
	seen := map[string]bool{}
	for {
		q := url.Values{"list-type": {"2"}, "prefix": {prefix}, "encoding-type": {"url"}}
		if token != "" {
			q.Set("continuation-token", token)
		}
		req, err := g.newRequest(ctx, http.MethodGet, "", q, nil)
		if err != nil {
			return nil, err
		}
		res, err := g.send(req, emptyPayloadHash)
		if err != nil {
			return nil, err
		}
		var page struct {
			XMLName                             xml.Name `xml:"ListBucketResult"`
			Contents                            []s3StageListedKey
			IsTruncated                         bool
			NextContinuationToken, EncodingType string
		}
		if res.StatusCode != 200 {
			err = statusError("list stages", res)
		} else {
			err = xml.NewDecoder(res.Body).Decode(&page)
		}
		res.Body.Close()
		if err != nil {
			return nil, err
		}
		for _, key := range page.Contents {
			if page.EncodingType == "url" {
				key.Key, err = url.PathUnescape(key.Key)
				if err != nil {
					return nil, err
				}
			}
			keys = append(keys, key)
		}
		if !page.IsTruncated {
			return keys, nil
		}
		token = page.NextContinuationToken
		if token == "" || seen[token] {
			return nil, ErrStageFormat
		}
		seen[token] = true
	}
}
func (g *S3Stores) PruneStages(ctx context.Context) (int, error) {
	keys, err := g.stageKeys(ctx)
	if err != nil {
		return 0, err
	}
	groups := map[string][]s3StageListedKey{}
	stages := map[string]*s3Stage{}
	records := map[string]*s3StageManifest{}
	protected := map[string]bool{}
	for _, key := range keys {
		rest, ok := strings.CutPrefix(key.Key, g.prefix+"stages/")
		if !ok {
			continue
		}
		parts := strings.SplitN(rest, "/", 3)
		if len(parts) != 3 || !validStageID(parts[1]) {
			continue
		}
		id, e := namespaceID(parts[0])
		if e != nil || namespaceSegment(id) != parts[0] {
			continue
		}
		group := parts[0] + "/" + parts[1]
		groups[group] = append(groups[group], key)
		if parts[2] == "manifest" {
			stages[group] = &s3Stage{store: &S3Store{stores: g, id: id}, id: parts[1]}
		}
	}
	// Finish the inventory before deleting any multipart uploads. Any unreadable
	// manifest stops pruning because its frozen digest cannot be protected safely.
	for group, st := range stages {
		m, _, e := st.read(ctx)
		if e != nil {
			if errors.Is(e, ErrNotExist) {
				continue
			}
			return 0, e
		}
		records[group] = m
		if m.State == StageCommitting {
			protected[g.blobKey(m.Commit.Digest)] = true
		}
	}
	count := 0
	var failures error
	for group, m := range records {
		func() {
			ctx, cancel := context.WithTimeout(ctx, g.stage.OperationTimeout)
			defer cancel()
			st := stages[group]
			now := g.stage.clock()
			if m.Lease != "" && now.Before(m.LeaseUntil) {
				return
			}
			if m.State == StageCommitting {
				if _, e := st.Commit(ctx, m.Commit); e != nil {
					failures = errors.Join(failures, e)
					return
				}
			}
			locked, tag, e := st.acquire(ctx)
			if e != nil {
				if !errors.Is(e, ErrStageConflict) {
					failures = errors.Join(failures, e)
				}
				return
			}
			if locked.State == StageCommitting {
				protected[g.blobKey(locked.Commit.Digest)] = true
				st.release(ctx, locked, tag)
				return
			}
			expired := st.expired(locked)
			e = st.cleanChunks(ctx, locked, groups[group], expired)
			if e != nil {
				failures = errors.Join(failures, e)
				st.release(ctx, locked, tag)
				return
			}
			if !expired {
				if e = st.release(ctx, locked, tag); e != nil {
					failures = errors.Join(failures, e)
				}
				return
			}
			req, x := g.newRequest(ctx, http.MethodDelete, st.key(), nil, nil)
			if x != nil {
				e = x
			} else {
				req.Header.Set("If-Match", tag)
				res, x := g.send(req, emptyPayloadHash)
				if x != nil {
					e = x
				} else {
					if res.StatusCode != 204 && res.StatusCode != 200 && res.StatusCode != 404 {
						e = statusError("prune stage", res)
					}
					res.Body.Close()
				}
			}
			if e != nil {
				failures = errors.Join(failures, e)
				st.release(ctx, locked, tag)
			} else {
				count++
			}
		}()
	}
	// Failed/stale appends may have uploaded immutable chunks before losing CAS.
	// A manifest-less group is reclaimable only after every object has aged past
	// the TTL and maximum operation duration; fresh in-flight writes remain safe.
	cutoff := g.stage.clock().Add(-g.stage.TTL - g.stage.OperationTimeout)
	for group, objects := range groups {
		if _, ok := stages[group]; ok {
			continue
		}
		old := true
		for _, obj := range objects {
			if obj.LastModified.IsZero() || !obj.LastModified.Before(cutoff) {
				old = false
			}
		}
		if !old {
			continue
		}
		for _, obj := range objects {
			if e := g.deleteKey(ctx, obj.Key); e != nil {
				failures = errors.Join(failures, e)
			}
		}
	}
	if e := g.pruneStageUploads(ctx, protected, cutoff); e != nil {
		failures = errors.Join(failures, e)
	}
	return count, failures
}
func (g *S3Stores) pruneStageUploads(ctx context.Context, protected map[string]bool, cutoff time.Time) error {
	keyMarker, idMarker := "", ""
	seen := map[string]bool{}
	for {
		q := url.Values{"uploads": {""}, "prefix": {g.prefix + "blob/"}, "encoding-type": {"url"}}
		if keyMarker != "" {
			q.Set("key-marker", keyMarker)
		}
		if idMarker != "" {
			q.Set("upload-id-marker", idMarker)
		}
		req, err := g.newRequest(ctx, http.MethodGet, "", q, nil)
		if err != nil {
			return err
		}
		res, err := g.send(req, emptyPayloadHash)
		if err != nil {
			return err
		}
		var page struct {
			XMLName xml.Name `xml:"ListMultipartUploadsResult"`
			Upload  []struct {
				Key       string
				UploadID  string `xml:"UploadId"`
				Initiated time.Time
			}
			IsTruncated        bool
			NextKeyMarker      string
			NextUploadIDMarker string `xml:"NextUploadIdMarker"`
			EncodingType       string
		}
		if res.StatusCode != 200 {
			err = statusError("list stage multipart uploads", res)
		} else {
			err = xml.NewDecoder(res.Body).Decode(&page)
		}
		res.Body.Close()
		if err != nil {
			return err
		}
		for _, up := range page.Upload {
			if page.EncodingType == "url" {
				up.Key, err = url.PathUnescape(up.Key)
				if err != nil {
					return err
				}
			}
			if !strings.HasPrefix(up.Key, g.prefix+"blob/") || protected[up.Key] || up.Initiated.IsZero() || !up.Initiated.Before(cutoff) {
				continue
			}
			req, err := g.newRequest(ctx, http.MethodDelete, up.Key, url.Values{"uploadId": {up.UploadID}}, nil)
			if err != nil {
				return err
			}
			res, err := g.send(req, emptyPayloadHash)
			if err != nil {
				return err
			}
			if res.StatusCode != 204 && res.StatusCode != 404 {
				err = statusError("abort abandoned stage multipart", res)
			}
			res.Body.Close()
			if err != nil {
				return err
			}
		}
		if !page.IsTruncated {
			return nil
		}
		keyMarker = page.NextKeyMarker
		idMarker = page.NextUploadIDMarker
		if page.EncodingType == "url" {
			keyMarker, err = url.PathUnescape(keyMarker)
			if err != nil {
				return err
			}
		}
		token := keyMarker + "\x00" + idMarker
		if token == "\x00" || seen[token] {
			return ErrStageFormat
		}
		seen[token] = true
	}
}

// Retain source errors even when io.ReadFull fills its buffer and suppresses the
// accompanying error, or treats a transport's ErrUnexpectedEOF as a short tail.
type s3StageInput struct {
	r   io.Reader
	err error
}

func (r *s3StageInput) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	if err != nil && err != io.EOF {
		r.err = err
	}
	return n, err
}

// cleanChunks runs under a manifest lease. Active stages retain all currently
// referenced chunks and any recent attempt data that could still be in flight.
func (s *s3Stage) cleanChunks(ctx context.Context, m *s3StageManifest, objects []s3StageListedKey, expired bool) error {
	keep := map[string]bool{}
	for _, chunk := range m.Chunks {
		keep[chunk.Key] = true
	}
	terminal := m.State == StageCommitted || m.State == StageAborted
	cutoff := s.store.stores.stage.clock().Add(-s.store.stores.stage.OperationTimeout)
	for _, obj := range objects {
		if !strings.HasPrefix(obj.Key, s.prefix()+"chunks/") {
			continue
		}
		if !terminal && !expired && (keep[obj.Key] || obj.LastModified.IsZero() || !obj.LastModified.Before(cutoff)) {
			continue
		}
		if err := s.store.stores.deleteKey(ctx, obj.Key); err != nil {
			return err
		}
	}
	return nil
}
func (s *s3Stage) cleanTerminal(ctx context.Context) {
	m, tag, err := s.acquire(ctx)
	if err != nil {
		return
	}
	defer s.release(ctx, m, tag)
	if m.State != StageCommitted && m.State != StageAborted {
		return
	}
	keys, err := s.store.stores.stageKeys(ctx, s.prefix())
	if err != nil {
		return
	}
	s.cleanChunks(ctx, m, keys, false)
}
