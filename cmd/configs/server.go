package configs

import (
	"errors"
	"fmt"

	"github.com/goccy/go-yaml"
	"github.com/goccy/go-yaml/ast"
	"github.com/lesomnus/flob"
	"github.com/lesomnus/flob/auth"
	"github.com/lesomnus/xddr"
	"github.com/lesomnus/z"
)

type ServerConfig struct {
	Use  string
	Addr xddr.HTTPLocal
	// Redirect serves blob downloads by redirecting to a backend presigned URL
	// when the store supports it (e.g. S3), instead of streaming through the
	// server. Stores without presign support stream as usual.
	Redirect      bool     `yaml:",omitempty"`
	ReadOnly      bool     `yaml:"read_only,omitempty"`
	AllowedStores []string `yaml:"allowed_stores,omitempty"`
	// Auth is opt-in; omitted preserves unauthenticated serving. Explicit null,
	// empty or invalid policies are rejected instead of disabling authentication.
	Auth *auth.Config `yaml:"auth,omitempty"`
}

func (c *ServerConfig) UnmarshalYAML(unmarshal func(any) error) error {
	type plain ServerConfig
	var parsed plain
	if err := unmarshal(&parsed); err != nil {
		return err
	}
	var fields map[string]ast.Node
	if err := unmarshal(&fields); err != nil {
		return err
	}
	// The auth key itself must be checked too: silently ignoring "authh"
	// would otherwise turn a protected configuration into a public server.
	for name := range fields {
		switch name {
		case "use", "addr", "redirect", "read_only", "allowed_stores", "auth":
		default:
			return fmt.Errorf("server: unknown field %q", name)
		}
	}
	if node, present := fields["auth"]; present {
		if node == nil || node.Type() == ast.NullType || parsed.Auth == nil {
			return errors.New("server.auth cannot be null; omit it for unauthenticated serving")
		}
		raw, err := node.MarshalYAML()
		if err != nil {
			return err
		}
		// A typo in the opt-in auth block must never silently discard a restriction.
		if err := yaml.UnmarshalWithOptions(raw, parsed.Auth, yaml.Strict()); err != nil {
			return fmt.Errorf("server.auth: %w", err)
		}
	}
	*c = ServerConfig(parsed)
	return nil
}

func (c *ServerConfig) Evaluate() error {
	z.FallbackP(&c.Use, "mem")
	z.FallbackP(&c.Addr, "0.0.0.0:8080")
	_, authErr := c.authorizer()

	return errors.Join(
		z.ErrIf(z.Take(c.Addr.Sanitize()).To(&c.Addr), ".addr"),
		authErr,
	)
}

func (c *ServerConfig) authorizer() (flob.HTTPAuthorizer, error) {
	if c.Auth == nil {
		return nil, nil
	}
	p, err := auth.New(*c.Auth, nil)
	if err != nil {
		return nil, fmt.Errorf("server.auth: %w", err)
	}
	return p, nil
}

// Handler builds the HTTP handler, including the policy cache. Invalid auth
// configurations return an error even if Evaluate has not been called.
func (c *ServerConfig) Handler(stores flob.Stores) (flob.HttpHandler, error) {
	authorizer, err := c.authorizer()
	if err != nil {
		return flob.HttpHandler{}, err
	}
	return flob.HttpHandler{
		Stores: stores, Redirect: c.Redirect, ReadOnly: c.ReadOnly,
		AllowedStores: c.AllowedStores, Authorizer: authorizer,
	}, nil
}
