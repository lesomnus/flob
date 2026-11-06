package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/lesomnus/flob"
)

const (
	keyLifetime     = 5 * time.Minute
	refreshInterval = 10 * time.Second
	clockSkew       = 30 * time.Second
	metadataLimit   = 1 << 20
)

var algorithms = []jose.SignatureAlgorithm{
	jose.RS256, jose.RS384, jose.RS512,
	jose.PS256, jose.PS384, jose.PS512,
	jose.ES256, jose.ES384, jose.ES512, jose.EdDSA,
}

type oidcVerifier struct {
	issuer, audience string
	client           *http.Client
	now              func() time.Time
	mu               sync.Mutex
	keys             jose.JSONWebKeySet
	fetched, tried   time.Time
	jwksURI          string
}

func secureURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Hostname() != "" &&
		u.User == nil && u.Fragment == ""
}

func newOIDC(c ProviderConfig, client *http.Client) (*oidcVerifier, error) {
	u, err := url.Parse(c.Issuer)
	if err != nil || !secureURL(c.Issuer) || u.RawQuery != "" || u.ForceQuery || c.Audience == "" {
		return nil, errors.New("an HTTPS issuer without query/fragment and an audience are required")
	}
	hc := *http.DefaultClient
	if client != nil {
		hc = *client
	}
	if hc.Timeout == 0 || hc.Timeout > 10*time.Second {
		hc.Timeout = 10 * time.Second
	}
	// Metadata may redirect, but credentials or cleartext endpoints are never used.
	previous := hc.CheckRedirect
	hc.CheckRedirect = func(r *http.Request, via []*http.Request) error {
		if !secureURL(r.URL.String()) || len(via) >= 10 {
			return errors.New("unsafe OIDC metadata redirect")
		}
		if previous != nil {
			return previous(r, via)
		}
		return nil
	}
	return &oidcVerifier{issuer: c.Issuer, audience: c.Audience, client: &hc, now: time.Now}, nil
}

func (o *oidcVerifier) verify(ctx context.Context, raw string) (map[string]string, error) {
	token, err := jwt.ParseSigned(raw, algorithms)
	if err != nil || len(token.Headers) != 1 || token.Headers[0].KeyID == "" {
		return nil, flob.ErrUnauthenticated
	}
	var peek map[string]json.RawMessage
	// Unverified iss only selects a configured provider. No token-provided URL
	// (including jku/x5u) is fetched and no unverified claim authorizes anything.
	if err := token.UnsafeClaimsWithoutVerification(&peek); err != nil {
		return nil, flob.ErrUnauthenticated
	}
	var issuer string
	if json.Unmarshal(peek["iss"], &issuer) != nil || issuer != o.issuer {
		return nil, flob.ErrUnauthenticated
	}
	key, err := o.key(ctx, token.Headers[0].KeyID)
	if err != nil {
		return nil, err
	}
	if key.Algorithm != "" && key.Algorithm != token.Headers[0].Algorithm {
		return nil, flob.ErrUnauthenticated
	}
	var standard jwt.Claims
	var verified map[string]json.RawMessage
	if err := token.Claims(key.Key, &verified); err != nil {
		return nil, flob.ErrUnauthenticated
	}
	// JWT claim names are case-sensitive. Decode standard claims only from
	// exact keys, avoiding encoding/json's case-insensitive struct field matching.
	standardFields := map[string]json.RawMessage{}
	for _, name := range []string{"iss", "sub", "aud", "exp", "iat", "nbf"} {
		if value, ok := verified[name]; ok {
			standardFields[name] = value
		}
	}
	encoded, err := json.Marshal(standardFields)
	if err != nil || json.Unmarshal(encoded, &standard) != nil {
		return nil, flob.ErrUnauthenticated
	}
	if standard.Expiry == nil || standard.IssuedAt == nil || standard.Subject == "" {
		return nil, flob.ErrUnauthenticated
	}
	if err := standard.ValidateWithLeeway(jwt.Expected{
		Issuer: o.issuer, AnyAudience: jwt.Audience{o.audience}, Time: o.now(),
	}, clockSkew); err != nil {
		return nil, flob.ErrUnauthenticated
	}
	claims := map[string]string{}
	for name, value := range verified {
		var text string
		if json.Unmarshal(value, &text) == nil {
			claims[name] = text
		}
	}
	return claims, nil
}

func signingKey(keys jose.JSONWebKeySet, kid string) (jose.JSONWebKey, bool) {
	// Ambiguous kid values are refused instead of picking an arbitrary key.
	matching := keys.Key(kid)
	if len(matching) != 1 || !matching[0].Valid() || !matching[0].IsPublic() ||
		(matching[0].Use != "" && matching[0].Use != "sig") {
		return jose.JSONWebKey{}, false
	}
	return matching[0], true
}

func (o *oidcVerifier) key(ctx context.Context, kid string) (jose.JSONWebKey, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	now := o.now()
	if key, ok := signingKey(o.keys, kid); ok && now.Sub(o.fetched) < keyLifetime {
		return key, nil
	}
	if !o.tried.IsZero() && now.Sub(o.tried) < refreshInterval {
		if o.fetched.IsZero() || now.Sub(o.fetched) >= keyLifetime {
			return jose.JSONWebKey{}, errors.New("OIDC metadata refresh is unavailable")
		}
		return jose.JSONWebKey{}, flob.ErrUnauthenticated
	}
	o.tried = now
	if o.jwksURI == "" {
		var discovery struct {
			Issuer  string `json:"issuer"`
			JwksURI string `json:"jwks_uri"`
		}
		if err := o.get(ctx, strings.TrimRight(o.issuer, "/")+"/.well-known/openid-configuration", &discovery); err != nil {
			return jose.JSONWebKey{}, err
		}
		if discovery.Issuer != o.issuer || !secureURL(discovery.JwksURI) {
			return jose.JSONWebKey{}, errors.New("invalid OIDC discovery issuer or JWKS URI")
		}
		o.jwksURI = discovery.JwksURI
	}
	var keys jose.JSONWebKeySet
	if err := o.get(ctx, o.jwksURI, &keys); err != nil {
		return jose.JSONWebKey{}, err
	}
	o.keys, o.fetched = keys, now
	if key, ok := signingKey(keys, kid); ok {
		return key, nil
	}
	return jose.JSONWebKey{}, flob.ErrUnauthenticated
}

func (o *oidcVerifier) get(ctx context.Context, target string, value any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	response, err := o.client.Do(req)
	if err != nil {
		return fmt.Errorf("fetch OIDC metadata: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("OIDC metadata returned status %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, metadataLimit+1))
	if err != nil || len(body) > metadataLimit {
		return errors.New("cannot read bounded OIDC metadata")
	}
	if err := json.Unmarshal(body, value); err != nil {
		return fmt.Errorf("invalid OIDC metadata: %w", err)
	}
	return nil
}
