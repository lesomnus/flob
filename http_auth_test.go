package flob

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type authTestStores struct{}

func (authTestStores) Use(string) Store { panic("unauthorized request opened storage") }

type authTestBody struct{}

func (authTestBody) Read([]byte) (int, error) { panic("unauthorized request consumed body") }
func (authTestBody) Close() error             { return nil }

type authTestAuthorizer func(*http.Request, string, Action) error

func (f authTestAuthorizer) Authorize(r *http.Request, store string, action Action) error {
	return f(r, store, action)
}

func TestHTTPAuthorizationBeforeStorageAndBody(t *testing.T) {
	for method, action := range map[string]Action{
		"GET": ActionRead, "HEAD": ActionRead, "POST": ActionAdd,
		"PATCH": ActionLabel, "DELETE": ActionErase,
	} {
		t.Run(method, func(t *testing.T) {
			calls := 0
			h := HttpHandler{Stores: authTestStores{}, Authorizer: authTestAuthorizer(func(r *http.Request, store string, got Action) error {
				calls++
				if store != "private" || got != action {
					t.Fatalf("authorization target=%q action=%q", store, got)
				}
				return ErrForbidden
			})}
			r := httptest.NewRequest(method, "/%70rivate/sha256:"+strings.Repeat("0", 64), nil)
			r.Body = authTestBody{}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != 403 || calls != 1 {
				t.Fatalf("status=%d authorization calls=%d", w.Code, calls)
			}
		})
	}
}

func TestHTTPAuthorizationErrorsAreSanitized(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
	}{
		{ErrUnauthenticated, 401}, {ErrForbidden, 403},
		{errors.New("private verifier detail"), 503},
	} {
		h := HttpHandler{Stores: authTestStores{}, Authorizer: authTestAuthorizer(func(*http.Request, string, Action) error { return tc.err })}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("POST", "/_", authTestBody{}))
		if w.Code != tc.status || strings.Contains(w.Body.String(), "private verifier detail") {
			t.Fatalf("response=%d %q", w.Code, w.Body.String())
		}
		if tc.status == 401 && w.Header().Get("WWW-Authenticate") != `Bearer realm="flob"` {
			t.Fatal("missing bearer challenge")
		}
	}
}
