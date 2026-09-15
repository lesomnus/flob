package flob

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestStat(t *testing.T) {
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
			st := s
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
				info, err := st.Stat(ctx, m.Digest)
				if err != nil || info.Digest() != m.Digest || mustSize(t, info) != int64(len(content)) {
					t.Fatalf("Stat = %v, %v", info, err)
				}
				other := stores.Use("b")
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

func TestOsInfoDoesNotReadLabelsUntilRequested(t *testing.T) {
	s := NewOsStores(t.TempDir()).Use("a").(OsStore)
	m, err := s.Add(t.Context(), Meta{}, strings.NewReader("hello"))
	if err != nil {
		t.Fatal(err)
	}
	// A directory at the labels path reliably makes reading fail, even as root.
	p := s.pathToRepo(m.Digest, "labels")
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.Mkdir(p, 0700); err != nil {
		t.Fatal(err)
	}
	statCtx, cancel := context.WithCancel(t.Context())
	info, err := s.Stat(statCtx, m.Digest)
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	r, opened, err := s.Open(t.Context(), m.Digest)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	data, err := io.ReadAll(r)
	if err != nil || string(data) != "hello" {
		t.Fatalf("Open content = %q, %v", data, err)
	}
	for _, info := range []Info{info, opened} {
		if info.Digest() != m.Digest || mustSize(t, info) != 5 {
			t.Fatalf("Info = %v", info)
		}
		if _, err := info.Labels(t.Context()); err == nil {
			t.Fatal("Labels unexpectedly read invalid labels")
		}
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := s.Label(t.Context(), m.Digest, Labels{"Version": {"first"}}); err != nil {
		t.Fatal(err)
	}
	for _, info := range []Info{info, opened} {
		labels, err := info.Labels(t.Context())
		if err != nil || labels.Get("Version") != "first" {
			t.Fatalf("retry Labels = %v, %v", labels, err)
		}
	}
	if err := s.Label(t.Context(), m.Digest, Labels{"Version": {"second"}}); err != nil {
		t.Fatal(err)
	}
	for _, info := range []Info{info, opened} {
		labels, err := info.Labels(t.Context())
		if err != nil || labels.Get("Version") != "first" {
			t.Fatalf("cached Labels = %v, %v", labels, err)
		}
	}
	fresh, err := s.Stat(t.Context(), m.Digest)
	if err != nil {
		t.Fatal(err)
	}
	labels, err := fresh.Labels(t.Context())
	if err != nil || labels.Get("Version") != "second" {
		t.Fatalf("fresh Labels = %v, %v", labels, err)
	}

}

type statTestWrapper struct{ Store }

func (s statTestWrapper) Unwrap() Store { return s.Store }

type fixedStatStore struct {
	UnimplementedStore
	size int64
	err  error
}

func (s fixedStatStore) Stat(_ context.Context, d Digest) (Info, error) {
	if s.err != nil {
		return nil, s.err
	}
	return NewInfo(d, s.size, nil), nil
}

func TestCompositeStat(t *testing.T) {
	factories := map[string]func(Store, Store) Store{
		"cache":    func(p, o Store) Store { return &CacheStore{Primary: p, Origin: o} },
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
				s := statTestWrapper{factory(primary, origin)}
				info, err := s.Stat(t.Context(), m.Digest)
				if err != nil || mustSize(t, info) != 5 {
					t.Fatalf("origin Stat = %v, %v", info, err)
				}
			}
			// Children retain both fallback and primary precedence.
			for _, primaryErr := range []error{nil, ErrNotExist} {
				s := factory(fixedStatStore{size: 7, err: primaryErr}, fixedStatStore{size: 9})
				want := int64(7)
				if primaryErr != nil {
					want = 9
				}
				info, err := s.Stat(t.Context(), m.Digest)
				if err != nil || mustSize(t, info) != want {
					t.Fatalf("Stat fallback = %v, %v; want %d", info, err, want)
				}
			}
			// An Info returned from the origin stays bound to it even when the
			// primary gains the blob before labels are requested.
			primary := NewMemStores().Use("primary")
			if err := origin.Label(t.Context(), m.Digest, Labels{"Source": {"origin"}}); err != nil {
				t.Fatal(err)
			}
			composite := factory(primary, origin)
			fromOrigin, err := composite.Stat(t.Context(), m.Digest)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := primary.Add(t.Context(), Meta{Labels: Labels{"Source": {"primary"}}}, strings.NewReader("hello")); err != nil {
				t.Fatal(err)
			}
			labels, err := fromOrigin.Labels(t.Context())
			if err != nil || labels.Get("Source") != "origin" {
				t.Fatalf("origin Info Labels = %v, %v", labels, err)
			}
			fromPrimary, err := composite.Stat(t.Context(), m.Digest)
			if err != nil {
				t.Fatal(err)
			}
			labels, err = fromPrimary.Labels(t.Context())
			if err != nil || labels.Get("Source") != "primary" {
				t.Fatalf("fresh Info Labels = %v, %v", labels, err)
			}

			wantErr := errors.New("origin unavailable")
			s := factory(ErrorStore{Err: ErrNotExist}, ErrorStore{Err: wantErr})
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
		wantPath := "/bucket/refs/a/sha256/" + strings.TrimPrefix(string(d), "sha256:")
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
	st := stores.Use("a")
	info, err := st.Stat(t.Context(), d)
	if err != nil || mustSize(t, info) != 5 || calls != 1 {
		t.Fatalf("Stat = %v, %v (%d requests)", info, err, calls)
	}
}
