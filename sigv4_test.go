package flob

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// signatureOf extracts the Signature= value from an Authorization header.
func signatureOf(t *testing.T, auth string) string {
	t.Helper()
	for _, part := range strings.Split(auth, ",") {
		part = strings.TrimSpace(part)
		if v, ok := strings.CutPrefix(part, "Signature="); ok {
			return v
		}
	}
	t.Fatalf("no Signature in Authorization header: %q", auth)
	return ""
}

// The credentials and expected signatures below are AWS's own published
// Signature Version 4 examples for S3 ("Authenticating Requests: Using the
// Authorization Header"), so they pin the signer to the reference algorithm.
const (
	exAccessKey = "AKIAIOSFODNN7EXAMPLE"
	exSecretKey = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
)

func exampleSigner() signer {
	return signer{
		creds:   Credentials{AccessKeyID: exAccessKey, SecretAccessKey: exSecretKey},
		region:  "us-east-1",
		service: "s3",
		now:     func() time.Time { return time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC) },
	}
}

func TestSigV4GetObjectExample(t *testing.T) {
	// GET /test.txt with a Range header.
	req := &http.Request{
		Method: http.MethodGet,
		URL:    &url.URL{Scheme: "https", Host: "examplebucket.s3.amazonaws.com", Opaque: "/test.txt"},
		Header: http.Header{},
	}
	req.Header.Set("Range", "bytes=0-9")

	if err := exampleSigner().sign(req, emptyPayloadHash); err != nil {
		t.Fatal(err)
	}

	const want = "f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41"
	if got := signatureOf(t, req.Header.Get("Authorization")); got != want {
		t.Fatalf("signature mismatch:\n got=%s\nwant=%s\nauth=%s", got, want, req.Header.Get("Authorization"))
	}
	if got := req.Header.Get("X-Amz-Date"); got != "20130524T000000Z" {
		t.Fatalf("x-amz-date=%q", got)
	}
}

func TestSigV4PutObjectExample(t *testing.T) {
	// PUT /test$file.text with a Date and a storage-class header, body
	// "Welcome to Amazon S3.".
	const bodyHash = "44ce7dd67c959e0d3524ffac1771dfbba87d2b6b4b4e99e42034a8b803f8b072"
	req := &http.Request{
		Method: http.MethodPut,
		URL:    &url.URL{Scheme: "https", Host: "examplebucket.s3.amazonaws.com", Opaque: "/test%24file.text"},
		Header: http.Header{},
	}
	req.Header.Set("Date", "Fri, 24 May 2013 00:00:00 GMT")
	req.Header.Set("X-Amz-Storage-Class", "REDUCED_REDUNDANCY")

	if err := exampleSigner().sign(req, bodyHash); err != nil {
		t.Fatal(err)
	}

	const want = "98ad721746da40c64f1a55b78f14c238d841ea1380cd77a1b5971af0ece108bd"
	if got := signatureOf(t, req.Header.Get("Authorization")); got != want {
		t.Fatalf("signature mismatch:\n got=%s\nwant=%s\nauth=%s", got, want, req.Header.Get("Authorization"))
	}
}

func TestSigV4PresignGetObjectExample(t *testing.T) {
	// AWS's published "Query String Request Authentication" example: a presigned
	// GET of /test.txt on examplebucket, expiring in 86400s, dated 20130524.
	q, err := exampleSigner().presignQuery(context.Background(), http.MethodGet, "/test.txt", "examplebucket.s3.amazonaws.com", 86400*time.Second)
	if err != nil {
		t.Fatal(err)
	}

	var got string
	for _, part := range strings.Split(q, "&") {
		if v, ok := strings.CutPrefix(part, "X-Amz-Signature="); ok {
			got = v
		}
	}

	const want = "aeeed9bbccd4d02ee5c0109b86d86835f995330da4c265957d157751f604d404"
	if got != want {
		t.Fatalf("presign signature mismatch:\n got=%s\nwant=%s\nquery=%s", got, want, q)
	}
	// Sanity: the mandatory query parameters are present and signed-headers is host.
	for _, must := range []string{
		"X-Amz-Algorithm=AWS4-HMAC-SHA256",
		"X-Amz-Credential=AKIAIOSFODNN7EXAMPLE%2F20130524%2Fus-east-1%2Fs3%2Faws4_request",
		"X-Amz-Date=20130524T000000Z",
		"X-Amz-Expires=86400",
		"X-Amz-SignedHeaders=host",
	} {
		if !strings.Contains(q, must) {
			t.Errorf("presign query missing %q\nquery=%s", must, q)
		}
	}
}

func TestSigV4PresignClampsExpiry(t *testing.T) {
	s := exampleSigner()
	// Over the 7-day maximum is clamped; sub-second is raised to 1s.
	if q, _ := s.presignQuery(context.Background(), http.MethodGet, "/x", "h", 100*24*time.Hour); !strings.Contains(q, "X-Amz-Expires=604800") {
		t.Errorf("expiry not clamped to 604800: %s", q)
	}
	if q, _ := s.presignQuery(context.Background(), http.MethodGet, "/x", "h", 0); !strings.Contains(q, "X-Amz-Expires=1&") {
		t.Errorf("zero expiry not raised to 1: %s", q)
	}
}

