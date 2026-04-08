package cmd

import (
	"context"
	"io"
	"os"

	"github.com/lesomnus/flob"
	"github.com/lesomnus/xli"
	"github.com/lesomnus/xli/arg"
	"github.com/lesomnus/z"
)

func NewCmdAdd() *xli.Command {
	return &xli.Command{
		Name: "add",

		Args: arg.Args{
			&arg.String{Name: "STORE_ID"},
			&arg.String{Name: "FILE"},
		},

		Handler: WithStores(func(ctx context.Context, cmd *xli.Command, s flob.Stores) error {
			id := arg.MustGet[string](cmd, "STORE_ID")
			p := arg.MustGet[string](cmd, "FILE")

			f, err := os.Open(p)
			if err != nil {
				return z.Err(err, "open file")
			}
			defer f.Close()

			m, err := s.Use(id).Add(ctx, flob.Meta{}, f)
			if err != nil {
				return z.Err(err, "op")
			}

			cmd.Println(m.Digest)
			return nil
		}),
	}
}

func NewCmdGet() *xli.Command {
	return &xli.Command{
		Name: "get",

		Args: arg.Args{
			&arg.String{Name: "STORE_ID"},
			&arg.String{Name: "DIGEST"},
		},

		Handler: WithStores(func(ctx context.Context, cmd *xli.Command, s flob.Stores) error {
			id := arg.MustGet[string](cmd, "STORE_ID")
			d_ := arg.MustGet[string](cmd, "DIGEST")

			d, err := flob.Digest(d_).Sanitize()
			if err != nil {
				return z.Err(err, "invalid digest")
			}

			m, err := s.Use(id).Get(ctx, d)
			if err != nil {
				return z.Err(err, "op")
			}

			cmd.Println("Digest:", m.Digest)
			cmd.Println("Size:", m.Size)
			if len(m.Labels) > 0 {
				cmd.Println("Labels:")
				for k, v := range m.Labels {
					cmd.Println("  ", k, "=", v)
				}
			}
			return nil
		}),
	}
}

func NewCmdRead() *xli.Command {
	return &xli.Command{
		Name: "read",

		Args: arg.Args{
			&arg.String{Name: "STORE_ID"},
			&arg.String{Name: "DIGEST"},
		},

		Handler: WithStores(func(ctx context.Context, cmd *xli.Command, s flob.Stores) error {
			id := arg.MustGet[string](cmd, "STORE_ID")
			d_ := arg.MustGet[string](cmd, "DIGEST")

			d, err := flob.Digest(d_).Sanitize()
			if err != nil {
				return z.Err(err, "invalid digest")
			}

			f, _, err := s.Use(id).Open(ctx, d)
			if err != nil {
				return z.Err(err, "op")
			}
			defer f.Close()

			_, err = io.Copy(cmd, f)
			return err
		}),
	}
}
