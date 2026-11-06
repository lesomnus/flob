// Package auth authorizes flob HTTP operations using OIDC job/workflow claims.
package auth

import (
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"

	"github.com/lesomnus/flob"
)

// Config is an opt-in, deny-by-default policy. Matches compare verified string
// claims exactly; permissions name decoded namespaces (or "*") and operations.
type Config struct {
	Providers   map[string]ProviderConfig   `yaml:"providers"`
	Permissions map[string]PermissionConfig `yaml:"permissions"`
	Matches     map[string]MatchConfig      `yaml:"matches"`
}

type ProviderConfig struct {
	Kind     string `yaml:"kind"`
	Issuer   string `yaml:"issuer"`
	Audience string `yaml:"audience"`
}

type PermissionConfig struct {
	Stores  []string      `yaml:"stores"`
	Actions []flob.Action `yaml:"actions"`
}

type MatchConfig struct {
	For   string            `yaml:"for"`
	When  map[string]string `yaml:"when"`
	Grant []string          `yaml:"grant"`
}

type permission struct {
	stores  []string
	actions []flob.Action
}

type match struct {
	when   map[string]string
	grants []permission
}

type provider struct {
	verifier *oidcVerifier
	matches  []match
}

// Policy is immutable after construction and safe for concurrent requests.
type Policy struct {
	providers []provider
}

var _ flob.HTTPAuthorizer = (*Policy)(nil)

// New validates the whole policy without fetching provider metadata. With no
// credentials, no provider is contacted. An empty policy is an error, not an
// unauthenticated server. client may customize TLS roots for a private issuer;
// nil uses the default HTTP transport. Issuers/JWKS must always use HTTPS.
func New(c Config, client *http.Client) (*Policy, error) {
	if len(c.Providers) == 0 || len(c.Permissions) == 0 || len(c.Matches) == 0 {
		return nil, errors.New("auth: providers, permissions and matches are required")
	}
	permissions := map[string]permission{}
	for name, v := range c.Permissions {
		if name == "" || len(v.Stores) == 0 || len(v.Actions) == 0 {
			return nil, fmt.Errorf("auth: permission %q requires stores and actions", name)
		}
		for _, store := range v.Stores {
			if store == "" {
				return nil, fmt.Errorf("auth: permission %q has an empty store", name)
			}
		}
		for _, action := range v.Actions {
			switch action {
			case flob.ActionRead, flob.ActionAdd, flob.ActionLabel, flob.ActionErase:
			default:
				return nil, fmt.Errorf("auth: permission %q has unknown action %q", name, action)
			}
		}
		permissions[name] = permission{slices.Clone(v.Stores), slices.Clone(v.Actions)}
	}
	// Stable ordering also makes provider selection and diagnostics deterministic.
	names := slices.Sorted(maps.Keys(c.Providers))
	byName := map[string]int{}
	p := &Policy{}
	for _, name := range names {
		v := c.Providers[name]
		if name == "" || v.Kind != "oidc" {
			return nil, fmt.Errorf("auth: provider %q requires kind oidc", name)
		}
		o, err := newOIDC(v, client)
		if err != nil {
			return nil, fmt.Errorf("auth: provider %q: %w", name, err)
		}
		byName[name] = len(p.providers)
		p.providers = append(p.providers, provider{verifier: o})
	}
	for _, name := range slices.Sorted(maps.Keys(c.Matches)) {
		v := c.Matches[name]
		i, ok := byName[v.For]
		if !ok || name == "" || len(v.When) == 0 || len(v.Grant) == 0 {
			return nil, fmt.Errorf("auth: match %q requires a known provider, nonempty when and grant", name)
		}
		for k, value := range v.When {
			if k == "" || value == "" {
				return nil, fmt.Errorf("auth: match %q has an empty claim condition", name)
			}
		}
		m := match{when: maps.Clone(v.When)}
		for _, grant := range v.Grant {
			permission, ok := permissions[grant]
			if !ok {
				return nil, fmt.Errorf("auth: match %q grants unknown permission %q", name, grant)
			}
			m.grants = append(m.grants, permission)
		}
		p.providers[i].matches = append(p.providers[i].matches, m)
	}
	return p, nil
}

// Authorize accepts only a single Bearer ID token. A valid token alone grants
// nothing: a matching provider/claim rule and store/action grant are required.
func (p *Policy) Authorize(r *http.Request, store string, action flob.Action) error {
	values := r.Header.Values("Authorization")
	if len(values) != 1 || len(values[0]) > 64<<10 {
		return flob.ErrUnauthenticated
	}
	parts := strings.Fields(values[0])
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return flob.ErrUnauthenticated
	}
	authenticated := false
	var unavailable error
	for _, provider := range p.providers {
		claims, err := provider.verifier.verify(r.Context(), parts[1])
		if err != nil {
			if !errors.Is(err, flob.ErrUnauthenticated) {
				unavailable = err
			}
			continue
		}
		authenticated = true
		for _, m := range provider.matches {
			if !matches(claims, m.when) {
				continue
			}
			for _, grant := range m.grants {
				if slices.Contains(grant.actions, action) &&
					(slices.Contains(grant.stores, store) || slices.Contains(grant.stores, "*")) {
					return nil
				}
			}
		}
	}
	if authenticated {
		return flob.ErrForbidden
	}
	if unavailable != nil {
		return unavailable
	}
	return flob.ErrUnauthenticated
}

func matches(claims map[string]string, when map[string]string) bool {
	for name, want := range when {
		got, ok := claims[name]
		if !ok || got != want {
			return false
		}
	}
	return true
}
