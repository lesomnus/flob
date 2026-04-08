package cmd

import (
	"context"
	"errors"
	"os"

	"github.com/goccy/go-yaml"
	"github.com/lesomnus/flob/cmd/flob/configs"
	"github.com/lesomnus/otx/log"
	"github.com/lesomnus/xli"
	"github.com/lesomnus/xli/flg"
	"github.com/lesomnus/xli/mode"
	"github.com/lesomnus/z"
)

var use_config = z.NewUse[*Config]()

type Config struct {
	path string

	Stores configs.StoresConfig
	Otel   configs.OtelConfig `yaml:",omitempty"`
}

func (c *Config) Sanitize() error {
	return errors.Join()
}

func configHandler() xli.Handler {
	path_to_lookup := []string{
		"./flob.yaml",
		"./flob.yml",
	}

	return xli.OnF(func(m mode.Mode) bool {
		return m == mode.Run|mode.Pass || m == mode.Run
	}, func(ctx context.Context, cmd *xli.Command, next xli.Next) error {
		var conf_path string
		if !flg.VisitP(cmd, "conf", &conf_path) {
			for _, p := range path_to_lookup {
				if _, err := os.Stat(p); err == nil {
					conf_path = p
					break
				}
			}
		}

		c := &Config{}
		if conf_path == "" {
			c.path = "/dev/null"
		} else if err := readConfigFile(ctx, c, conf_path); err != nil {
			return z.Err(err, "read config")
		}

		ctx, otx, err := c.Otel.Build(ctx)
		if err != nil {
			return z.Err(err, "build otel")
		}
		defer otx.Shutdown(ctx)

		l := log.From(ctx)
		l.Info("config loaded", "path", c.path)

		ctx = use_config.Into(ctx, c)
		return next(ctx)
	})
}

func readConfigFile(ctx context.Context, c *Config, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return z.Err(err, "open")
	}
	defer f.Close()

	if err := yaml.NewDecoder(f).DecodeContext(ctx, c); err != nil {
		return z.Err(err, "decode")
	}

	c.path = path
	return nil
}