func TestSigV4SessionTokenHeaderSigned(t *testing.T) {
	s := exampleSigner()
	s.creds = Credentials{AccessKeyID: exAccessKey, SecretAccessKey: exSecretKey, SessionToken: "FQoGZXIvYXdzEXAMPLETOKEN"}

	req := &http.Request{
		Method: http.MethodGet,
		URL:    &url.URL{Scheme: "https", Host: "examplebucket.s3.amazonaws.com", Opaque: "/test.txt"},
		Header: http.Header{},
	}
	if err := s.sign(req, emptyPayloadHash); err != nil {
		t.Fatal(err)
	}

	if req.Header.Get("X-Amz-Security-Token") == "" {
		t.Fatal("session token header not set")
	}
	if !strings.Contains(req.Header.Get("Authorization"), "x-amz-security-token") {
		t.Fatalf("session token not in signed headers: %s", req.Header.Get("Authorization"))
	}
}

func TestAwsURIEncode(t *testing.T) {
	cases := []struct {
		in          string
		encodeSlash bool
		want        string
	}{
		{"test.txt", false, "test.txt"},
		{"test$file.text", false, "test%24file.text"},
		{"a/b/c", false, "a/b/c"},
		{"a/b/c", true, "a%2Fb%2Fc"},
		{"key with space", true, "key%20with%20space"},
		{"tilde~_-.ok", true, "tilde~_-.ok"},
	}
	for _, c := range cases {
		if got := awsURIEncode(c.in, c.encodeSlash); got != c.want {
			t.Errorf("awsURIEncode(%q, %v)=%q want %q", c.in, c.encodeSlash, got, c.want)
		}
	}
}

// credentialOf extracts the access key id from an Authorization header's
// Credential= scope.
func credentialOf(t *testing.T, auth string) string {
	t.Helper()
	_, rest, ok := strings.Cut(auth, "Credential=")
	if !ok {
		t.Fatalf("no Credential in Authorization header: %q", auth)
	}
	id, _, _ := strings.Cut(rest, "/")
	return id
}

func newExampleRequest() *http.Request {
	return &http.Request{
		Method: http.MethodGet,
		URL:    &url.URL{Scheme: "https", Host: "examplebucket.s3.amazonaws.com", Opaque: "/test.txt"},
		Header: http.Header{},
	}
}

func TestSigV4ProviderMatchesFixedCredentials(t *testing.T) {
	// A provider giving the same set signs exactly as the fixed set does.
	fixed := newExampleRequest()
	if err := exampleSigner().sign(fixed, emptyPayloadHash); err != nil {
		t.Fatal(err)
	}

	s := exampleSigner()
	s.creds = CredentialsFunc(func(context.Context) (Credentials, error) {
		return Credentials{AccessKeyID: exAccessKey, SecretAccessKey: exSecretKey}, nil
	})
	provided := newExampleRequest()
	if err := s.sign(provided, emptyPayloadHash); err != nil {
		t.Fatal(err)
	}

	if got, want := provided.Header.Get("Authorization"), fixed.Header.Get("Authorization"); got != want {
		t.Fatalf("authorization differs:\n got=%s\nwant=%s", got, want)
	}
}

func TestSigV4AsksProviderAtEachSignature(t *testing.T) {
	sets := []Credentials{
		{AccessKeyID: "AKIDFIRST", SecretAccessKey: "first", SessionToken: "token-first"},
		{AccessKeyID: "AKIDSECOND", SecretAccessKey: "second", SessionToken: "token-second"},
	}
	calls := 0
	s := exampleSigner()
	s.creds = CredentialsFunc(func(context.Context) (Credentials, error) {
		c := sets[min(calls, len(sets)-1)]
		calls++
		return c, nil
	})

	first := newExampleRequest()
	if err := s.sign(first, emptyPayloadHash); err != nil {
		t.Fatal(err)
	}
	second := newExampleRequest()
	if err := s.sign(second, emptyPayloadHash); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("provider asked %d times; want 2", calls)
	}

	// Each request carries one whole set: its own key id and session token.
	for i, req := range []*http.Request{first, second} {
		if got := credentialOf(t, req.Header.Get("Authorization")); got != sets[i].AccessKeyID {
			t.Errorf("request %d signed by %q; want %q", i, got, sets[i].AccessKeyID)
		}
		if got := req.Header.Get("X-Amz-Security-Token"); got != sets[i].SessionToken {
			t.Errorf("request %d token %q; want %q", i, got, sets[i].SessionToken)
		}
	}
	if signatureOf(t, first.Header.Get("Authorization")) == signatureOf(t, second.Header.Get("Authorization")) {
		t.Error("both requests have the same signature despite different secrets")
	}

	// A presigned URL is signed with the set current at that moment.
	q, err := s.presignQuery(context.Background(), http.MethodGet, "/x", "h", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(q, "X-Amz-Credential=AKIDSECOND%2F") || !strings.Contains(q, "X-Amz-Security-Token=token-second") {
		t.Errorf("presign not signed with the current set: %s", q)
	}
}

