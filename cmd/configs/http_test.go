package configs

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"
	"github.com/lesomnus/flob"
)

func TestHTTPConfigFailoverAndOriginAuthentication(t *testing.T) {
	t.Setenv("TEST_PRIMARY_BLOB_TOKEN", "primary-token")
	t.Setenv("TEST_BACKUP_BLOB_TOKEN", "backup-token")
	ctx := context.Background()
	blobs := flob.NewMemStores()
	meta, err := blobs.Use("_").Add(ctx, flob.Meta{}, strings.NewReader("published bytes"))
	if err != nil {
		t.Fatal(err)
	}
	type request struct{ method, auth, match string }
	primaryRequests := make(chan request, 4)
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryRequests <- request{r.Method, r.Header.Get("Authorization"), r.Header.Get("If-Match")}
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		conn.Close() // No HTTP response: the read may fail over.
	}))
	t.Cleanup(primary.Close)
	backupRequests := make(chan request, 4)
	handler := http.StripPrefix("/backup", flob.HttpHandler{Stores: blobs, ReadOnly: true})
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backupRequests <- request{r.Method, r.Header.Get("Authorization"), r.Header.Get("If-Match")}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(backup.Close)
	doc := fmt.Sprintf(`http/remote:
  targets: [%q, %q]
  auth:
    %q:
      bearer_token_env: TEST_PRIMARY_BLOB_TOKEN
    %q:
      bearer_token_env: TEST_BACKUP_BLOB_TOKEN
`, primary.URL+"/primary", backup.URL+"/backup", primary.URL, backup.URL)
	c := StoresConfig{}
	if err := yaml.Unmarshal([]byte(doc), &c); err != nil {
		t.Fatal(err)
	}
	stores, err := c.Use(ctx, "http/remote")
	if err != nil {
		t.Fatal(err)
	}
	reader, _, err := stores.Use("_").Open(ctx, meta.Digest)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(reader)
	if err != nil || string(body) != "published bytes" {
		t.Fatalf("download = %q, %v", body, err)
	}
	if _, err := reader.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if body, err := io.ReadAll(reader); err != nil || string(body) != "published bytes" {
		t.Fatalf("reopened download = %q, %v", body, err)
	}
	reader.Close()
	if len(primaryRequests) != 2 {
		t.Fatalf("primary requests = %d; only HEAD and the first GET may fail over", len(primaryRequests))
	}
	for _, method := range []string{"HEAD", "GET"} {
		if r := <-primaryRequests; r.method != method || r.auth != "Bearer primary-token" {
			t.Fatalf("primary request = %+v", r)
		}
	}
	if len(backupRequests) != 3 {
		t.Fatalf("backup requests = %d, want HEAD, GET and a pinned range GET", len(backupRequests))
	}
	for i, method := range []string{"HEAD", "GET", "GET"} {
		r := <-backupRequests
		if r.method != method || r.auth != "Bearer backup-token" {
			t.Fatalf("backup request %d = %+v", i, r)
		}
		if i == 2 && r.match == "" {
			t.Fatal("range GET was not pinned")
		}
	}
	dumped, err := yaml.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(dumped), "primary-token") || strings.Contains(string(dumped), "backup-token") {
		t.Fatal("resolved token leaked into serialized config")
	}
}

func TestHTTPConfigPublicOriginNeedsNoAuthentication(t *testing.T) {
	seen := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Get("Authorization")
		w.WriteHeader(404)
	}))
	t.Cleanup(srv.Close)
	c := StoresConfig{"http/public": &StoresConfigHttp{Targets: []string{srv.URL}}}
	s, err := c.Use(t.Context(), "http/public")
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Use("_").Stat(t.Context(), flob.DigestFromBytes([]byte("missing")))
	if err == nil {
		t.Fatal("expected missing blob")
	}
	if auth := <-seen; auth != "" {
		t.Fatalf("public Authorization = %q", auth)
	}
}

func TestHTTPConfigInvalidAuthentication(t *testing.T) {
	t.Setenv("TEST_BLOB_TOKEN", "token-secret")
	t.Setenv("TEST_EMPTY_BLOB_TOKEN", "")
	for _, config := range []StoresConfigHttp{
		{},
		{Targets: []string{"https://origin.invalid"}, Auth: map[string]HTTPAuthConfig{"https://origin.invalid": {}}},
		{Targets: []string{"https://origin.invalid"}, Auth: map[string]HTTPAuthConfig{"https://origin.invalid": {BearerTokenEnv: "TEST_EMPTY_BLOB_TOKEN"}}},
		{Targets: []string{"https://origin.invalid"}, Auth: map[string]HTTPAuthConfig{"https://origin.invalid/path": {BearerTokenEnv: "TEST_BLOB_TOKEN"}}},
	} {
		c := StoresConfig{"http/invalid": &config}
		_, err := c.Use(t.Context(), "http/invalid")
		if err == nil || strings.Contains(err.Error(), "token-secret") {
			t.Fatalf("config error = %v", err)
		}
	}
}
