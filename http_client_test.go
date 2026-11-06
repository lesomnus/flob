package flob

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func getHTTPOrigin(c *httpOriginClient, target string) (*http.Response, error) {
	r, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	return c.Do(r)
}

func unavailableHTTPOrigin(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := "http://" + l.Addr().String()
	l.Close()
	return addr
}

func TestHttpOriginConnectionFailureUsesNextOrigin(t *testing.T) {
	var path, authorization, cookie string
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, authorization, cookie = r.URL.EscapedPath(), r.Header.Get("Authorization"), r.Header.Get("Cookie")
		io.WriteString(w, "blob bytes")
	}))
	defer second.Close()
	first := unavailableHTTPOrigin(t) + "/primary"
	c, err := newHTTPOriginClient(HttpConfig{Targets: []string{first, second.URL + "/fallback"}})
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("GET", first+"/_/sha256:abc", nil)
	req.Header.Set("Authorization", "Bearer first-origin-only")
	req.Header.Set("Cookie", "first-origin-only=1")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "blob bytes" || path != "/fallback/_/sha256:abc" || authorization != "" || cookie != "" || !strings.HasPrefix(resp.Request.URL.String(), second.URL) {
		t.Fatalf("fallback: body=%q path=%q auth=%q cookie=%q url=%s", body, path, authorization, cookie, resp.Request.URL)
	}
}

func TestHttpOriginHTTPResponsesAreTerminal(t *testing.T) {
	for _, status := range []int{200, 401, 403, 404, 429, 500, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			calls := 0
			first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) }))
			defer first.Close()
			second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(200) }))
			defer second.Close()
			c, _ := newHTTPOriginClient(HttpConfig{Targets: []string{first.URL, second.URL}})
			resp, err := getHTTPOrigin(c, first.URL+"/_/sha256:abc")
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != status || calls != 0 {
				t.Fatalf("status=%d fallback calls=%d", resp.StatusCode, calls)
			}
		})
	}
}

func TestHttpOriginWritesPinnedReadsAndCancellationDoNotFallback(t *testing.T) {
	calls := 0
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(200) }))
	defer second.Close()
	first := unavailableHTTPOrigin(t)
	c, _ := newHTTPOriginClient(HttpConfig{Targets: []string{first, second.URL}})
	for _, method := range []string{"POST", "PATCH", "DELETE", "GET"} {
		req, _ := http.NewRequest(method, first+"/_/sha256:abc", nil)
		if method == "GET" {
			req.Header.Set("If-Match", `"pinned"`)
		}
		if _, err := c.Do(req); err == nil {
			t.Fatal("request unexpectedly succeeded")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req, _ := http.NewRequestWithContext(ctx, "HEAD", first+"/_/sha256:abc", nil)
	if _, err := c.Do(req); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation=%v", err)
	}
	if calls != 0 {
		t.Fatalf("fallback calls=%d", calls)
	}
}

func TestHttpOriginFallbackRelativeRedirectUsesFallbackOrigin(t *testing.T) {
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/alias" {
			http.Redirect(w, r, "/_/blob", 308)
			return
		}
		io.WriteString(w, "ok")
	}))
	defer second.Close()
	first := unavailableHTTPOrigin(t)
	c, _ := newHTTPOriginClient(HttpConfig{Targets: []string{first, second.URL}})
	resp, err := getHTTPOrigin(c, first+"/alias")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Request.URL.String() != second.URL+"/_/blob" {
		t.Fatal(resp.Request.URL)
	}
}

func TestHttpOriginInvalidTargets(t *testing.T) {
	for _, targets := range [][]string{nil, {"ftp://example.com"}, {"https://user:secret@example.com"}, {"https://example.com?token=secret"}, {"https://example.com", "https://example.com/"}} {
		if _, err := NewHttpStores(HttpConfig{Targets: targets}); err == nil {
			t.Fatalf("accepted %v", targets)
		}
	}
}
