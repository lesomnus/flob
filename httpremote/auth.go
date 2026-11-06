package httpremote

import (
	"encoding/base64"
	"fmt"
	"strings"
)

// Auth selects an HTTP authentication scheme. Value is read for each actual
// request, including redirects; it must be safe for concurrent use. Bearer
// expects a token, and basic expects the unencoded username:password pair.
// Providers can resolve literals, environment references or rotating files
// without coupling the HTTP client to a configuration package.
type Auth struct {
	Kind  string
	Value func() (string, error)
}

func (a Auth) header() (string, error) {
	value, err := a.Value()
	if err != nil {
		return "", err
	}
	switch a.Kind {
	case "bearer":
		if value == "" || strings.IndexFunc(value, func(r rune) bool { return r <= ' ' || r >= 127 }) >= 0 {
			return "", fmt.Errorf("invalid bearer token")
		}
		return "Bearer " + value, nil
	case "basic":
		if !strings.Contains(value, ":") || strings.IndexFunc(value, func(r rune) bool { return r < ' ' || r == 127 }) >= 0 {
			return "", fmt.Errorf("basic authentication requires a username:password pair without control characters")
		}
		return "Basic " + base64.StdEncoding.EncodeToString([]byte(value)), nil
	default:
		return "", fmt.Errorf("unsupported authentication kind %q", a.Kind)
	}
}
