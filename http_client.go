package flob

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// httpOriginClient selects an origin before invoking http.Client.Do, so cookies
// and redirects use the actual request URL. It is safe for concurrent use.
type httpOriginClient struct {
	client  http.Client
	targets []*url.URL
	auth    map[string]HTTPAuth
}

// HttpConfig configures an HTTP blob backend. Targets are ordered base URLs;
// Auth maps exact origins (scheme, hostname and effective port; no path) to
// credentials. An origin without an entry receives no configured Authorization.
type HttpConfig struct {
	// Client is copied; nil uses the default HTTP client. Its Timeout covers
	// all failover attempts and reading the response body.
	Client *http.Client
	// Targets contains at least one HTTP(S) base URL. Reads may fail over in
	// this order; writes start at the first target.
	Targets []string
	Auth    map[string]HTTPAuth
	// WrapTransport decorates the selected transport once, after the default
	// dial, TLS and response-header timeouts have been configured.
	WrapTransport func(http.RoundTripper) http.RoundTripper
}

// newHTTPOriginClient selects an actual URL before http.Client.Do, so only
// GET/HEAD transport failures before any response may advance to the next base.
// Requests outside these bases, writes and If-Match reads do not fail over.
// Configure the supplied HTTP client before calling NewHttpStores.
func newHTTPOriginClient(config HttpConfig) (*httpOriginClient, error) {
	if len(config.Targets) == 0 {
		return nil, fmt.Errorf("at least one HTTP target is required")
	}
	c := &httpOriginClient{client: *http.DefaultClient, auth: map[string]HTTPAuth{}}
	seen := map[string]bool{}
	for i, raw := range config.Targets {
		u, err := url.Parse(raw)
		if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") ||
			u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
			return nil, fmt.Errorf("invalid HTTP target at index %d", i)
		}
		u.RawPath = strings.TrimRight(u.EscapedPath(), "/")
		u.Path, _ = url.PathUnescape(u.RawPath)
		key := httpOrigin(u) + u.EscapedPath()
		if seen[key] {
			return nil, fmt.Errorf("duplicate HTTP target at index %d", i)
		}
		seen[key] = true
		c.targets = append(c.targets, u)
	}
	for raw, auth := range config.Auth {
		u, err := url.Parse(raw)
		if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") ||
			u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
			return nil, fmt.Errorf("authentication requires an HTTP origin without a path, query or userinfo")
		}
		key := httpOrigin(u)
		if _, exists := c.auth[key]; exists {
			return nil, fmt.Errorf("duplicate authentication origin %s", key)
		}
		if auth.Kind != "bearer" && auth.Kind != "basic" {
			return nil, fmt.Errorf("unsupported authentication kind %q for origin %s", auth.Kind, key)
		}
		if auth.Value == nil {
			return nil, fmt.Errorf("authentication value is required for origin %s", key)
		}
		c.auth[key] = auth
	}
	if config.Client != nil {
		c.client = *config.Client
	}
	if c.client.Transport == nil {
		c.client.Transport = http.DefaultTransport
		if base, ok := http.DefaultTransport.(*http.Transport); ok {
			t := base.Clone()
			t.DialContext = (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext
			t.TLSHandshakeTimeout = 5 * time.Second
			t.ResponseHeaderTimeout = 15 * time.Second
			c.client.Transport = t
		}
	}
	if config.WrapTransport != nil {
		c.client.Transport = config.WrapTransport(c.client.Transport)
		if c.client.Transport == nil {
			return nil, fmt.Errorf("transport wrapper returned nil")
		}
	}
	checkRedirect := c.client.CheckRedirect
	c.client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		// net/http may restore initial credentials on another port or subdomain.
		// Once a redirect crosses an origin, also strip them on any return hop.
		for _, previous := range via {
			if httpOrigin(previous.URL) != httpOrigin(req.URL) {
				stripHTTPCredentials(req)
				break
			}
		}
		if checkRedirect != nil {
			return checkRedirect(req, via)
		}
		if len(via) >= 10 {
			return fmt.Errorf("stopped after 10 redirects")
		}
		return nil
	}
	return c, nil
}

