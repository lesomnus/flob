package httpremote

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

func unavailable(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := "http://" + l.Addr().String()
	l.Close()
	return addr
}

func TestConnectionFailureUsesNextOrigin(t *testing.T) {
	var path, authorization, cookie string
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, authorization, cookie = r.URL.EscapedPath(), r.Header.Get("Authorization"), r.Header.Get("Cookie")
		io.WriteString(w, "blob bytes")
	}))
	defer second.Close()
	first := unavailable(t) + "/primary"
	c, err := NewClient(nil, Config{Targets: []string{first, second.URL + "/fallback"}})
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

func TestHTTPResponsesAreTerminal(t *testing.T) {
	for _, status := range []int{200, 401, 403, 404, 429, 500, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			calls := 0
			first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) }))
			defer first.Close()
			second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(200) }))
			defer second.Close()
			c, _ := NewClient(nil, Config{Targets: []string{first.URL, second.URL}})
			resp, err := c.Get(first.URL + "/_/sha256:abc")
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

func TestWritesPinnedReadsAndCancellationDoNotFallback(t *testing.T) {
	calls := 0
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(200) }))
	defer second.Close()
	first := unavailable(t)
	c, _ := NewClient(nil, Config{Targets: []string{first, second.URL}})
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

func TestFallbackRelativeRedirectUsesFallbackOrigin(t *testing.T) {
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/alias" {
			http.Redirect(w, r, "/_/blob", 308)
			return
		}
		io.WriteString(w, "ok")
	}))
	defer second.Close()
	first := unavailable(t)
	c, _ := NewClient(nil, Config{Targets: []string{first, second.URL}})
	resp, err := c.Get(first + "/alias")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Request.URL.String() != second.URL+"/_/blob" {
		t.Fatal(resp.Request.URL)
	}
}

func TestInvalidTargets(t *testing.T) {
	for _, targets := range [][]string{nil, {"ftp://example.com"}, {"https://user:secret@example.com"}, {"https://example.com?token=secret"}, {"https://example.com", "https://example.com/"}} {
		if _, err := NewClient(nil, Config{Targets: targets}); err == nil {
			t.Fatalf("accepted %v", targets)
		}
	}
}
