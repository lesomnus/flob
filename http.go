package flob

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// HttpHandler servers [Stores] over HTTP.
//
//	  POST /{store-id}          - [Store.Add]
//	  POST /{store-id}/{digest} - [Store.Add] with pre-computed digest
//	  HEAD /{store-id}/{digest} - [Store.Stat]
//	   GET /{store-id}/{digest} - [Store.Open]
//	 PATCH /{store-id}/{digest} - [Store.Label]
//	DELETE /{store-id}/{digest} - [Store.Erase]
//
// Headers with the "Flob-" prefix are saved as labels and returned in the response
// of GET and HEAD requests without the prefix.
type HttpHandler struct {
	Stores Stores

	// Redirect, when true, serves GET by redirecting to a backend-provided
	// presigned URL when [AsPresigner] finds that capability. If unavailable or
	// presigning fails (including [ErrNotExist]), it falls back to [Store.Open].
	Redirect bool
	// RedirectTTL bounds the validity of presigned redirect URLs. When zero,
	// [DefaultRedirectTTL] is used.
	RedirectTTL time.Duration
}

// DefaultRedirectTTL is the presigned-URL lifetime used when
// [HttpHandler.RedirectTTL] is unset.
const DefaultRedirectTTL = 15 * time.Minute

func (h HttpHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	id, digest_raw, ok := h.parsePath(r.URL.EscapedPath())
	if !ok {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	store := h.Stores.Use(id)

	d := Digest(digest_raw)
	if d != "" {
		var err error
		d, err = d.Sanitize()
		if err != nil {
			http.Error(w, "invalid digest: "+err.Error(), http.StatusBadRequest)
			return
		}
	} else if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ctx := r.Context()
	switch r.Method {
	case http.MethodPost:
		labels := Labels{}
		h.parseLabels(labels, r)

		m, err := store.Add(ctx, Meta{Digest: d, Labels: labels}, r.Body)
		if err != nil {
			switch {
			case errors.Is(err, ErrNotExist):
				http.Error(w, err.Error(), http.StatusNotFound)
			case errors.Is(err, ErrAlreadyExists):
				h.setMetaHeaders(w, m)
				// This response carries no body, so the blob size set by
				// setMetaHeaders must not be advertised as its length or strict
				// clients (and reverse proxies) wait for bytes that never come.
				w.Header().Del("Content-Length")
				w.WriteHeader(http.StatusOK)
			case errors.Is(err, ErrDigestMismatch):
				http.Error(w, err.Error(), http.StatusUnprocessableEntity)
			default:
				http.Error(w, err.Error(), http.StatusInternalServerError)
			}
			return
		}
		h.setMetaHeaders(w, m)
		w.Header().Set("Location", "/"+namespaceSegment(id)+"/"+string(m.Digest))
		// This response carries no body, so the blob size set by setMetaHeaders
		// must not be advertised as its length or strict clients (and reverse
		// proxies) wait for bytes that never come.
		w.Header().Del("Content-Length")
		w.WriteHeader(http.StatusCreated)

	case http.MethodHead:
		info, err := store.Stat(r.Context(), d)
		if err != nil {
			switch {
			case errors.Is(err, ErrNotExist):
				http.Error(w, err.Error(), http.StatusNotFound)
			default:
				http.Error(w, err.Error(), http.StatusInternalServerError)
			}
			return
		}
		m, err := infoMeta(r.Context(), info)
		var modified string
		if err == nil {
			modified, err = lastModified(r.Context(), info)
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		h.setMetaHeaders(w, m)
		if modified != "" {
			w.Header().Set("Last-Modified", modified)
		}
		w.WriteHeader(http.StatusOK)

	case http.MethodGet:
		if h.Redirect {
			if p, ok := AsPresigner(store); ok {
				ttl := h.RedirectTTL
				if ttl <= 0 {
					ttl = DefaultRedirectTTL
				}
				loc, m, err := p.PresignOpen(ctx, d, ttl)
				switch {
				case err == nil:
					h.setMetaHeaders(w, m)
					// The 307 body is a tiny placeholder, not the blob, so the
					// blob size must not be advertised as this response's length.
					w.Header().Del("Content-Length")
					w.Header().Set("Cache-Control", "no-store")
					http.Redirect(w, r, loc, http.StatusTemporaryRedirect)
					return
				default:
					// Fall back to streaming, including when the primary misses:
					// a wrapping store may still read from an origin or secondary.
				}
			}
		}

		rc, info, err := store.Open(r.Context(), d)
		if err != nil {
			switch {
			case errors.Is(err, ErrNotExist):
				http.Error(w, err.Error(), http.StatusNotFound)
			default:
				http.Error(w, err.Error(), http.StatusInternalServerError)
			}
			return
		}
		defer rc.Close()
		m, err := infoMeta(r.Context(), info)
		var modified string
		if err == nil {
			modified, err = lastModified(r.Context(), info)
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		h.setMetaHeaders(w, m)
		if modified != "" {
			w.Header().Set("Last-Modified", modified)
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		http.ServeContent(w, r, "", time.Time{}, rc)

	case http.MethodPatch:
		labels := Labels{}
		h.parseLabels(labels, r)

		if err := store.Label(r.Context(), d, labels); err != nil {
			switch {
			case errors.Is(err, ErrNotExist):
				http.Error(w, err.Error(), http.StatusNotFound)
			default:
				http.Error(w, err.Error(), http.StatusInternalServerError)
			}
			return
		}
		w.WriteHeader(http.StatusNoContent)

	case http.MethodDelete:
		if err := store.Erase(r.Context(), d); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// parsePath splits /{store-id} or /{store-id}/{digest} from the escaped request
// path, URL-unescapes each segment once, and decodes the namespace ID.
// Any deeper path returns ok=false.
func (h *HttpHandler) parsePath(path string) (storeID, digest string, ok bool) {
	parts := strings.SplitN(strings.TrimPrefix(path, "/"), "/", 3)
	if len(parts) > 2 || parts[0] == "" {
		return "", "", false
	}
	segment, err := url.PathUnescape(parts[0])
	if err != nil {
		return "", "", false
	}
	storeID, err = namespaceID(segment)
	if err != nil {
		return "", "", false
	}
	if len(parts) == 2 {
		digest, err = url.PathUnescape(parts[1])
		if err != nil {
			return "", "", false
		}
	}
	return storeID, digest, true
}

// parseLabels extracts headers with the "Flob-" prefix from r, strips the prefix,
// and updates vs with the resulting key-value pairs.
func (h *HttpHandler) parseLabels(vs Labels, r *http.Request) {
	for key, values := range r.Header {
		if after, ok := strings.CutPrefix(key, "Flob-"); ok {
			vs[after] = values
		}
	}
}

// setMetaHeaders writes ETag, Content-Length, and label headers to w.
func (h *HttpHandler) setMetaHeaders(w http.ResponseWriter, m Meta) {
	w.Header().Set("ETag", `"`+string(m.Digest)+`"`)
	w.Header().Set("Content-Length", strconv.FormatInt(m.Size, 10))

	for key, values := range m.Labels {
		for _, v := range values {
			w.Header().Add(key, v)
		}
	}
}

// lastModified formats [Info.Added] for the Last-Modified header, or returns ""
// when the store does not know when the blob was added.
func lastModified(ctx context.Context, info Info) (string, error) {
	added, err := info.Added(ctx)
	if errors.Is(err, errors.ErrUnsupported) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return added.UTC().Format(http.TimeFormat), nil
}

// parseLastModified reads a Last-Modified header as the time a blob was added.
// HTTP dates have second precision; a missing or malformed header is unknown.
func parseLastModified(h http.Header) time.Time {
	added, _ := http.ParseTime(h.Get("Last-Modified"))
	return added
}

// HttpStores is an HTTP client for [HttpHandler] server.
type HttpStores struct {
	Client *http.Client
	Target string // base URL of the server, e.g. "http://localhost:8080"
}

func (s HttpStores) Use(id string) Store {
	client := s.Client
	if client == nil {
		client = http.DefaultClient
	}
	return HttpStore{
		client: client,
		base:   strings.TrimRight(s.Target, "/"),
		id:     namespaceSegment(id),
	}
}

type HttpStore struct {
	client *http.Client
	base   string
	id     string
}

func (s HttpStore) url(d Digest) string {
	if d == "" {
		return s.base + "/" + s.id
	}
	return s.base + "/" + s.id + "/" + string(d)
}

func (s HttpStore) Add(ctx context.Context, m Meta, r io.Reader) (Meta, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url(m.Digest), r)
	if err != nil {
		return Meta{}, err
	}
	s.setLabels(req, m.Labels)

	res, err := s.client.Do(req)
	if err != nil {
		return Meta{}, err
	}
	defer res.Body.Close()

	m = s.parseMeta(res)
	switch res.StatusCode {
	case http.StatusOK:
		err = ErrAlreadyExists
	case http.StatusCreated:
		err = nil
	case http.StatusUnprocessableEntity:
		err = ErrDigestMismatch
	case http.StatusNotFound:
		err = ErrNotExist
	default:
		err = fmt.Errorf("unexpected HTTP status: %s", res.Status)
	}
	return m, err
}

func (s HttpStore) Stat(ctx context.Context, d Digest) (Info, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, s.url(d), nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if err := s.parseErr(resp); err != nil {
		return nil, err
	}
	m := s.parseMeta(resp)
	return NewInfo(m.Digest, m.Size, parseLastModified(resp.Header), func(context.Context) (Labels, error) { return m.Labels, nil }), nil
}

// Open retrieves metadata with HEAD and streams content lazily through ranged
// GET requests. Seeking does not download bytes. See [httpRangeReader] for the
// range and representation-consistency checks shared with the S3 backend.
func (s HttpStore) Open(ctx context.Context, d Digest) (io.ReadSeekCloser, Info, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, s.url(d), nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Accept-Encoding", "identity")
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	if err := s.parseErr(resp); err != nil {
		return nil, nil, err
	}
	if resp.StatusCode != http.StatusOK || resp.ContentLength < 0 || !identityResponse(resp) {
		return nil, nil, fmt.Errorf("invalid blob HEAD response: status %d, length %d", resp.StatusCode, resp.ContentLength)
	}
	m := s.parseMeta(resp)
	reader := newHTTPRangeReader(ctx, m.Size, s.url(d), responseURL(resp), resp.Header.Get("ETag"), false,
		func(ctx context.Context, target string, offset int64, etag string) (*http.Response, error) {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
			if err != nil {
				return nil, err
			}
			req.Header.Set("Accept-Encoding", "identity")
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", offset, m.Size-1))
			if etag != "" {
				req.Header.Set("If-Match", etag)
			}
			return s.client.Do(req)
		})
	return reader, NewInfo(m.Digest, m.Size, parseLastModified(resp.Header), func(context.Context) (Labels, error) { return m.Labels, nil }), nil
}

func (s HttpStore) Label(ctx context.Context, d Digest, labels Labels) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, s.url(d), nil)
	if err != nil {
		return err
	}
	s.setLabels(req, labels)
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return s.parseErr(resp)
}

func (s HttpStore) Erase(ctx context.Context, d Digest) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, s.url(d), nil)
	if err != nil {
		return err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return s.parseErr(resp)
}

// setLabels writes labels to req as Flob-<Key> headers.
func (HttpStore) setLabels(req *http.Request, labels Labels) {
	for key, values := range labels {
		for _, v := range values {
			req.Header.Add("Flob-"+key, v)
		}
	}
}

func (HttpStore) parseErr(res *http.Response) error {
	switch res.StatusCode {
	case http.StatusOK, http.StatusCreated, http.StatusNoContent, http.StatusPartialContent:
		return nil
	case http.StatusNotFound:
		return ErrNotExist
	case http.StatusUnprocessableEntity:
		return ErrDigestMismatch
	default:
		return fmt.Errorf("unexpected HTTP status: %s", res.Status)
	}
}

func (HttpStore) parseMeta(res *http.Response) Meta {
	m := Meta{Size: res.ContentLength}
	if etag := res.Header.Get("ETag"); etag != "" {
		m.Digest = Digest(strings.Trim(etag, `"`))
	}
	for key, values := range res.Header {
		if !meta_headers_to_skip[key] {
			if m.Labels == nil {
				m.Labels = make(Labels)
			}
			m.Labels[key] = values
		}
	}
	return m
}

// meta_headers_to_skip contains standard HTTP response headers that are not blob labels.
var meta_headers_to_skip = map[string]bool{
	"Etag":              true,
	"Content-Length":    true,
	"Content-Type":      true,
	"Transfer-Encoding": true,
	"Content-Encoding":  true,
	"Date":              true,
	"Server":            true,
	"Connection":        true,
	"Location":          true,
	"Cache-Control":     true,
	"Vary":              true,
	"Accept-Ranges":     true,
	"Content-Range":     true,
	"Last-Modified":     true,
}

// httpRangeReader streams one response at a time. Read and Seek are serialized;
// Close cancels requests and closes the active body without waiting for Read.
// Reopened responses must keep the same strong ETag. A server without one can
// serve a single sequential response but cannot safely support reopened reads.
// A 200 response is accepted only at offset zero; ignored nonzero ranges fail.
// The reader never buffers the blob or downloads a prefix to emulate seeking.
type httpRangeReader struct {
	ctx               context.Context
	cancel            context.CancelFunc
	fetch             func(context.Context, string, int64, string) (*http.Response, error)
	size              int64
	responseEnd       int64
	offset            int64
	etag              string
	headURL           string
	getURL            string
	representationURL string
	fetched           bool
	conditionalFirst  bool
	op                sync.Mutex
	mu                sync.Mutex // Only protects lifecycle state, never held during network I/O.
	body              io.ReadCloser
	closed            bool
	readErr           error
}

func newHTTPRangeReader(ctx context.Context, size int64, getURL, headURL, etag string, conditionalFirst bool, fetch func(context.Context, string, int64, string) (*http.Response, error)) *httpRangeReader {
	ctx, cancel := context.WithCancel(ctx)
	return &httpRangeReader{ctx: ctx, cancel: cancel, size: size, getURL: getURL, headURL: headURL, etag: strongETag(etag), conditionalFirst: conditionalFirst, fetch: fetch}
}
func strongETag(etag string) string {
	if len(etag) < 2 || etag[0] != '"' || etag[len(etag)-1] != '"' {
		return ""
	}
	for _, c := range []byte(etag[1 : len(etag)-1]) {
		if c < 0x21 || c == '"' || c == 0x7f {
			return ""
		}
	}
	return etag
}
func responseURL(resp *http.Response) string {
	if resp.Request != nil && resp.Request.URL != nil {
		return resp.Request.URL.String()
	}
	return ""
}

func identityResponse(resp *http.Response) bool {
	encoding := resp.Header.Get("Content-Encoding")
	return !resp.Uncompressed && (encoding == "" || strings.EqualFold(encoding, "identity"))
}
func (r *httpRangeReader) stateError() error {
	r.mu.Lock()
	closed := r.closed
	r.mu.Unlock()
	if closed {
		return os.ErrClosed
	}
	return r.ctx.Err()
}
func (r *httpRangeReader) closeBody() {
	r.mu.Lock()
	body := r.body
	r.body = nil
	r.mu.Unlock()
	if body != nil {
		body.Close()
	}
}
func (r *httpRangeReader) openResponse() error {
	if r.fetched && r.etag == "" {
		return errors.New("cannot reopen blob without a strong ETag")
	}
	// Pin the first validated representation URL. Redirect endpoints may mint
	// different signed URLs on each request; their ETags need not match the
	// final object's validator. Later requests go directly to the pinned URL.
	target := r.getURL
	conditional := ""
	if r.fetched {
		target = r.representationURL
		conditional = r.etag
	} else if r.conditionalFirst {
		conditional = r.etag
	}
	resp, err := r.fetch(r.ctx, target, r.offset, conditional)
	if err != nil {
		return err
	}
	accepted := false
	defer func() {
		if !accepted {
			resp.Body.Close()
		}
	}()
	if resp.StatusCode == http.StatusNotFound {
		return ErrNotExist
	}
	if !identityResponse(resp) {
		return errors.New("encoded blob response cannot be ranged")
	}
	end := r.size - 1
	switch resp.StatusCode {
	case http.StatusPartialContent:
		start, last, total, ok := parseContentRange(resp.Header.Get("Content-Range"))
		if !ok || start != r.offset || last >= r.size || total != r.size {
			return fmt.Errorf("invalid Content-Range %q for offset %d and size %d", resp.Header.Get("Content-Range"), r.offset, r.size)
		}
		end = last
	case http.StatusOK:
		if r.offset != 0 {
			return errors.New("server ignored a nonzero blob range")
		}
		if resp.Header.Get("Content-Range") != "" {
			return errors.New("unexpected Content-Range on full blob response")
		}
	default:
		return fmt.Errorf("unexpected ranged GET status: %d", resp.StatusCode)
	}
	expected := end - r.offset + 1
	if resp.ContentLength >= 0 && resp.ContentLength != expected {
		return fmt.Errorf("ranged GET length %d, want %d", resp.ContentLength, expected)
	}
	etag := strongETag(resp.Header.Get("ETag"))
	url := responseURL(resp)
	if r.fetched && url != r.representationURL {
		return errors.New("blob response redirected to a different resource during read")
	}
	if !r.fetched && r.conditionalFirst && url != r.headURL {
		return errors.New("blob GET resource differs from its HEAD response")
	}
	if r.fetched || r.conditionalFirst || (r.headURL != "" && url == r.headURL) {
		if r.etag != "" && etag != r.etag {
			return errors.New("blob representation changed during read")
		}
	}
	if !r.fetched {
		r.etag = etag
		r.representationURL = url
		r.fetched = true
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return os.ErrClosed
	}
	if err := r.ctx.Err(); err != nil {
		r.mu.Unlock()
		return err
	}
	r.body = resp.Body
	r.responseEnd = end
	r.mu.Unlock()
	accepted = true
	return nil
}
func parseContentRange(value string) (start, end, total int64, ok bool) {
	bounds, size, found := strings.Cut(strings.TrimPrefix(value, "bytes "), "/")
	if !found || !strings.HasPrefix(value, "bytes ") {
		return
	}
	first, last, found := strings.Cut(bounds, "-")
	if !found {
		return
	}
	parse := func(v string) (int64, error) {
		if v == "" {
			return 0, errors.New("empty range")
		}
		for _, c := range v {
			if c < '0' || c > '9' {
				return 0, errors.New("invalid range")
			}
		}
		return strconv.ParseInt(v, 10, 64)
	}
	var err error
	if start, err = parse(first); err != nil {
		return
	}
	if end, err = parse(last); err != nil {
		return
	}
	if total, err = parse(size); err != nil {
		return
	}
	ok = start <= end && end < total
	return
}
func (r *httpRangeReader) Read(p []byte) (int, error) {
	r.op.Lock()
	defer r.op.Unlock()
	if err := r.stateError(); err != nil {
		return 0, err
	}
	if len(p) == 0 {
		return 0, nil
	}
	if r.readErr != nil {
		return 0, r.readErr
	}
	if r.offset >= r.size {
		return 0, io.EOF
	}
	r.mu.Lock()
	body := r.body
	r.mu.Unlock()
	if body == nil {
		if err := r.openResponse(); err != nil {
			return 0, err
		}
		r.mu.Lock()
		body = r.body
		r.mu.Unlock()
		if body == nil {
			return 0, os.ErrClosed
		}
	}
	if int64(len(p)) > r.responseEnd-r.offset+1 {
		p = p[:int(r.responseEnd-r.offset+1)]
	}
	n, err := body.Read(p)
	r.offset += int64(n)
	if contextErr := r.stateError(); contextErr != nil {
		err = contextErr
	}
	if err == io.EOF && r.offset <= r.responseEnd {
		err = io.ErrUnexpectedEOF
	}
	if r.offset == r.responseEnd+1 && (err == nil || err == io.EOF) {
		// Content-Length may be absent (chunked response). Probe the boundary so
		// malformed overlong responses cannot silently pass as a complete blob.
		if err == nil {
			var extra [1]byte
			count, tailErr := io.ReadFull(body, extra[:])
			if count != 0 {
				err = errors.New("ranged GET exceeded advertised blob size")
			} else if tailErr != io.EOF {
				err = tailErr
			}
		}
		if err == io.EOF {
			err = nil
		}
		r.closeBody()
	}
	if contextErr := r.stateError(); contextErr != nil {
		err = contextErr
	}
	if err != nil {
		r.readErr = err
		r.closeBody()
	}
	return n, err
}
func (r *httpRangeReader) Seek(offset int64, whence int) (int64, error) {
	r.op.Lock()
	defer r.op.Unlock()
	if err := r.stateError(); err != nil {
		return 0, err
	}
	base := int64(0)
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		base = r.offset
	case io.SeekEnd:
		base = r.size
	default:
		return 0, errors.New("invalid seek whence")
	}
	if offset < -base || offset > math.MaxInt64-base {
		return 0, errors.New("invalid seek offset")
	}
	next := base + offset
	if next != r.offset {
		r.closeBody()
		r.offset = next
		r.readErr = nil
	}
	return next, nil
}
func (r *httpRangeReader) Close() error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	body := r.body
	r.body = nil
	r.mu.Unlock()
	r.cancel()
	if body != nil {
		return body.Close()
	}
	return nil
}
