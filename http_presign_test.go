package flob

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type presignTestStore struct {
	Store
	presignErr   error
	presignCalls int
	openCalls    int
}

func (s *presignTestStore) PresignOpen(ctx context.Context, d Digest, ttl time.Duration) (string, Meta, error) {
	s.presignCalls++
	if s.presignErr != nil {
		return "", Meta{}, s.presignErr
	}
	info, err := s.Store.Stat(ctx, d)
	if err != nil {
		return "", Meta{}, err
	}
	m, err := infoMeta(ctx, info)
	return "https://example.test/blob", m, err
}

func (s *presignTestStore) Open(ctx context.Context, d Digest) (io.ReadSeekCloser, Info, error) {
	s.openCalls++
	return s.Store.Open(ctx, d)
}

func TestPrimaryPresigner(t *testing.T) {
	wrappers := map[string]func(Store, Store) Store{
		"cache": func(primary, fallback Store) Store {
			return (&CacheStores{Primary: FixedStores{Store: primary}, Origin: FixedStores{Store: fallback}}).Use("t")
		},
		"fallback": func(primary, fallback Store) Store {
			return FallbackStores{Primary: FixedStores{Store: primary}, Secondary: fallback}.Use("t")
		},
	}
	for name, wrap := range wrappers {
		t.Run(name, func(t *testing.T) {
			t.Run("discovery through nested decorators", func(t *testing.T) {
				primary := &presignTestStore{Store: NewMemStores().Use("t")}
				secondary := &presignTestStore{Store: NewMemStores().Use("t")}
				store := AllowDuplicates(CheckExistence(PrepareDigest(wrap(primary, secondary), Canonical)))
				if p, ok := AsPresigner(store); !ok || p != primary {
					t.Fatalf("AsPresigner = %v, %v; want primary", p, ok)
				}
				if _, ok := AsPresigner(wrap(primary.Store, secondary)); ok {
					t.Fatal("secondary Presigner must not be exposed")
				}
			})
			for _, tc := range []struct {
				name            string
				primaryHasBlob  bool
				fallbackHasBlob bool
				presignErr      error
				status          int
			}{
				{name: "primary hit redirects", primaryHasBlob: true, status: http.StatusTemporaryRedirect},
				{name: "primary miss streams fallback", fallbackHasBlob: true, status: http.StatusOK},
				{name: "missing everywhere returns 404", status: http.StatusNotFound},
				{name: "presign error streams primary", primaryHasBlob: true, presignErr: errors.New("presign unavailable"), status: http.StatusOK},
			} {
				t.Run(tc.name, func(t *testing.T) {
					ctx := t.Context()
					const content = "blob content"
					d := DigestFromBytes([]byte(content))
					primary := &presignTestStore{Store: NewMemStores().Use("t"), presignErr: tc.presignErr}
					fallback := NewMemStores().Use("t")
					for _, target := range []struct {
						store     Store
						populated bool
					}{{primary, tc.primaryHasBlob}, {fallback, tc.fallbackHasBlob}} {
						if target.populated {
							if _, err := target.store.Add(ctx, Meta{}, strings.NewReader(content)); err != nil {
								t.Fatal(err)
							}
						}
					}
					h := HttpHandler{Stores: FixedStores{Store: wrap(primary, fallback)}, Redirect: true}
					response := httptest.NewRecorder()
					h.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/t/"+string(d), nil).WithContext(ctx))
					if response.Code != tc.status {
						t.Fatalf("status = %d; want %d: %s", response.Code, tc.status, response.Body.String())
					}
					if primary.presignCalls != 1 {
						t.Fatalf("presign calls = %d; want 1", primary.presignCalls)
					}
					if tc.status == http.StatusTemporaryRedirect {
						if got := response.Header().Get("Location"); got != "https://example.test/blob" {
							t.Fatalf("Location = %q", got)
						}
						if primary.openCalls != 0 {
							t.Fatal("redirect unexpectedly opened the primary blob")
						}
					} else {
						wantOpenCalls := 1
						if name == "cache" && !tc.primaryHasBlob {
							// Cache miss leaders recheck after joining the flight
							// in case an earlier fill committed in the meantime.
							wantOpenCalls = 2
						}
						if primary.openCalls != wantOpenCalls {
							t.Fatalf("open calls = %d; want %d", primary.openCalls, wantOpenCalls)
						}
						if response.Header().Get("Location") != "" {
							t.Fatal("streaming response has redirect location")
						}
						if tc.status == http.StatusOK && response.Body.String() != content {
							t.Fatalf("body = %q; want %q", response.Body.String(), content)
						}
					}
				})
			}
		})
	}
}
