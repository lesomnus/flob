package auth

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/lesomnus/flob"
)

type testIssuer struct {
	server                   *httptest.Server
	key                      *rsa.PrivateKey
	mu                       sync.Mutex
	keys                     []jose.JSONWebKey
	keyRequests              int
	discoveryIssuer, jwksURI string
	fail, oversized          bool
}

func newTestIssuer(t *testing.T) *testIssuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p := &testIssuer{key: key, keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}}
	mux := http.NewServeMux()
	p.server = httptest.NewTLSServer(mux)
	t.Cleanup(p.server.Close)
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		issuer, uri := p.discoveryIssuer, p.jwksURI
		if issuer == "" {
			issuer = p.server.URL
		}
		if uri == "" {
			uri = p.server.URL + "/jwks"
		}
		json.NewEncoder(w).Encode(map[string]string{"issuer": issuer, "jwks_uri": uri})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.keyRequests++
		if p.fail {
			w.WriteHeader(503)
			return
		}
		if p.oversized {
			io.WriteString(w, strings.Repeat(" ", metadataLimit+1))
			return
		}
		json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: p.keys})
	})
	return p
}

func (p *testIssuer) claims() map[string]any {
	now := time.Now()
	return map[string]any{"iss": p.server.URL, "aud": "blob.example.com", "sub": "repo:acme/assets:ref:refs/heads/main",
		"iat": now.Add(-time.Second).Unix(), "nbf": now.Add(-time.Second).Unix(), "exp": now.Add(time.Hour).Unix(),
		"repository_id": "123", "workflow_ref": "acme/assets/.github/workflows/publish.yml@refs/heads/main", "ref": "refs/heads/main"}
}

func sign(t *testing.T, key *rsa.PrivateKey, kid string, claims map[string]any) string {
	t.Helper()
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", kid))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := jwt.Signed(signer).Claims(claims).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func (p *testIssuer) config() Config {
	return Config{
		Providers:   map[string]ProviderConfig{"github": {Kind: "oidc", Issuer: p.server.URL, Audience: "blob.example.com"}},
		Permissions: map[string]PermissionConfig{"publish": {Stores: []string{"_"}, Actions: []flob.Action{flob.ActionRead, flob.ActionAdd}}},
		Matches:     map[string]MatchConfig{"release": {For: "github", When: map[string]string{"repository_id": "123", "workflow_ref": "acme/assets/.github/workflows/publish.yml@refs/heads/main", "ref": "refs/heads/main"}, Grant: []string{"publish"}}},
	}
}

func (p *testIssuer) policy(t *testing.T) *Policy {
	t.Helper()
	policy, err := New(p.config(), p.server.Client())
	if err != nil {
		t.Fatal(err)
	}
	return policy
}

func bearer(token string) *http.Request {
	r := httptest.NewRequest("GET", "/_/sha256:"+strings.Repeat("0", 64), nil)
	r.Header.Set("Authorization", "Bearer "+token)
	return r
}

