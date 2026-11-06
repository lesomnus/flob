package configs

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"
	"github.com/lesomnus/flob"
	"github.com/lesomnus/flob/auth"
)

const oidcServerYAML = `
use: os/public
auth:
  providers:
    github:
      kind: oidc
      issuer: https://token.actions.githubusercontent.com
      audience: blob.example.com
  permissions:
    publish:
      stores: ["_"]
      actions: [read, add]
  matches:
    publish:
      for: github
      when:
        repository_id: "123"
        workflow_ref: acme/assets/.github/workflows/publish.yml@refs/heads/main
        ref: refs/heads/main
      grant: [publish]
`

func TestServerOIDCConfigWiresAuthentication(t *testing.T) {
	var c ServerConfig
	if err := yaml.Unmarshal([]byte(oidcServerYAML), &c); err != nil {
		t.Fatal(err)
	}
	if err := c.Evaluate(); err != nil {
		t.Fatal(err)
	}
	h, err := c.Handler(flob.NewMemStores())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := h.Authorizer.(*auth.Policy); !ok {
		t.Fatalf("authorizer=%T", h.Authorizer)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/_", strings.NewReader("unauthenticated upload")))
	if w.Code != 401 {
		t.Fatalf("unauthenticated upload=%d: %s", w.Code, w.Body.String())
	}
}

func TestServerRejectsInvalidAuthInsteadOfDisablingIt(t *testing.T) {
	for _, doc := range []string{
		"auth: null\n", "auth:\n", "auth: {}\n",
		strings.Replace(oidcServerYAML, "audience: blob.example.com", "audience_typo: blob.example.com", 1),
		strings.Replace(oidcServerYAML, "actions: [read, add]", "actions: [push]", 1),
		strings.Replace(oidcServerYAML, "grant: [publish]", "grant: [unknown]", 1),
		strings.Replace(oidcServerYAML, "kind: oidc", "kind: oidc\n      unexpected: true", 1),
	} {
		var c ServerConfig
		if err := yaml.Unmarshal([]byte(doc), &c); err != nil {
			continue
		}
		if err := c.Evaluate(); err == nil {
			t.Fatalf("accepted invalid auth:\n%s", doc)
		}
		if _, err := c.Handler(flob.NewMemStores()); err == nil {
			t.Fatal("handler ignored invalid authentication")
		}
	}
}

func TestServerOmittedAuthRemainsCompatible(t *testing.T) {
	var c ServerConfig
	if err := yaml.Unmarshal([]byte("use: mem\n"), &c); err != nil {
		t.Fatal(err)
	}
	h, err := c.Handler(flob.NewMemStores())
	if err != nil {
		t.Fatal(err)
	}
	if h.Authorizer != nil {
		t.Fatal("omitted auth enabled authentication")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/_", strings.NewReader("legacy upload")))
	if w.Code != 201 {
		t.Fatalf("legacy upload=%d: %s", w.Code, w.Body.String())
	}
}
