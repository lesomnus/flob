package flob

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var namespaceIDs = []string{"simple", "a.b-c_1", "", ".", "..", "~", "~YQ", "a/b", "a", "../escaped", "a/../../b", "/abs", `a\b`, "%2F", "/", "#?\\\x00", "nul", "CON.txt", "trailing.", "한글"}

func TestNamespaceSegments(t *testing.T) {
	seen := map[string]string{}
	for _, id := range namespaceIDs {
		segment := namespaceSegment(id)
		if segment == "" || segment == "." || segment == ".." || strings.ContainsAny(segment, "/\\\x00") {
			t.Fatalf("unsafe segment %q for %q", segment, id)
		}
		if previous, ok := seen[segment]; ok {
			t.Fatalf("collision: %q and %q", id, previous)
		}
		seen[segment] = id
		got, err := namespaceID(segment)
		if err != nil || got != id {
			t.Fatalf("decode %q = %q, %v", segment, got, err)
		}
	}
	for _, id := range []string{"simple", "a.b-c_1", "Upper123"} {
		if namespaceSegment(id) != id {
			t.Fatalf("ordinary namespace moved: %q", id)
		}
	}
	for _, segment := range []string{"~YQ", "~!", "~Lh", "~Lw=="} {
		if _, err := namespaceID(segment); err == nil {
			t.Fatalf("invalid encoded segment accepted: %q", segment)
		}
	}
}

func TestNamespaceIsolation(t *testing.T) {
	factories := map[string]newStoresFn{
		"os": func(t *testing.T) Stores { return NewOsStores(t.TempDir()) },
		"s3": func(t *testing.T) Stores { s, _ := newMockS3Stores(t); return s },
		"http": func(t *testing.T) Stores {
			mux := http.NewServeMux()
			mux.Handle("/", &HttpHandler{Stores: NewOsStores(t.TempDir())})
			srv := httptest.NewServer(mux)
			t.Cleanup(srv.Close)
			return HttpStores{Target: srv.URL, Client: srv.Client()}
		},
	}
	for name, factory := range factories {
		t.Run(name, func(t *testing.T) {
			stores := factory(t)
			d := DigestFromBytes([]byte("content"))
			for _, id := range namespaceIDs {
				s := stores.Use(id)
				if _, err := s.Stat(t.Context(), d); err != ErrNotExist {
					t.Fatalf("namespace %q not isolated: %v", id, err)
				}
				_, err := s.Add(t.Context(), Meta{Labels: Labels{"Owner": {namespaceSegment(id)}}}, strings.NewReader("content"))
				if err != nil {
					t.Fatalf("Add %q: %v", id, err)
				}
			}
			for _, id := range namespaceIDs {
				s := stores.Use(id)
				r, info, err := s.Open(t.Context(), d)
				if err != nil {
					t.Fatalf("Open %q: %v", id, err)
				}
				data, err := io.ReadAll(r)
				r.Close()
				if err != nil || string(data) != "content" {
					t.Fatalf("content %q: %q, %v", id, data, err)
				}
				labels, err := info.Labels(t.Context())
				if err != nil || labels.Get("Owner") != namespaceSegment(id) {
					t.Fatalf("labels %q: %v, %v", id, labels, err)
				}
				if err := s.Label(t.Context(), d, Labels{"Owner": {"updated"}}); err != nil {
					t.Fatalf("Label %q: %v", id, err)
				}
				info, err = s.Stat(t.Context(), d)
				if err != nil {
					t.Fatalf("Stat %q: %v", id, err)
				}
				labels, err = info.Labels(t.Context())
				if err != nil || labels.Get("Owner") != "updated" {
					t.Fatalf("updated labels %q: %v, %v", id, labels, err)
				}
				if err := s.Erase(t.Context(), d); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestOsNamespacePathsConfined(t *testing.T) {
	root := t.TempDir()
	stores := NewOsStores(root)
	for _, id := range namespaceIDs {
		s := stores.Use(id).(OsStore)
		if filepath.Dir(s.repo) != filepath.Join(root, "repos") {
			t.Fatalf("namespace %q escaped: %q", id, s.repo)
		}
		if _, err := s.Add(t.Context(), Meta{}, strings.NewReader("content")); err != nil {
			t.Fatalf("Add %q: %v", id, err)
		}
	}
	entries, err := os.ReadDir(filepath.Join(root, "repos"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(namespaceIDs) {
		t.Fatalf("namespace directories = %d; want %d", len(entries), len(namespaceIDs))
	}
}

func TestS3NamespaceKeysFlat(t *testing.T) {
	stores, mock := newMockS3Stores(t)
	for _, id := range namespaceIDs {
		if _, err := stores.Use(id).Add(t.Context(), Meta{}, strings.NewReader("content")); err != nil {
			t.Fatalf("Add %q: %v", id, err)
		}
	}
	mock.mu.Lock()
	defer mock.mu.Unlock()
	count := 0
	for key := range mock.objects {
		if strings.HasPrefix(key, "refs/") {
			if len(strings.Split(key, "/")) != 4 {
				t.Fatalf("nested ref: %q", key)
			}
			count++
		}
	}
	if count != len(namespaceIDs) {
		t.Fatalf("references = %d", count)
	}
}

func TestHttpNamespaceEncoding(t *testing.T) {
	h := &HttpHandler{Stores: NewMemStores()}
	for _, id := range namespaceIDs {
		path := "/" + namespaceSegment(id)
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader("content"))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("POST %q: %d %s", id, rec.Code, rec.Body.String())
		}
		want := path + "/" + string(DigestFromBytes([]byte("content")))
		if got := rec.Header().Get("Location"); got != want {
			t.Fatalf("Location = %q; want %q", got, want)
		}
	}
	for path, want := range map[string]string{"/a%2Fb": "a/b", "/%252F": "%2F", "/%7E": ""} {
		got, _, ok := h.parsePath(path)
		if !ok || got != want {
			t.Fatalf("parse %q = %q, %v", path, got, ok)
		}
	}
	for _, path := range []string{"/", "/~YQ", "/~!", "/a/b/c", "/a%"} {
		if _, _, ok := h.parsePath(path); ok {
			t.Fatalf("accepted malformed path %q", path)
		}
	}
}
