package configs

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-yaml"
	"github.com/lesomnus/flob"
)

// echoBucket is a stand-in S3 endpoint: it answers HEAD with 404 so Stat returns
// ErrNotExist, which is enough to prove the config produced a working, signed
// client pointed at the right place.
func TestStoresConfigS3(t *testing.T) {
	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(404)
	}))
	t.Cleanup(srv.Close)

	doc := `
s3/main:
  endpoint: ` + srv.URL + `
  region: us-east-1
  bucket: my-bucket
  prefix: tenant-a
  path_style: true
  access_key_id: AKIAEXAMPLE
  secret_access_key: secretexample
`
	c := StoresConfig{}
	if err := yaml.Unmarshal([]byte(doc), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if _, ok := c["s3/main"].(*StoresConfigS3); !ok {
		t.Fatalf("s3 config type = %T", c["s3/main"])
	}

	stores, err := c.build(context.Background(), "s3/main")
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	d := flob.Digest("sha256:0000000000000000000000000000000000000000000000000000000000000000")
	_, err = stores.Use("store-1").Stat(context.Background(), d)
	if err == nil || !strings.Contains(err.Error(), "not exist") {
		t.Fatalf("stat err = %v", err)
	}

	if !strings.Contains(gotPath, "/my-bucket/tenant-a/refs/") {
		t.Fatalf("request path = %q", gotPath)
	}
	if !strings.HasPrefix(gotAuth, "AWS4-HMAC-SHA256 ") {
		t.Fatalf("authorization = %q", gotAuth)
	}
}

func TestStoresConfigS3PublicEndpoint(t *testing.T) {
	doc := `
s3/main:
  bucket: b
  path_style: true
  public_endpoint: https://cdn.example.com
`
	c := StoresConfig{}
	if err := yaml.Unmarshal([]byte(doc), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	s3c, ok := c["s3/main"].(*StoresConfigS3)
	if !ok {
		t.Fatalf("s3 config type = %T", c["s3/main"])
	}
	if s3c.PublicEndpoint != "https://cdn.example.com" {
		t.Fatalf("public_endpoint = %q", s3c.PublicEndpoint)
	}
	if _, err := c.build(context.Background(), "s3/main"); err != nil {
		t.Fatalf("build: %v", err)
	}
}

// fakePresignStore implements flob.Store (via the unimplemented base) plus
// flob.Presigner, standing in for the S3 store.
type fakePresignStore struct{ flob.UnimplementedStore }

func (fakePresignStore) PresignOpen(ctx context.Context, d flob.Digest, ttl time.Duration) (string, flob.Meta, error) {
	return "https://example.test/presigned", flob.Meta{Digest: d}, nil
}

// TestPresignerSurvivesTraceAndMeterWrapping guards the exact production wiring:
// serve.go hands the handler a store wrapped in StoresTrace and then StoresMeter.
// Both decorators embed flob.Store, which does not include PresignOpen, so without
// capability forwarding the handler's presign lookup would silently fail and
// server.redirect would be dead. flob.AsPresigner must recover it via Unwrap.
func TestPresignerSurvivesTraceAndMeterWrapping(t *testing.T) {
	inner := flob.FixedStores{Store: fakePresignStore{}}
	traced := StoresTrace{Stores: inner}
	meter := StoresMeter{base: traced}

	store := meter.Use("x") // == StoreMeter{Store: StoreTrace{fakePresignStore}}
	if _, ok := flob.AsPresigner(store); !ok {
		t.Fatal("Presigner capability lost through StoresTrace + StoresMeter; server.redirect would be a no-op")
	}
}

func TestServerConfigRedirect(t *testing.T) {
	var sc ServerConfig
	if err := yaml.Unmarshal([]byte("redirect: true\n"), &sc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !sc.Redirect {
		t.Fatal("server.redirect not parsed as true")
	}
}

func TestStoresConfigS3EnvFallback(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "env-key")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "env-secret")
	t.Setenv("AWS_REGION", "eu-west-1")

	doc := `
s3/env:
  bucket: b
  path_style: true
`
	c := StoresConfig{}
	if err := yaml.Unmarshal([]byte(doc), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, err := c.build(context.Background(), "s3/env"); err != nil {
		t.Fatalf("build: %v", err)
	}
}
