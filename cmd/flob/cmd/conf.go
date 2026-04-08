package cmd

import (
	"context"

	"github.com/goccy/go-yaml"
	"github.com/lesomnus/xli"
	"github.com/lesomnus/z"
)

func NewCmdConf() *xli.Command {
	return &xli.Command{
		Name: "conf",
		Handler: xli.OnRun(func(ctx context.Context, cmd *xli.Command, next xli.Next) error {
			c := use_config.Must(ctx)
			data, err := yaml.MarshalWithOptions(c, yaml.Indent(2))
			if err != nil {
				return z.Err(err, "marshal")
			}

			cmd.Println(string(data))
			return nil
		}),
	}
}