func TestSigV4ProviderGetsRequestContext(t *testing.T) {
	type key struct{}
	var got any
	s := exampleSigner()
	s.creds = CredentialsFunc(func(ctx context.Context) (Credentials, error) {
		got = ctx.Value(key{})
		return Credentials{AccessKeyID: exAccessKey, SecretAccessKey: exSecretKey}, nil
	})

	req := newExampleRequest().WithContext(context.WithValue(context.Background(), key{}, "request"))
	if err := s.sign(req, emptyPayloadHash); err != nil {
		t.Fatal(err)
	}
	if got != "request" {
		t.Fatalf("provider saw %v; want the request's context", got)
	}

	got = nil
	if _, err := s.presignQuery(context.WithValue(context.Background(), key{}, "presign"), http.MethodGet, "/x", "h", time.Minute); err != nil {
		t.Fatal(err)
	}
	if got != "presign" {
		t.Fatalf("provider saw %v; want the presign context", got)
	}
}

func TestSigV4ProviderError(t *testing.T) {
	cause := errors.New("token file unreadable")
	s := exampleSigner()
	s.creds = CredentialsFunc(func(context.Context) (Credentials, error) {
		return Credentials{}, cause
	})

	req := newExampleRequest()
	if err := s.sign(req, emptyPayloadHash); !errors.Is(err, cause) {
		t.Fatalf("sign err = %v; want %v", err, cause)
	}
	if auth := req.Header.Get("Authorization"); auth != "" {
		t.Fatalf("request signed despite the error: %s", auth)
	}
	if _, err := s.presignQuery(context.Background(), http.MethodGet, "/x", "h", time.Minute); !errors.Is(err, cause) {
		t.Fatalf("presign err = %v; want %v", err, cause)
	}
}

func TestSigV4NilProviderIsZeroCredentials(t *testing.T) {
	zero := exampleSigner()
	zero.creds = Credentials{}
	want := newExampleRequest()
	if err := zero.sign(want, emptyPayloadHash); err != nil {
		t.Fatal(err)
	}

	s := exampleSigner()
	s.creds = nil
	got := newExampleRequest()
	if err := s.sign(got, emptyPayloadHash); err != nil {
		t.Fatal(err)
	}
	if got.Header.Get("Authorization") != want.Header.Get("Authorization") {
		t.Fatalf("nil provider signs unlike Credentials{}:\n got=%s\nwant=%s", got.Header.Get("Authorization"), want.Header.Get("Authorization"))
	}

	wantQ, _ := zero.presignQuery(context.Background(), http.MethodGet, "/x", "h", time.Minute)
	gotQ, err := s.presignQuery(context.Background(), http.MethodGet, "/x", "h", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if gotQ != wantQ {
		t.Fatalf("nil provider presigns unlike Credentials{}:\n got=%s\nwant=%s", gotQ, wantQ)
	}
}

func TestSigV4Expires(t *testing.T) {
	now := exampleSigner().now()
	withExpiry := func(expires time.Time) signer {
		s := exampleSigner()
		s.creds = Credentials{AccessKeyID: exAccessKey, SecretAccessKey: exSecretKey, Expires: expires}
		return s
	}

	t.Run("presign is cut to the credentials' lifetime", func(t *testing.T) {
		s := withExpiry(now.Add(90 * time.Second))
		q, err := s.presignQuery(context.Background(), http.MethodGet, "/x", "h", 15*time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(q, "X-Amz-Expires=90&") {
			t.Errorf("expiry not cut to 90s: %s", q)
		}
	})

	t.Run("a shorter ttl is kept", func(t *testing.T) {
		s := withExpiry(now.Add(time.Hour))
		q, err := s.presignQuery(context.Background(), http.MethodGet, "/x", "h", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(q, "X-Amz-Expires=60&") {
			t.Errorf("ttl not kept at 60s: %s", q)
		}
	})

	t.Run("expired credentials are refused", func(t *testing.T) {
		for _, expires := range []time.Time{now, now.Add(-time.Second)} {
			s := withExpiry(expires)
			req := newExampleRequest()
			if err := s.sign(req, emptyPayloadHash); err == nil || !strings.Contains(err.Error(), "expired") {
				t.Errorf("sign at expiry %s: err = %v; want expired", expires, err)
			}
			if _, err := s.presignQuery(context.Background(), http.MethodGet, "/x", "h", time.Minute); err == nil || !strings.Contains(err.Error(), "expired") {
				t.Errorf("presign at expiry %s: err = %v; want expired", expires, err)
			}
		}
	})

	t.Run("credentials not yet expired sign", func(t *testing.T) {
		s := withExpiry(now.Add(time.Second))
		if err := s.sign(newExampleRequest(), emptyPayloadHash); err != nil {
			t.Fatal(err)
		}
	})
}
