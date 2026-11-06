package httpremote

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestRedirectMustNotRestoreCredentials(t *testing.T) {
	var auth, cookie, proxyAuth string
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/alias" {
			http.Redirect(w, r, "/blob", http.StatusTemporaryRedirect)
			return
		}
		auth, cookie = r.Header.Get("Authorization"), r.Header.Get("Cookie")
		proxyAuth = r.Header.Get("Proxy-Authorization")
		io.WriteString(w, "ok")
	}))
	defer second.Close()
	first := unavailable(t)
	c, err := NewClient(nil, Config{Targets: []string{first, second.URL}})
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("GET", first+"/alias", nil)
	req.Header.Set("Authorization", "Bearer primary-secret")
	req.Header.Set("Cookie", "session=primary-secret")
	req.Header["proxy-authorization"] = []string{"lowercase-secret"}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if auth != "" || cookie != "" || proxyAuth != "" {
		t.Fatalf("fallback redirect received original credentials: Authorization=%q Cookie=%q Proxy-Authorization=%q", auth, cookie, proxyAuth)
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestFallbackCookiesBelongToFallback(t *testing.T) {
	jar, _ := cookiejar.New(nil)
	base := &http.Client{Jar: jar, Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "primary.invalid" {
			return nil, errors.New("connection failed")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Set-Cookie": []string{"session=fallback-token; Path=/"}}, Body: io.NopCloser(strings.NewReader("ok")), Request: r}, nil
	})}
	c, err := NewClient(base, Config{Targets: []string{"https://primary.invalid", "https://fallback.invalid"}})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Get("https://primary.invalid/blob")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	primary, _ := url.Parse("https://primary.invalid")
	fallback, _ := url.Parse("https://fallback.invalid")
	if len(jar.Cookies(primary)) != 0 || len(jar.Cookies(fallback)) != 1 {
		t.Fatalf("cookies stored under wrong origin: primary=%v fallback=%v", jar.Cookies(primary), jar.Cookies(fallback))
	}
}

func TestOriginAuthenticationAcrossFailoverAndRedirect(t *testing.T) {
	const primary = "https://origin.invalid:8443"
	const fallback = "https://origin.invalid:9443"
	const public = "http://origin.invalid:9443"
	var calls []string
	base := &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		calls = append(calls, r.URL.String()+" "+r.Header.Get("Authorization"))
		switch r.URL.String() {
		case primary + "/blob":
			return nil, errors.New("connection failed")
		case fallback + "/blob":
			return &http.Response{StatusCode: 307, Header: http.Header{"Location": {public + "/blob"}}, Body: http.NoBody, Request: r}, nil
		default:
			if r.Header.Get("Cookie") != "" || r.Host != "" {
				t.Fatalf("original credentials or Host reached public origin: %v", r)
			}
			return &http.Response{StatusCode: 200, Header: http.Header{}, Body: http.NoBody, Request: r}, nil
		}
	})}
	tokens := map[string]string{primary: "primary-token", fallback: "fallback-token"}
	c, err := NewClient(base, Config{Targets: []string{primary, fallback}, BearerTokens: tokens})
	if err != nil {
		t.Fatal(err)
	}
	tokens[fallback] = "changed-after-construction"
	r, _ := http.NewRequest("GET", primary+"/blob", nil)
	r.Header.Set("Authorization", "Bearer caller-token")
	r.Header.Set("Cookie", "caller=secret")
	r.Host = "original-host.invalid"
	resp, err := c.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	want := []string{primary + "/blob Bearer primary-token", fallback + "/blob Bearer fallback-token", public + "/blob "}
	if strings.Join(calls, "\n") != strings.Join(want, "\n") {
		t.Fatalf("requests = %q, want %q", calls, want)
	}
	if r.Header.Get("Authorization") != "Bearer caller-token" || r.Header.Get("Cookie") != "caller=secret" || r.Host != "original-host.invalid" || r.URL.String() != primary+"/blob" {
		t.Fatalf("caller request was changed: %v", r)
	}
}

func TestRedirectReturnDoesNotRestoreOriginalCredentials(t *testing.T) {
	base := &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		location := ""
		switch r.URL.String() {
		case "https://a.invalid/start":
			location = "https://a.invalid:8443/next"
		case "https://a.invalid:8443/next":
			location = "https://a.invalid/end"
		}
		if r.URL.Path != "/start" && (r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "") {
			t.Fatalf("credentials restored at %s", r.URL)
		}
		status := 200
		if location != "" {
			status = 307
		}
		return &http.Response{StatusCode: status, Header: http.Header{"Location": {location}}, Body: http.NoBody, Request: r}, nil
	})}
	c, err := NewClient(base, Config{Targets: []string{"https://a.invalid"}})
	if err != nil {
		t.Fatal(err)
	}
	r, _ := http.NewRequest("GET", "https://a.invalid/start", nil)
	r.Header.Set("Authorization", "Bearer secret")
	r.Header.Set("Cookie", "session=secret")
	resp, err := c.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

