package flob

// AWS Signature Version 4 signing for S3, implemented against the published
// specification without depending on the AWS SDK.
//
// Reference: https://docs.aws.amazon.com/AmazonS3/latest/API/sig-v4-authenticating-requests.html
//
// Only the "Authorization header, single chunk" flavor is implemented, which is
// all the S3 store needs: every request body is either empty or fully buffered,
// so its SHA-256 is known before the request is sent, or is streamed as
// UNSIGNED-PAYLOAD with the service checking x-amz-checksum-sha256 instead.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// emptyPayloadHash is the SHA-256 of an empty body, used as x-amz-content-sha256
// for requests that carry no payload (HEAD, GET, DELETE, LIST, and the empty
// reference marker).
const emptyPayloadHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// Credentials holds the access key used to sign S3 requests. SessionToken is
// optional and only set for temporary (STS) credentials.
//
// Credentials is itself a [CredentialsProvider] that always gives itself, which
// is how a fixed set is configured.
type Credentials struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
	// Expires is when these credentials stop working; zero is never. A
	// signature is refused once it has passed, and a presigned URL is not made
	// to outlive it.
	Expires time.Time
}

// Retrieve implements [CredentialsProvider] by returning c.
func (c Credentials) Retrieve(context.Context) (Credentials, error) {
	return c, nil
}

// CredentialsProvider gives the credentials to sign with. It is asked before
// each signature, presigned URLs included, and from many goroutines at once, so
// it must be safe for concurrent use and should be cheap. The returned set is
// used whole for that one signature, so a provider that rotates keys never has
// a new key id mixed with an old secret. An error fails the request.
type CredentialsProvider interface {
	Retrieve(ctx context.Context) (Credentials, error)
}

// CredentialsFunc adapts a function to a [CredentialsProvider].
type CredentialsFunc func(ctx context.Context) (Credentials, error)

// Retrieve implements [CredentialsProvider] by calling f.
func (f CredentialsFunc) Retrieve(ctx context.Context) (Credentials, error) {
	return f(ctx)
}

// signer computes SigV4 signatures for a fixed region and service.
type signer struct {
	creds   CredentialsProvider // nil signs with Credentials{}
	region  string
	service string // always "s3" here
	now     func() time.Time
}

// headersNotSigned are request headers excluded from the signature. Content-Length
// is recomputed by the transport, and the rest are transport/client noise that
// AWS also omits by default; signing them would make the signature depend on
// values we do not control.
var headersNotSigned = map[string]bool{
	"authorization":   true,
	"content-length":  true,
	"user-agent":      true,
	"accept-encoding": true,
	"connection":      true,
	"x-amzn-trace-id": true,
}

// retrieve asks the provider for the credentials to sign with at t.
func (s signer) retrieve(ctx context.Context, t time.Time) (Credentials, error) {
	if s.creds == nil {
		return Credentials{}, nil
	}
	c, err := s.creds.Retrieve(ctx)
	if err != nil {
		return Credentials{}, fmt.Errorf("s3: credentials: %w", err)
	}
	if !c.Expires.IsZero() && !t.Before(c.Expires) {
		return Credentials{}, fmt.Errorf("s3: credentials: expired at %s", c.Expires.UTC().Format(time.RFC3339))
	}
	return c, nil
}

// sign signs req in place, with credentials asked for under req's context. It
// sets X-Amz-Date, X-Amz-Content-Sha256, the Authorization header, and (when
// present) X-Amz-Security-Token. The canonical
// URI is taken from req.URL.Opaque and the canonical query from req.URL.RawQuery,
// so the signed request line is byte-for-byte what the transport puts on the
// wire; the caller is responsible for having set those to their SigV4-encoded
// forms. payloadHash must be the lowercase hex SHA-256 of the body (or
// [emptyPayloadHash] / "UNSIGNED-PAYLOAD").
//
// All non-excluded headers already on req are signed, so any header that must be
// covered by the signature (notably x-amz-meta-*) has to be set before calling
// sign.
func (s signer) sign(req *http.Request, payloadHash string) error {
	t := s.now().UTC()
	creds, err := s.retrieve(req.Context(), t)
	if err != nil {
		return err
	}
	amzDate := t.Format("20060102T150405Z")
	dateStamp := t.Format("20060102")

	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	if creds.SessionToken != "" {
		req.Header.Set("X-Amz-Security-Token", creds.SessionToken)
	}

	host := req.URL.Host
	if req.Host != "" {
		host = req.Host
	}

	// Collect the headers to sign: host plus every non-excluded request header,
	// each keyed by its lowercase name.
	type header struct{ name, value string }
	headers := []header{{"host", host}}
	for name, values := range req.Header {
		lower := strings.ToLower(name)
		if headersNotSigned[lower] {
			continue
		}
		headers = append(headers, header{lower, strings.Join(values, ",")})
	}
	sort.Slice(headers, func(i, j int) bool { return headers[i].name < headers[j].name })

	var canonicalHeaders strings.Builder
	signedNames := make([]string, len(headers))
	for i, h := range headers {
		canonicalHeaders.WriteString(h.name)
		canonicalHeaders.WriteByte(':')
		canonicalHeaders.WriteString(canonicalHeaderValue(h.value))
		canonicalHeaders.WriteByte('\n')
		signedNames[i] = h.name
	}
	signedHeaders := strings.Join(signedNames, ";")

	canonicalURI := req.URL.Opaque
	if canonicalURI == "" {
		canonicalURI = "/"
	}

	canonicalRequest := strings.Join([]string{
		req.Method,
		canonicalURI,
		req.URL.RawQuery,
		canonicalHeaders.String(),
		signedHeaders,
		payloadHash,
	}, "\n")

	scope := dateStamp + "/" + s.region + "/" + s.service + "/aws4_request"
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		scope,
		hexSHA256([]byte(canonicalRequest)),
	}, "\n")

	signature := hex.EncodeToString(hmacSHA256(s.signingKey(creds, dateStamp), stringToSign))

	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 "+
		"Credential="+creds.AccessKeyID+"/"+scope+", "+
		"SignedHeaders="+signedHeaders+", "+
		"Signature="+signature)
	return nil
}