// Do streams a response without modifying r. The supplied client's Timeout
// bounds all attempts and reading the response body. HTTP responses and
// redirect errors are terminal; interrupted bodies are never retried.
func (c *httpOriginClient) Do(r *http.Request) (*http.Response, error) {
	if r == nil || r.URL == nil {
		return nil, fmt.Errorf("an HTTP request URL is required")
	}
	if c.client.Timeout <= 0 {
		return c.do(r)
	}
	ctx, cancel := context.WithTimeout(r.Context(), c.client.Timeout)
	resp, err := c.do(r.Clone(ctx))
	if err != nil || resp.Body == nil {
		cancel()
	} else {
		resp.Body = &httpCancelBody{ReadCloser: resp.Body, cancel: cancel}
	}
	return resp, err
}

func (c *httpOriginClient) do(r *http.Request) (*http.Response, error) {
	start := -1
	var suffix string
	if (r.Method == http.MethodGet || r.Method == http.MethodHead) &&
		(r.Body == nil || r.Body == http.NoBody) && r.Header.Get("If-Match") == "" {
		for i, base := range c.targets {
			if httpOrigin(r.URL) != httpOrigin(base) {
				continue
			}
			prefix, path := base.EscapedPath(), r.URL.EscapedPath()
			if path == prefix || strings.HasPrefix(path, prefix+"/") {
				start, suffix = i, strings.TrimPrefix(path, prefix)
				break
			}
		}
	}
	targets := []*url.URL{r.URL}
	if start >= 0 {
		targets = c.targets[start:]
	}
	var last error
	for _, target := range targets {
		req := r.Clone(r.Context())
		if start >= 0 {
			u := *r.URL
			u.Scheme, u.Host = target.Scheme, target.Host
			u.RawPath = target.EscapedPath() + suffix
			u.Path, _ = url.PathUnescape(u.RawPath)
			req.URL = &u
			if httpOrigin(req.URL) != httpOrigin(r.URL) {
				stripHTTPCredentials(req)
			}
		}
		// A response followed by a redirect failure must not restart the download.
		attempt := &httpAttemptTransport{base: c.client.Transport, auth: c.auth}
		h := c.client
		h.Timeout = 0 // Do owns the deadline across all attempts and the body.
		h.Transport = attempt
		resp, err := h.Do(req)
		if err == nil || attempt.responded || !attempt.failed || r.Context().Err() != nil {
			return resp, err
		}
		last = err
	}
	return nil, last
}

func stripHTTPCredentials(r *http.Request) {
	r.Host, r.URL.User = "", nil
	removeHTTPHeader(r.Header, "Authorization")
	removeHTTPHeader(r.Header, "Proxy-Authorization")
	removeHTTPHeader(r.Header, "Cookie")
}

func removeHTTPHeader(header http.Header, name string) {
	for key := range header {
		if strings.EqualFold(key, name) {
			delete(header, key)
		}
	}
}

func httpOrigin(u *url.URL) string {
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	return strings.ToLower(u.Scheme) + "://" + net.JoinHostPort(strings.ToLower(u.Hostname()), port)
}

type httpAttemptTransport struct {
	base      http.RoundTripper
	auth      map[string]HTTPAuth
	responded bool
	failed    bool
}

func (t *httpAttemptTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if auth, ok := t.auth[httpOrigin(r.URL)]; ok {
		header, err := auth.header()
		if err != nil {
			return nil, fmt.Errorf("authentication for origin %s: %w", httpOrigin(r.URL), err)
		}
		r = r.Clone(r.Context())
		removeHTTPHeader(r.Header, "Authorization")
		r.Header.Set("Authorization", header)
	}
	resp, err := t.base.RoundTrip(r)
	t.responded = t.responded || resp != nil
	t.failed = t.failed || err != nil
	if err != nil && resp != nil && resp.Body != nil {
		resp.Body.Close()
	}
	return resp, err
}

type httpCancelBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *httpCancelBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil {
		b.cancel()
	}
	return n, err
}

func (b *httpCancelBody) Close() error {
	err := b.ReadCloser.Close()
	b.cancel()
	return err
}
