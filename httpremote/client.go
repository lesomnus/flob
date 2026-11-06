// Package httpremote provides ordered HTTP origins for read-only downloads.
package httpremote

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// NewClient copies client and retries GET/HEAD against the next target only
// when the HTTP transport fails. Every HTTP response, including 404 and 5xx,
// is authoritative. Writes, canceled requests and pinned If-Match reads are
// never retried. No response body is buffered or replayed.
//
// Targets are base URLs, in priority order. Requests outside these bases pass
// through unchanged. Credentials are stripped when changing origins. After a
// reader has pinned a response URL, it does not switch back to an earlier base.
func NewClient(client *http.Client, targets []string) (*http.Client, error) {
	if len(targets) == 0 {
		return nil, fmt.Errorf("at least one HTTP target is required")
	}
	bases := make([]*url.URL, 0, len(targets))
	seen := map[string]bool{}
	for i, raw := range targets {
		u, err := url.Parse(strings.TrimRight(raw, "/"))
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") ||
			u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return nil, fmt.Errorf("invalid HTTP target at index %d", i)
		}
		if seen[u.String()] {
			return nil, fmt.Errorf("duplicate HTTP target %q", raw)
		}
		seen[u.String()] = true
		bases = append(bases, u)
	}
	c := *http.DefaultClient
	if client != nil {
		c = *client
	}
	base := c.Transport
	if base == nil {
		t := http.DefaultTransport.(*http.Transport).Clone()
		t.DialContext = (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext
		t.TLSHandshakeTimeout = 5 * time.Second
		t.ResponseHeaderTimeout = 15 * time.Second
		base = t
	}
	c.Transport = &transport{base: base, targets: bases}
	return &c, nil
}

type transport struct {
	base    http.RoundTripper
	targets []*url.URL
}

func (t *transport) RoundTrip(r *http.Request) (*http.Response, error) {
	if (r.Method != http.MethodGet && r.Method != http.MethodHead) ||
		(r.Body != nil && r.Body != http.NoBody) || r.Header.Get("If-Match") != "" {
		return t.base.RoundTrip(r)
	}
	start := -1
	var suffix, escaped string
	for i, u := range t.targets {
		if r.URL.Scheme != u.Scheme || r.URL.Host != u.Host {
			continue
		}
		prefix := u.EscapedPath()
		path := r.URL.EscapedPath()
		if path != prefix && !strings.HasPrefix(path, prefix+"/") {
			continue
		}
		start = i
		suffix = strings.TrimPrefix(r.URL.Path, u.Path)
		escaped = strings.TrimPrefix(path, prefix)
		break
	}
	if start < 0 {
		return t.base.RoundTrip(r)
	}
	var last error
	for _, target := range t.targets[start:] {
		if err := r.Context().Err(); err != nil {
			return nil, err
		}
		req := r.Clone(r.Context())
		u := *r.URL
		u.Scheme, u.Host, u.User = target.Scheme, target.Host, nil
		u.Path, u.RawPath = target.Path+suffix, target.EscapedPath()+escaped
		req.URL = &u
		if u.Scheme != r.URL.Scheme || u.Host != r.URL.Host {
			req.Host = ""
			req.Header.Del("Authorization")
			req.Header.Del("Proxy-Authorization")
			req.Header.Del("Cookie")
		}
		resp, err := t.base.RoundTrip(req)
		if err == nil {
			resp.Request = req
			// net/http resolves redirects against the logical original request.
			// Preserve the actual origin for a relative fallback redirect.
			if location := resp.Header.Get("Location"); location != "" {
				if loc, e := req.URL.Parse(location); e == nil {
					resp.Header.Set("Location", loc.String())
				}
			}
			return resp, nil
		}
		if resp != nil && resp.Body != nil {
			resp.Body.Close()
		}
		last = err
	}
	return nil, last
}