func TestFallbackUsesItsCookieJar(t *testing.T) {
	jar, _ := cookiejar.New(nil)
	fallback, _ := url.Parse("https://fallback.invalid")
	jar.SetCookies(fallback, []*http.Cookie{{Name: "session", Value: "fallback", Path: "/"}})
	base := &http.Client{Jar: jar, Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "primary.invalid" {
			return nil, errors.New("connection failed")
		}
		if r.Header.Get("Cookie") != "session=fallback" {
			t.Fatalf("fallback cookies = %q", r.Header.Get("Cookie"))
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: http.NoBody, Request: r}, nil
	})}
	c, err := NewClient(base, Config{Targets: []string{"https://primary.invalid", fallback.String()}})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Get("https://primary.invalid/blob")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

func TestRedirectFailuresDoNotRestartFailover(t *testing.T) {
	for _, policyError := range []bool{false, true} {
		t.Run(fmt.Sprint(policyError), func(t *testing.T) {
			calls := 0
			base := &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.Host == "fallback.invalid" {
					t.Fatal("redirect failure retried at fallback")
				}
				if r.URL.Path == "/next" {
					return nil, errors.New("redirect transport failed")
				}
				return &http.Response{StatusCode: 307, Header: http.Header{"Location": {"/next"}}, Body: http.NoBody, Request: r}, nil
			})}
			if policyError {
				base.CheckRedirect = func(*http.Request, []*http.Request) error { return errors.New("policy denied") }
			}
			c, err := NewClient(base, Config{Targets: []string{"https://primary.invalid", "https://fallback.invalid"}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := c.Get("https://primary.invalid/blob"); err == nil {
				t.Fatal("expected redirect failure")
			}
			want := 2
			if policyError {
				want = 1
			}
			if calls != want {
				t.Fatalf("calls = %d, want %d", calls, want)
			}
		})
	}
}

func TestTimeoutBoundsAllAttemptsAndBody(t *testing.T) {
	var firstDeadline time.Time
	base := &http.Client{Timeout: time.Second, Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		deadline, ok := r.Context().Deadline()
		if !ok {
			t.Fatal("no request deadline")
		}
		if r.URL.Host == "primary.invalid" {
			firstDeadline = deadline
			return nil, errors.New("connection failed")
		}
		if !deadline.Equal(firstDeadline) {
			t.Fatal("timeout restarted at fallback")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("ok")), Request: r}, nil
	})}
	c, err := NewClient(base, Config{Targets: []string{"https://primary.invalid", "https://fallback.invalid"}})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Get("https://primary.invalid/blob")
	if err != nil {
		t.Fatal(err)
	}
	if err := resp.Request.Context().Err(); err != nil {
		t.Fatalf("context canceled before reading body: %v", err)
	}
	if _, err := io.ReadAll(resp.Body); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.Request.Context().Err() == nil {
		t.Fatal("timeout context not released after body read")
	}
}

func TestInvalidAuthenticationDoesNotExposeTokens(t *testing.T) {
	for _, auth := range []map[string]string{
		{"https://origin.invalid/path": "secret"},
		{"https://origin.invalid?query=secret": "secret"},
		{"https://user:secret@origin.invalid": "secret"},
		{"https://origin.invalid": "secret\ninvalid"},
		{"https://origin.invalid": ""},
		{"https://origin.invalid": "one", "https://ORIGIN.invalid:443/": "two"},
	} {
		_, err := NewClient(nil, Config{Targets: []string{"https://origin.invalid"}, BearerTokens: auth})
		if err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatalf("invalid configuration error = %v", err)
		}
	}
}

func TestTransportDecorationKeepsDefaultTimeouts(t *testing.T) {
	calls := 0
	_, err := NewClient(nil, Config{Targets: []string{"https://origin.invalid"}, WrapTransport: func(base http.RoundTripper) http.RoundTripper {
		calls++
		transport, ok := base.(*http.Transport)
		if !ok || transport.ResponseHeaderTimeout <= 0 || transport.TLSHandshakeTimeout <= 0 {
			t.Fatalf("unbounded default transport: %v", base)
		}
		return base
	}})
	if err != nil || calls != 1 {
		t.Fatalf("transport decoration: calls=%d error=%v", calls, err)
	}
}