func TestOIDCProtectedHTTPStore(t *testing.T) {
	p := newTestIssuer(t)
	policy := p.policy(t)
	stores := flob.NewMemStores()
	server := httptest.NewServer(flob.HttpHandler{Stores: stores, Authorizer: policy})
	defer server.Close()
	token := sign(t, p.key, "k1", p.claims())
	request := func(method, path, token, body string, want int) []byte {
		t.Helper()
		r, err := http.NewRequest(method, server.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		res, err := server.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, err := io.ReadAll(res.Body)
		if err != nil {
			t.Fatal(err)
		}
		if res.StatusCode != want {
			t.Fatalf("%s %s = %d: %s", method, path, res.StatusCode, b)
		}
		return b
	}
	request("POST", "/_", "", "untrusted", 401)
	request("POST", "/_", token, "published bytes", 201)
	digest := sha256.Sum256([]byte("published bytes"))
	path := "/_/sha256:" + hex.EncodeToString(digest[:])
	if body := request("GET", path, token, "", 200); string(body) != "published bytes" {
		t.Fatalf("body=%q", body)
	}
	request("HEAD", path, token, "", 200)
	request("POST", "/private", token, "private bytes", 403)
	request("PATCH", path, token, "", 403)
	request("DELETE", path, token, "", 403)
	request("GET", path, token, "", 200)
	// The same canonical namespace is authorized after URL decoding.
	request("GET", "/%5f/sha256:"+hex.EncodeToString(digest[:]), token, "", 200)
	// The configured HTTP backend sends the same OIDC ID token using the
	// public constructor, for writes, metadata and streamed range reads.
	client, err := flob.NewHttpStores(flob.HttpConfig{
		Targets: []string{server.URL},
		Auth: map[string]flob.HTTPAuth{server.URL: {
			Kind: "bearer", Value: func() (string, error) { return token, nil },
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	m, err := client.Use("_").Add(t.Context(), flob.Meta{}, strings.NewReader("configured client bytes"))
	if err != nil {
		t.Fatal(err)
	}
	r, _, err := client.Use("_").Open(t.Context(), m.Digest)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	r.Close()
	if err != nil || string(got) != "configured client bytes" {
		t.Fatalf("configured client download = %q, %v", got, err)
	}
	badJob := p.claims()
	badJob["workflow_ref"] = "acme/assets/.github/workflows/untrusted.yml@refs/heads/main"
	request("POST", "/_", sign(t, p.key, "k1", badJob), "untrusted workflow", 403)
	if _, err := stores.Use("private").Stat(t.Context(), flob.Digest("sha256:"+hex.EncodeToString(digest[:]))); !errors.Is(err, flob.ErrNotExist) {
		t.Fatal(err)
	}
}

func TestOIDCClaimsAndCredentials(t *testing.T) {
	p := newTestIssuer(t)
	policy := p.policy(t)
	for _, tc := range []struct {
		name, key string
		value     any
		remove    bool
		want      error
	}{
		{"wrong audience", "aud", "somewhere-else", false, flob.ErrUnauthenticated},
		{"wrong issuer", "iss", "https://untrusted.example", false, flob.ErrUnauthenticated},
		{"expired", "exp", time.Now().Add(-time.Hour).Unix(), false, flob.ErrUnauthenticated},
		{"future nbf", "nbf", time.Now().Add(time.Hour).Unix(), false, flob.ErrUnauthenticated},
		{"future iat", "iat", time.Now().Add(time.Hour).Unix(), false, flob.ErrUnauthenticated},
		{"no expiry", "exp", nil, true, flob.ErrUnauthenticated},
		{"no issued at", "iat", nil, true, flob.ErrUnauthenticated},
		{"no audience", "aud", nil, true, flob.ErrUnauthenticated},
		{"no subject", "sub", nil, true, flob.ErrUnauthenticated},
		{"wrong repository", "repository_id", "456", false, flob.ErrForbidden},
		{"missing repository", "repository_id", nil, true, flob.ErrForbidden},
		{"numeric claim", "repository_id", 123, false, flob.ErrForbidden},
		{"array claim", "repository_id", []string{"123"}, false, flob.ErrForbidden},
		{"wrong ref", "ref", "refs/heads/feature", false, flob.ErrForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claims := p.claims()
			if tc.remove {
				delete(claims, tc.key)
			} else {
				claims[tc.key] = tc.value
			}
			err := policy.Authorize(bearer(sign(t, p.key, "k1", claims)), "_", flob.ActionAdd)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err=%v want=%v", err, tc.want)
			}
		})
	}
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	for name, raw := range map[string]string{"malformed": "not-a-token", "forged": sign(t, other, "k1", p.claims()), "no kid": sign(t, p.key, "", p.claims())} {
		t.Run(name, func(t *testing.T) {
			if err := policy.Authorize(bearer(raw), "_", flob.ActionRead); !errors.Is(err, flob.ErrUnauthenticated) {
				t.Fatal(err)
			}
		})
	}
	valid := sign(t, p.key, "k1", p.claims())
	for _, header := range []string{"", "Basic " + valid, "Bearer " + valid + " extra", strings.Repeat("x", (64<<10)+1)} {
		r := bearer(valid)
		r.Header.Set("Authorization", header)
		if err := policy.Authorize(r, "_", flob.ActionRead); !errors.Is(err, flob.ErrUnauthenticated) {
			t.Fatal(err)
		}
	}
	r := bearer(valid)
	r.Header.Add("Authorization", "Bearer "+valid)
	if err := policy.Authorize(r, "_", flob.ActionRead); !errors.Is(err, flob.ErrUnauthenticated) {
		t.Fatal(err)
	}
	// An audience array may contain the configured audience.
	claims := p.claims()
	claims["aud"] = []string{"other", "blob.example.com"}
	if err := policy.Authorize(bearer(sign(t, p.key, "k1", claims)), "_", flob.ActionRead); err != nil {
		t.Fatal(err)
	}
}

func TestOIDCStandardClaimNamesAreCaseSensitive(t *testing.T) {
	p := newTestIssuer(t)
	policy := p.policy(t)
	for _, key := range []string{"iss", "aud", "sub", "exp", "iat"} {
		claims := p.claims()
		claims[strings.ToUpper(key)] = claims[key]
		delete(claims, key)
		if err := policy.Authorize(bearer(sign(t, p.key, "k1", claims)), "_", flob.ActionRead); !errors.Is(err, flob.ErrUnauthenticated) {
			t.Fatalf("accepted uppercase %s: %v", key, err)
		}
	}
	claims := p.claims()
	claims["exp"] = time.Now().Add(-time.Hour).Unix()
	claims["EXP"] = time.Now().Add(time.Hour).Unix()
	if err := policy.Authorize(bearer(sign(t, p.key, "k1", claims)), "_", flob.ActionRead); !errors.Is(err, flob.ErrUnauthenticated) {
		t.Fatalf("uppercase expiry bypassed expiration: %v", err)
	}
}

func TestPolicyRejectsInvalidConfigurations(t *testing.T) {
	p := newTestIssuer(t)
	for name, change := range map[string]func(*Config){
		"empty":        func(c *Config) { *c = Config{} },
		"unknown kind": func(c *Config) { v := c.Providers["github"]; v.Kind = "jwt"; c.Providers["github"] = v },
		"http issuer": func(c *Config) {
			v := c.Providers["github"]
			v.Issuer = "http://issuer.example"
			c.Providers["github"] = v
		},
		"no audience": func(c *Config) { v := c.Providers["github"]; v.Audience = ""; c.Providers["github"] = v },
		"issuer userinfo": func(c *Config) {
			v := c.Providers["github"]
			v.Issuer = "https://user:password@issuer.example"
			c.Providers["github"] = v
		},
		"issuer query": func(c *Config) {
			v := c.Providers["github"]
			v.Issuer = "https://issuer.example?query=x"
			c.Providers["github"] = v
		},
		"empty permission": func(c *Config) { c.Permissions["publish"] = PermissionConfig{} },
		"empty store":      func(c *Config) { v := c.Permissions["publish"]; v.Stores = []string{""}; c.Permissions["publish"] = v },
		"unknown action": func(c *Config) {
			v := c.Permissions["publish"]
			v.Actions = []flob.Action{"push"}
			c.Permissions["publish"] = v
		},
		"unknown provider": func(c *Config) { v := c.Matches["release"]; v.For = "typo"; c.Matches["release"] = v },
		"no claims":        func(c *Config) { v := c.Matches["release"]; v.When = nil; c.Matches["release"] = v },
		"empty claim":      func(c *Config) { v := c.Matches["release"]; v.When["ref"] = ""; c.Matches["release"] = v },
		"unknown grant":    func(c *Config) { v := c.Matches["release"]; v.Grant = []string{"typo"}; c.Matches["release"] = v },
		"no grants":        func(c *Config) { v := c.Matches["release"]; v.Grant = nil; c.Matches["release"] = v },
	} {
		t.Run(name, func(t *testing.T) {
			c := p.config()
			change(&c)
			if _, err := New(c, p.server.Client()); err == nil {
				t.Fatal("accepted invalid auth configuration")
			}
		})
	}
}

func TestPolicyCopiesConfigurationAndConcurrentVerification(t *testing.T) {
	p := newTestIssuer(t)
	c := p.config()
	policy, err := New(c, p.server.Client())
	if err != nil {
		t.Fatal(err)
	}
	c.Permissions["publish"].Stores[0] = "private"
	c.Permissions["publish"].Actions[0] = flob.ActionErase
	c.Matches["release"].When["repository_id"] = "456"
	raw := sign(t, p.key, "k1", p.claims())
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := policy.Authorize(bearer(raw), "_", flob.ActionRead); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.keyRequests != 1 {
		t.Fatalf("JWKS requests=%d", p.keyRequests)
	}
	if err := policy.Authorize(bearer(raw), "private", flob.ActionRead); !errors.Is(err, flob.ErrForbidden) {
		t.Fatal(err)
	}
}

func TestExplicitMutationGrants(t *testing.T) {
	p := newTestIssuer(t)
	c := p.config()
	v := c.Permissions["publish"]
	v.Actions = append(v.Actions, flob.ActionLabel, flob.ActionErase)
	c.Permissions["publish"] = v
	policy, err := New(c, p.server.Client())
	if err != nil {
		t.Fatal(err)
	}
	stores := flob.NewMemStores()
	m, err := stores.Use("_").Add(t.Context(), flob.Meta{}, strings.NewReader("data"))
	if err != nil {
		t.Fatal(err)
	}
	h := flob.HttpHandler{Stores: stores, Authorizer: policy}
	raw := sign(t, p.key, "k1", p.claims())
	for _, method := range []string{"PATCH", "DELETE"} {
		r := httptest.NewRequest(method, "/_/"+string(m.Digest), nil)
		r.Header.Set("Authorization", "Bearer "+raw)
		r.Header.Set("Flob-Color", "blue")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code < 200 || w.Code >= 300 {
			t.Fatalf("%s=%d %s", method, w.Code, w.Body.String())
		}
	}
	if _, err := stores.Use("_").Stat(t.Context(), m.Digest); !errors.Is(err, flob.ErrNotExist) {
		t.Fatal(err)
	}
}

func TestVerifiedClaimsMustNotMatchAsPatterns(t *testing.T) {
	p := newTestIssuer(t)
	c := p.config()
	m := c.Matches["release"]
	m.When = maps.Clone(m.When)
	m.When["workflow_ref"] = "acme/*"
	c.Matches["release"] = m
	policy, err := New(c, p.server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if err := policy.Authorize(bearer(sign(t, p.key, "k1", p.claims())), "_", flob.ActionAdd); !errors.Is(err, flob.ErrForbidden) {
		t.Fatal(err)
	}
}
