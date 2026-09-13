package flob

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestStater(t *testing.T) {
	factories := map[string]newStoresFn{
		"memory": func(t *testing.T) Stores { return NewMemStores() },
		"os":     func(t *testing.T) Stores { return NewOsStores(t.TempDir()) },
		"s3":     func(t *testing.T) Stores { s, _ := newMockS3Stores(t); return s },
	}
	for name, factory := range factories {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			stores := factory(t)
			s := stores.Use("a")
			st, ok := AsStater(s)
			if !ok {
				t.Fatal("missing Stater")
			}
			for _, d := range []Digest{digest_nil, "", "../invalid", "sha256:bad"} {
				if _, err := st.Stat(ctx, d); !errors.Is(err, ErrNotExist) {
					t.Fatalf("Stat(%q): %v", d, err)
				}
			}
			for _, content := range []string{"hello", ""} {
				m, err := s.Add(ctx, Meta{Labels: Labels{"Test": {"label"}}}, strings.NewReader(content))
				if err != nil {
					t.Fatal(err)
				}
				size, err := st.Stat(ctx, m.Digest)
				if err != nil || size != int64(len(content)) {
					t.Fatalf("Stat = %d, %v", size, err)
				}
				other, _ := AsStater(stores.Use("b"))
				if _, err := other.Stat(ctx, m.Digest); !errors.Is(err, ErrNotExist) {
					t.Fatalf("cross-store Stat: %v", err)
				}
				if err := s.Erase(ctx, m.Digest); err != nil {
					t.Fatal(err)
				}
				if _, err := st.Stat(ctx, m.Digest); !errors.Is(err, ErrNotExist) {
					t.Fatalf("erased Stat: %v", err)
				}
			}
		})
	}
}

func TestOsStatDoesNotReadLabels(t *testing.T) {
	s := NewOsStores(t.TempDir()).Use("a").(OsStore)
	m, err := s.Add(t.Context(), Meta{}, strings.NewReader("hello"))
	if err != nil {
		t.Fatal(err)
	}
	// A directory at the labels path reliably makes Get fail, even as root.
	p := s.pathToRepo(m.Digest, "labels")
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.Mkdir(p, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(t.Context(), m.Digest); err == nil {
		t.Fatal("Get unexpectedly read invalid labels")
	}
	size, err := s.Stat(t.Context(), m.Digest)
	if err != nil || size != 5 {
		t.Fatalf("Stat = %d, %v", size, err)
	}
}

type statTestWrapper struct{ Store }

func (s statTestWrapper) Unwrap() Store { return s.Store }

type getOnlyStatStore struct {
	UnimplementedStore
	size int64
	err  error
}

func (s getOnlyStatStore) Get(context.Context, Digest) (Meta, error) {
	return Meta{Size: s.size}, s.err
}

func TestAsStater(t *testing.T) {
	s := NewMemStores().Use("a")
	for _, decorated := range []Store{s, statTestWrapper{AllowDuplicates(CheckExistence(PrepareDigest(s, "")))}} {
		got, ok := AsStater(decorated)
		if !ok || got != s.(Stater) {
			t.Fatalf("AsStater = %v, %v", got, ok)
		}
	}
	for _, s := range []Store{nil, UnimplementedStore{}, statTestWrapper{nil}} {
		if got, ok := AsStater(s); ok || got != nil {
			t.Fatalf("AsStater = %v, %v", got, ok)
		}
	}
}

func TestCompositeStat(t *testing.T) {
	factories := map[string]func(Store, Store) Store{
		"cache":    func(p, o Store) Store { return CacheStore{Primary: p, Origin: o} },
		"fallback": func(p, o Store) Store { return FallbackStore{Primary: p, Secondary: o} },
	}
	for name, factory := range factories {
		t.Run(name, func(t *testing.T) {
			origin := NewMemStores().Use("origin")
			m, err := origin.Add(t.Context(), Meta{}, strings.NewReader("hello"))
			if err != nil {
				t.Fatal(err)
			}
			for _, primary := range []Store{NewMemStores().Use("empty"), ErrorStore{Err: errors.New("unavailable")}} {
				s, _ := AsStater(statTestWrapper{factory(primary, origin)})
				size, err := s.Stat(t.Context(), m.Digest)
				if err != nil || size != 5 {
					t.Fatalf("origin Stat = %d, %v", size, err)
				}
			}
			// Get-only children retain both fallback and primary precedence.
			for _, primaryErr := range []error{nil, ErrNotExist} {
				s, _ := AsStater(factory(getOnlyStatStore{size: 7, err: primaryErr}, getOnlyStatStore{size: 9}))
				want := int64(7)
				if primaryErr != nil {
					want = 9
				}
				size, err := s.Stat(t.Context(), m.Digest)
				if err != nil || size != want {
					t.Fatalf("Get fallback = %d, %v; want %d", size, err, want)
				}
			}
			wantErr := errors.New("origin unavailable")
			s, _ := AsStater(factory(ErrorStore{Err: ErrNotExist}, ErrorStore{Err: wantErr}))
			if _, err := s.Stat(t.Context(), m.Digest); !errors.Is(err, wantErr) {
				t.Fatalf("origin error = %v", err)
			}
		})
	}
}

func TestS3StatReadsReferenceSize(t *testing.T) {
	d := DigestFromBytes([]byte("hello"))
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		wantPath := "/bucket/refs/sha256/" + strings.TrimPrefix(string(d), "sha256:") + "/a"
		if r.Method != http.MethodHead || r.URL.Path != wantPath {
			t.Errorf("request = %s %s; want HEAD %s", r.Method, r.URL.Path, wantPath)
		}
		w.Header().Set("Content-Length", "0")
		w.Header().Set(metaPrefix+metaSizeKey, "5")
	}))
	defer srv.Close()
	stores, err := NewS3Stores(S3Config{Endpoint: srv.URL, Bucket: "bucket", Region: "us-east-1", UsePathStyle: true, Credentials: Credentials{AccessKeyID: "key", SecretAccessKey: "secret"}})
	if err != nil {
		t.Fatal(err)
	}
	st, _ := AsStater(stores.Use("a"))
	size, err := st.Stat(t.Context(), d)
	if err != nil || size != 5 || calls != 1 {
		t.Fatalf("Stat = %d, %v (%d requests)", size, err, calls)
	}
}
