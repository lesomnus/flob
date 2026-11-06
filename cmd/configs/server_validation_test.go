package configs

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"
	"github.com/lesomnus/flob"
)

func TestServerMisspelledAuthMustNotCreatePublicServer(t *testing.T) {
	doc := strings.Replace(oidcServerYAML, "\nauth:", "\nauthh:", 1)
	var c ServerConfig
	if err := yaml.Unmarshal([]byte(doc), &c); err != nil {
		return
	}
	if err := c.Evaluate(); err != nil {
		return
	}
	h, err := c.Handler(flob.NewMemStores())
	if err != nil {
		return
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/_", strings.NewReader("unauthenticated bytes")))
	t.Fatalf("auth typo accepted without an error: Authorizer=%T, unauthenticated POST status=%d", h.Authorizer, w.Code)
}
