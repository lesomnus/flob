package flob

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestReadOnlyPublicHandler(t *testing.T) {
	ctx := context.Background()
	stores := NewMemStores()
	content := []byte("published blob")
	m, err := stores.Use("_").Add(ctx, Meta{}, bytes.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stores.Use("private").Add(ctx, Meta{}, bytes.NewReader(content)); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(HttpHandler{Stores: stores, ReadOnly: true, AllowedStores: []string{"_"}})
	defer srv.Close()
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		req, _ := http.NewRequest(method, srv.URL+"/_/"+string(m.Digest), nil)
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || (method == http.MethodGet && !bytes.Equal(body, content)) {
			t.Fatalf("%s: status %d, body %q", method, resp.StatusCode, body)
		}
	}
	for _, method := range []string{http.MethodPost, http.MethodPatch, http.MethodDelete, http.MethodPut} {
		req, _ := http.NewRequest(method, srv.URL+"/_/"+string(m.Digest), bytes.NewBufferString("replacement"))
		req.Header.Set("Flob-Changed", "yes")
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 405 || resp.Header.Get("Allow") != "GET, HEAD" {
			t.Fatalf("%s: status %d, Allow %q", method, resp.StatusCode, resp.Header.Get("Allow"))
		}
	}
	for _, path := range []string{"/private/", "/%70rivate/"} {
		resp, err := srv.Client().Get(srv.URL + path + string(m.Digest))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 404 {
			t.Fatalf("private namespace: %d", resp.StatusCode)
		}
	}
	reader, info, err := stores.Use("_").Open(ctx, m.Digest)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	body, _ := io.ReadAll(reader)
	labels, err := info.Labels(ctx)
	if err != nil || !bytes.Equal(body, content) || len(labels) != 0 {
		t.Fatalf("write attempt changed original blob: %q %v %v", body, labels, err)
	}
}
