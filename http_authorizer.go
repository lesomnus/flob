package flob

import (
	"errors"
	"net/http"
)

// Action identifies the store operation an HTTP request performs.
type Action string

const (
	ActionRead  Action = "read"  // GET and HEAD
	ActionAdd   Action = "add"   // POST
	ActionLabel Action = "label" // PATCH
	ActionErase Action = "erase" // DELETE
)

var (
	ErrUnauthenticated = errors.New("unauthenticated")
	ErrForbidden       = errors.New("forbidden")
)

// HTTPAuthorizer checks a request before storage access or reading its body.
// Store is the decoded, canonical namespace ID. Return ErrUnauthenticated for
// invalid/missing credentials, ErrForbidden for insufficient permissions, and
// another error when authentication is temporarily unavailable.
type HTTPAuthorizer interface {
	Authorize(r *http.Request, store string, action Action) error
}

func (h HttpHandler) authorize(w http.ResponseWriter, r *http.Request, store string) bool {
	if h.Authorizer == nil {
		return true
	}
	var action Action
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		action = ActionRead
	case http.MethodPost:
		action = ActionAdd
	case http.MethodPatch:
		action = ActionLabel
	case http.MethodDelete:
		action = ActionErase
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return false
	}
	err := h.Authorizer.Authorize(r, store, action)
	switch {
	case err == nil:
		return true
	case errors.Is(err, ErrUnauthenticated):
		w.Header().Set("WWW-Authenticate", `Bearer realm="flob"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	case errors.Is(err, ErrForbidden):
		http.Error(w, "forbidden", http.StatusForbidden)
	default:
		// Never expose verifier errors, claims or credentials in responses.
		http.Error(w, "authentication unavailable", http.StatusServiceUnavailable)
	}
	return false
}
