package flob

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// HttpHandler servers [Stores] over HTTP.
//
//	  POST /{store-id}          - [Store.Add]
//	  POST /{store-id}/{digest} - [Store.Add] with pre-computed digest
//	  HEAD /{store-id}/{digest} - [Store.Get]
//	   GET /{store-id}/{digest} - [Store.Open]
//	 PATCH /{store-id}/{digest} - [Store.Label]
//	DELETE /{store-id}/{digest} - [Store.Erase]
//
// Headers with the "Flob-" prefix are saved as labels and returned in the response
// of GET and HEAD requests without the prefix.
type HttpHandler struct {
	Stores Stores
}

func (h HttpHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	id, digest_raw, ok := h.parsePath(r.URL.Path)
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
				http.Error(w, err.Error(), http.StatusConflict)
			case errors.Is(err, ErrDigestMismatch):
				http.Error(w, err.Error(), http.StatusUnprocessableEntity)
			default:
				http.Error(w, err.Error(), http.StatusInternalServerError)
			}
			return
		}
		h.setMetaHeaders(w, m)
		w.Header().Set("Location", "/"+id+"/"+string(m.Digest))
		w.WriteHeader(http.StatusCreated)

	case http.MethodHead:
		m, err := store.Get(r.Context(), d)
		if err != nil {
			switch {
			case errors.Is(err, ErrNotExist):
				http.Error(w, err.Error(), http.StatusNotFound)
			default:
				http.Error(w, err.Error(), http.StatusInternalServerError)
			}
			return
		}
		h.setMetaHeaders(w, m)
		w.WriteHeader(http.StatusOK)

	case http.MethodGet:
		rc, m, err := store.Open(r.Context(), d)
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
		h.setMetaHeaders(w, m)
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

// parsePath splits /{store-id} or /{store-id}/{digest} from the request path.
// Any deeper path returns ok=false.
func (h *HttpHandler) parsePath(path string) (storeID, digest string, ok bool) {
	parts := strings.SplitN(strings.TrimPrefix(path, "/"), "/", 3)
	switch len(parts) {
	case 1:
		if parts[0] == "" {
			return "", "", false
		}
		return parts[0], "", true
	case 2:
		return parts[0], parts[1], true
	default:
		return "", "", false
	}
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
		id:     id,
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
	if err := s.parseErr(res); err != nil {
		return m, err
	}
	return m, nil
}

func (s HttpStore) Get(ctx context.Context, d Digest) (Meta, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, s.url(d), nil)
	if err != nil {
		return Meta{}, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return Meta{}, err
	}
	defer resp.Body.Close()
	if err := s.parseErr(resp); err != nil {
		return Meta{}, err
	}
	return s.parseMeta(resp), nil
}

// Open downloads the blob content into memory to satisfy [io.ReadSeekCloser].
func (s HttpStore) Open(ctx context.Context, d Digest) (io.ReadSeekCloser, Meta, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.url(d), nil)
	if err != nil {
		return nil, Meta{}, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, Meta{}, err
	}
	if err := s.parseErr(resp); err != nil {
		resp.Body.Close()
		return nil, Meta{}, err
	}
	m := s.parseMeta(resp)
	data, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return nil, Meta{}, err
	}
	return nopCloser{bytes.NewReader(data)}, m, nil
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
	case http.StatusConflict:
		return ErrAlreadyExists
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