// unsignedPayload is the x-amz-content-sha256 sentinel for a body that is not
// signed: presigned URLs, where it is not known at URL-generation time, and
// streamed blobs, where it is known only once sent.
const unsignedPayload = "UNSIGNED-PAYLOAD"

// presignMaxExpiry is the SigV4 upper bound on X-Amz-Expires (7 days).
const presignMaxExpiry = 7 * 24 * time.Hour

// presignQuery builds the SigV4 "query string" signature for a presigned URL and
// returns the full canonical query string with the trailing X-Amz-Signature
// appended. canonicalURI is the SigV4-encoded request path (as produced for
// [signer.sign] via req.URL.Opaque); host is the value bound into the sole signed
// header. Only the host header is signed, so the resulting URL needs no extra
// request headers to remain valid. expires is first cut to what is left of the
// credentials' lifetime, since the URL stops working with them, then clamped to
// [1s, presignMaxExpiry].
func (s signer) presignQuery(ctx context.Context, method, canonicalURI, host string, expires time.Duration) (string, error) {
	t := s.now().UTC()
	creds, err := s.retrieve(ctx, t)
	if err != nil {
		return "", err
	}
	amzDate := t.Format("20060102T150405Z")
	dateStamp := t.Format("20060102")
	scope := dateStamp + "/" + s.region + "/" + s.service + "/aws4_request"

	if !creds.Expires.IsZero() {
		expires = min(expires, creds.Expires.Sub(t))
	}
	if expires < time.Second {
		expires = time.Second
	} else if expires > presignMaxExpiry {
		expires = presignMaxExpiry
	}

	q := url.Values{}
	q.Set("X-Amz-Algorithm", "AWS4-HMAC-SHA256")
	q.Set("X-Amz-Credential", creds.AccessKeyID+"/"+scope)
	q.Set("X-Amz-Date", amzDate)
	q.Set("X-Amz-Expires", strconv.FormatInt(int64(expires.Seconds()), 10))
	q.Set("X-Amz-SignedHeaders", "host")
	if creds.SessionToken != "" {
		q.Set("X-Amz-Security-Token", creds.SessionToken)
	}
	canonicalQueryString := canonicalQuery(q)

	canonicalRequest := strings.Join([]string{
		method,
		canonicalURI,
		canonicalQueryString,
		"host:" + host + "\n",
		"host",
		unsignedPayload,
	}, "\n")

	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		scope,
		hexSHA256([]byte(canonicalRequest)),
	}, "\n")

	signature := hex.EncodeToString(hmacSHA256(s.signingKey(creds, dateStamp), stringToSign))
	return canonicalQueryString + "&X-Amz-Signature=" + signature, nil
}

// signingKey derives the date/region/service-scoped signing key for creds.
func (s signer) signingKey(creds Credentials, dateStamp string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+creds.SecretAccessKey), dateStamp)
	kRegion := hmacSHA256(kDate, s.region)
	kService := hmacSHA256(kRegion, s.service)
	return hmacSHA256(kService, "aws4_request")
}

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

func hexSHA256(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// canonicalHeaderValue trims leading/trailing whitespace and collapses internal
// runs of whitespace to a single space, per the SigV4 canonicalization rules for
// unquoted header values.
func canonicalHeaderValue(v string) string {
	return strings.Join(strings.Fields(v), " ")
}

// awsURIEncode encodes s per the SigV4 rules: unreserved characters
// (A-Z a-z 0-9 - _ . ~) are left as-is and everything else is percent-encoded
// with uppercase hex. "/" is encoded only when encodeSlash is true, so it can be
// used both for path segments (encodeSlash=false) and query components
// (encodeSlash=true).
func awsURIEncode(s string, encodeSlash bool) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		case c == '/':
			if encodeSlash {
				b.WriteString("%2F")
			} else {
				b.WriteByte('/')
			}
		default:
			b.WriteByte('%')
			const hexUpper = "0123456789ABCDEF"
			b.WriteByte(hexUpper[c>>4])
			b.WriteByte(hexUpper[c&0x0f])
		}
	}
	return b.String()
}
