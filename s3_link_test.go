package flob

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestS3LinkOnlyTransfersReference(t *testing.T) {
	mock := newMockS3("bucket")
	var mu sync.Mutex
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		mu.Unlock()
		if r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/refs/") {
			data, err := io.ReadAll(r.Body)
			if err != nil || len(data) != 0 {
				t.Errorf("reference body = %q, %v", data, err)
			}
			r.Body = io.NopCloser(strings.NewReader(string(data)))
		}
		mock.ServeHTTP(w, r)
	}))
	defer srv.Close()
	stores, err := NewS3Stores(S3Config{Endpoint: srv.URL, Region: "us-east-1", Bucket: "bucket", UsePathStyle: true, Credentials: Credentials{AccessKeyID: "key", SecretAccessKey: "secret"}})
	if err != nil {
		t.Fatal(err)
	}
	source := stores.Use("source").(*S3Store)
	dest := stores.Use("dest").(*S3Store)
	added, err := source.Add(t.Context(), Meta{Labels: Labels{"Owner": {"source"}}}, strings.NewReader("content"))
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	calls = nil
	mu.Unlock()
	linked, err := dest.Link(t.Context(), added.Digest, AllowDuplicates(source))
	if err != nil {
		t.Fatal(err)
	}
	if linked.Digest != added.Digest || linked.Size != 7 || linked.Labels.Get("Owner") != "source" {
		t.Fatalf("Link Meta = %#v", linked)
	}
	mu.Lock()
	gotCalls := append([]string(nil), calls...)
	mu.Unlock()
	want := []string{"HEAD /bucket/" + stores.refKey(added.Digest, "source"), "PUT /bucket/" + stores.refKey(added.Digest, "dest")}
	if fmt.Sprint(gotCalls) != fmt.Sprint(want) {
		t.Fatalf("Link requests = %v; want %v", gotCalls, want)
	}
	linked.Labels["Owner"][0] = "mutated"
	if err := source.Erase(t.Context(), added.Digest); err != nil {
		t.Fatal(err)
	}
	r, info, err := dest.Open(t.Context(), added.Digest)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	data, err := io.ReadAll(r)
	if err != nil || string(data) != "content" {
		t.Fatalf("linked content = %q, %v", data, err)
	}
	labels, err := info.Labels(t.Context())
	if err != nil || labels.Get("Owner") != "source" {
		t.Fatalf("linked labels = %v, %v", labels, err)
	}
}

func TestS3LinkVisibilityAndDuplicates(t *testing.T) {
	stores, _ := newMockS3Stores(t)
	source := stores.Use("source").(*S3Store)
	dest := stores.Use("dest").(*S3Store)
	m, err := source.Add(t.Context(), Meta{Labels: Labels{"Owner": {"source"}}}, strings.NewReader("content"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dest.Link(t.Context(), m.Digest, stores.Use("missing")); !errors.Is(err, ErrNotExist) {
		t.Fatalf("missing source = %v", err)
	}
	if _, err := dest.Stat(t.Context(), m.Digest); !errors.Is(err, ErrNotExist) {
		t.Fatalf("missing source created target: %v", err)
	}
	const n = 12
	results := make(chan error, n)
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() { _, err := dest.Link(t.Context(), m.Digest, source); results <- err })
	}
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		} else if !errors.Is(err, ErrAlreadyExists) {
			t.Fatal(err)
		}
	}
	if successes != 1 {
		t.Fatalf("successful concurrent links = %d", successes)
	}
	if err := dest.Label(t.Context(), m.Digest, Labels{"Owner": {"dest"}}); err != nil {
		t.Fatal(err)
	}
	for _, from := range []Store{source, dest} {
		if _, err := dest.Link(t.Context(), m.Digest, from); !errors.Is(err, ErrAlreadyExists) {
			t.Fatalf("duplicate = %v", err)
		}
	}
	info, err := dest.Stat(t.Context(), m.Digest)
	if err != nil {
		t.Fatal(err)
	}
	labels, err := info.Labels(t.Context())
	if err != nil || labels.Get("Owner") != "dest" {
		t.Fatalf("duplicate replaced labels: %v, %v", labels, err)
	}
	if _, err := dest.Link(t.Context(), m.Digest, stores.Use("missing")); !errors.Is(err, ErrNotExist) {
		t.Fatalf("source missing priority = %v", err)
	}
	other, _ := newMockS3Stores(t)
	var nilSource *S3Store
	sameBackendDifferentPool := *stores
	for _, from := range []Store{nil, nilSource, NewMemStores().Use("source"), other.Use("source"), sameBackendDifferentPool.Use("source")} {
		if _, err := dest.Link(t.Context(), m.Digest, from); !errors.Is(err, ErrIncompatibleStore) {
			t.Fatalf("incompatible = %v", err)
		}
	}
	if _, err := dest.Link(t.Context(), "bad", source); err == nil {
		t.Fatal("invalid digest accepted")
	}
}

func TestS3LinkConditionalConflict(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodHead:
			w.Header().Set(metaPrefix+metaSizeKey, "7")
		case http.MethodPut:
			if r.Header.Get("If-None-Match") != "*" {
				t.Error("missing conditional create")
			}
			w.WriteHeader(http.StatusConflict)
		default:
			t.Errorf("unexpected %s", r.Method)
		}
	}))
	defer srv.Close()
	stores, err := NewS3Stores(S3Config{Endpoint: srv.URL, Region: "us-east-1", Bucket: "bucket", UsePathStyle: true, Credentials: Credentials{AccessKeyID: "key", SecretAccessKey: "secret"}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = stores.Use("dest").(*S3Store).Link(t.Context(), DigestFromBytes([]byte("content")), stores.Use("source"))
	if err == nil || errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("conditional conflict = %v", err)
	}
}
