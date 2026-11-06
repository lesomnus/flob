package auth

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/lesomnus/flob"
)

func TestOIDCRejectsUnsignedAndSymmetricTokens(t *testing.T) {
	p := newTestIssuer(t)
	policy := p.policy(t)
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.HS256, Key: []byte(strings.Repeat("x", 32))}, (&jose.SignerOptions{}).WithHeader("kid", "k1"))
	if err != nil {
		t.Fatal(err)
	}
	symmetric, err := jwt.Signed(signer).Claims(p.claims()).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	claims, err := json.Marshal(p.claims())
	if err != nil {
		t.Fatal(err)
	}
	unsigned := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","kid":"k1"}`)) + "." + base64.RawURLEncoding.EncodeToString(claims) + "."
	for _, raw := range []string{symmetric, unsigned} {
		if err := policy.Authorize(bearer(raw), "_", flob.ActionAdd); !errors.Is(err, flob.ErrUnauthenticated) {
			t.Fatal(err)
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.keyRequests != 0 {
		t.Fatal("unsupported algorithms fetched keys")
	}
}

func TestOIDCRefreshAndFailureRecovery(t *testing.T) {
	p := newTestIssuer(t)
	policy := p.policy(t)
	now := time.Now()
	policy.providers[0].verifier.now = func() time.Time { return now }
	raw := sign(t, p.key, "k1", p.claims())
	if err := policy.Authorize(bearer(raw), "_", flob.ActionRead); err != nil {
		t.Fatal(err)
	}
	// Repeated unknown IDs cannot cause unbounded refreshes; known cached keys work.
	for range 5 {
		bad := sign(t, p.key, "unknown", p.claims())
		if err := policy.Authorize(bearer(bad), "_", flob.ActionRead); !errors.Is(err, flob.ErrUnauthenticated) {
			t.Fatal(err)
		}
	}
	p.mu.Lock()
	if p.keyRequests != 1 {
		t.Fatal(p.keyRequests)
	}
	p.fail = true
	p.mu.Unlock()
	if err := policy.Authorize(bearer(raw), "_", flob.ActionRead); err != nil {
		t.Fatal(err)
	}
	// Cached keys are not trusted forever after the provider becomes unavailable.
	now = now.Add(keyLifetime + time.Second)
	err := policy.Authorize(bearer(raw), "_", flob.ActionRead)
	if err == nil || errors.Is(err, flob.ErrUnauthenticated) {
		t.Fatalf("want provider outage, got %v", err)
	}
	p.mu.Lock()
	p.fail = false
	p.mu.Unlock()
	if err := policy.Authorize(bearer(raw), "_", flob.ActionRead); err == nil {
		t.Fatal("refresh cooldown ignored")
	}
	now = now.Add(refreshInterval)
	if err := policy.Authorize(bearer(raw), "_", flob.ActionRead); err != nil {
		t.Fatal(err)
	}
	// Rotate keys, then ensure the previous key is no longer accepted.
	newKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	p.keys = []jose.JSONWebKey{{Key: &newKey.PublicKey, KeyID: "k2", Algorithm: "RS256", Use: "sig"}}
	p.mu.Unlock()
	now = now.Add(refreshInterval)
	if err := policy.Authorize(bearer(sign(t, newKey, "k2", p.claims())), "_", flob.ActionRead); err != nil {
		t.Fatal(err)
	}
	if err := policy.Authorize(bearer(raw), "_", flob.ActionRead); !errors.Is(err, flob.ErrUnauthenticated) {
		t.Fatal(err)
	}
}

func TestOIDCDiscoveryAndKeyRestrictions(t *testing.T) {
	for _, name := range []string{"wrong issuer", "insecure jwks", "oversized jwks", "duplicate kid", "encryption key", "private key", "wrong key algorithm"} {
		t.Run(name, func(t *testing.T) {
			p := newTestIssuer(t)
			switch name {
			case "wrong issuer":
				p.discoveryIssuer = "https://other.example"
			case "insecure jwks":
				p.jwksURI = "http://other.example/jwks"
			case "oversized jwks":
				p.oversized = true
			case "duplicate kid":
				p.keys = append(p.keys, p.keys[0])
			case "encryption key":
				p.keys[0].Use = "enc"
			case "private key":
				p.keys[0].Key = p.key
			case "wrong key algorithm":
				p.keys[0].Algorithm = "RS512"
			}
			policy := p.policy(t)
			if err := policy.Authorize(bearer(sign(t, p.key, "k1", p.claims())), "_", flob.ActionRead); err == nil {
				t.Fatal("accepted invalid provider/key metadata")
			}
		})
	}
}

func TestOIDCMetadataCannotRedirectToHTTP(t *testing.T) {
	var hits atomic.Int32
	insecure := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1); w.WriteHeader(200) }))
	defer insecure.Close()
	p := newTestIssuer(t)
	redirect := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, insecure.URL, http.StatusFound) }))
	defer redirect.Close()
	c := p.config()
	v := c.Providers["github"]
	v.Issuer = redirect.URL
	c.Providers["github"] = v
	policy, err := New(c, redirect.Client())
	if err != nil {
		t.Fatal(err)
	}
	claims := p.claims()
	claims["iss"] = redirect.URL
	if err := policy.Authorize(bearer(sign(t, p.key, "k1", claims)), "_", flob.ActionRead); err == nil {
		t.Fatal("accepted cleartext redirect")
	}
	if hits.Load() != 0 {
		t.Fatal("contacted cleartext endpoint")
	}
}

func TestPolicyWildcardStoreAndExactClaims(t *testing.T) {
	p := newTestIssuer(t)
	c := p.config()
	permission := c.Permissions["publish"]
	permission.Stores = []string{"*"}
	c.Permissions["publish"] = permission
	policy, err := New(c, p.server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if err := policy.Authorize(bearer(sign(t, p.key, "k1", p.claims())), "any-namespace", flob.ActionAdd); err != nil {
		t.Fatal(err)
	}
	claims := p.claims()
	claims["workflow_ref"] = "acme/assets/.github/workflows/publish.yml@refs/heads/main-extra"
	if err := policy.Authorize(bearer(sign(t, p.key, "k1", claims)), "_", flob.ActionAdd); !errors.Is(err, flob.ErrForbidden) {
		t.Fatal(err)
	}
}
