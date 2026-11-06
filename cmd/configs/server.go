package configs

import (
	"errors"

	"github.com/lesomnus/xddr"
	"github.com/lesomnus/z"
)

type ServerConfig struct {
	Use  string
	Addr xddr.HTTPLocal
	// Redirect serves blob downloads by redirecting to a backend presigned URL
	// when the store supports it (e.g. S3), instead of streaming through the
	// server. Stores without presign support stream as usual.
	Redirect bool `yaml:",omitempty"`
}

func (c *ServerConfig) Evaluate() error {
	z.FallbackP(&c.Use, "mem")
	z.FallbackP(&c.Addr, "0.0.0.0:8080")

	return errors.Join(
		z.ErrIf(z.Take(c.Addr.Sanitize()).To(&c.Addr), ".addr"),
	)
}
