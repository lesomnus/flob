// Package httpremote provides ordered HTTP origins for read-only downloads.
package httpremote

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

// Client selects an origin before invoking http.Client.Do, so cookies and
// redirects use the actual request URL. It is safe for concurrent use.
type Client struct {
	client  http.Client
	targets []*url.URL
	tokens  map[string]string
}

// Config separates ordered download bases from authentication. BearerTokens
// maps exact origins (scheme, hostname and effective port; no path) to tokens.
// An origin without an entry receives no configured Authorization header.
type Config struct {
	Targets      []string
	BearerTokens map[string]string
}

// NewClient copies client and configures ordered base URLs. Only GET/HEAD
// transport failures before any HTTP response may advance to the next base.
// Requests outside these bases, writes and If-Match reads do not fail over.
// Configure the supplied HTTP client before calling NewClient.
func NewClient(client *http.Client, config Config) (*Client, error) {
	if len(config.Targets) == 0 {
		return nil, fmt.Errorf("at least one HTTP target is required")
	}
	c := &Client{client: *http.DefaultClient, tokens: map[string]string{}}
	seen := map[string]bool{}
	for i, raw := range config.Targets {
		u, err := url.Parse(raw)
		if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") ||
			u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
			return nil, fmt.Errorf("invalid HTTP target at index %d", i)
		}
		u.RawPath = strings.TrimRight(u.EscapedPath(), "/")
		u.Path, _ = url.PathUnescape(u.RawPath)
		key := origin(u) + u.EscapedPath()
		if seen[key] {
			return nil, fmt.Errorf("duplicate HTTP target at index %d", i)
		}
		seen[key] = true
		c.targets = append(c.targets, u)
	}
	for raw, token := range config.BearerTokens {
		u, err := url.Parse(raw)
		if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") ||
			u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
			return nil, fmt.Errorf("bearer authentication requires an HTTP origin without a path, query or userinfo")
		}
		key := origin(u)
		if _, exists := c.tokens[key]; exists {
			return nil, fmt.Errorf("duplicate bearer authentication origin %s", key)
		}
		if token == "" || strings.IndexFunc(token, func(r rune) bool { return r <= ' ' || r >= 127 }) >= 0 {
			return nil, fmt.Errorf("invalid bearer token for origin %s", key)
		}
		c.tokens[key] = token
	}
	if client != nil {
		c.client = *client
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
	checkRedirect := c.client.CheckRedirect
	c.client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		// net/http may restore initial credentials on another port or subdomain.
		// Once a redirect crosses an origin, also strip them on any return hop.
		for _, previous := range via {
			if origin(previous.URL) != origin(req.URL) {
				stripCredentials(req)
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
func (c *Client) Do(r *http.Request) (*http.Response, error) {
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
		resp.Body = &cancelBody{ReadCloser: resp.Body, cancel: cancel}
	}
	return resp, err
}

func (c *Client) do(r *http.Request) (*http.Response, error) {
	start := -1
	var suffix string
	if (r.Method == http.MethodGet || r.Method == http.MethodHead) &&
		(r.Body == nil || r.Body == http.NoBody) && r.Header.Get("If-Match") == "" {
		for i, base := range c.targets {
			if origin(r.URL) != origin(base) {
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
			if origin(req.URL) != origin(r.URL) {
				stripCredentials(req)
			}
		}
		// A response followed by a redirect failure must not restart the download.
		attempt := &attemptTransport{base: c.client.Transport, tokens: c.tokens}
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

// Get sends a GET request through Do.
func (c *Client) Get(rawURL string) (*http.Response, error) {
	r, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	return c.Do(r)
}

// Head sends a HEAD request through Do.
func (c *Client) Head(rawURL string) (*http.Response, error) {
	r, err := http.NewRequest(http.MethodHead, rawURL, nil)
	if err != nil {
		return nil, err
	}
	return c.Do(r)
}

func stripCredentials(r *http.Request) {
	r.Host, r.URL.User = "", nil
	r.Header.Del("Authorization")
	r.Header.Del("Proxy-Authorization")
	r.Header.Del("Cookie")
}

func origin(u *url.URL) string {
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

type attemptTransport struct {
	base      http.RoundTripper
	tokens    map[string]string
	responded bool
	failed    bool
}

func (t *attemptTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if token, ok := t.tokens[origin(r.URL)]; ok {
		r = r.Clone(r.Context())
		r.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := t.base.RoundTrip(r)
	t.responded = t.responded || resp != nil
	t.failed = t.failed || err != nil
	if err != nil && resp != nil && resp.Body != nil {
		resp.Body.Close()
	}
	return resp, err
}

type cancelBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *cancelBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil {
		b.cancel()
	}
	return n, err
}

func (b *cancelBody) Close() error {
	err := b.ReadCloser.Close()
	b.cancel()
	return err
}
