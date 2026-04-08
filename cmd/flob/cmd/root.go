package cmd

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/lesomnus/flob"
	"github.com/lesomnus/otx/log"
	"github.com/lesomnus/xli"
	"github.com/lesomnus/z"
)

func NewCmdRoot() *xli.Command {
	return &xli.Command{
		Name: "flob",

		Commands: []*xli.Command{
			NewCmdConf(),
			NewCmdAdd(),
			NewCmdGet(),
			NewCmdRead(),
		},

		Handler: xli.Chain(
			configHandler(),
			WithStores(func(ctx context.Context, cmd *xli.Command, s flob.Stores) error {
				l := log.From(ctx)

				mux := http.NewServeMux()
				mux.Handle("/", flob.HttpHandler{Stores: s})

				l.Info("serve", slog.String("addr", ":8080"))
				if err := http.ListenAndServe(":8080", mux); err != nil {
					return z.Err(err, "start http server")
				}

				return nil
			}),
		),
	}
}

func WithStores(f func(ctx context.Context, cmd *xli.Command, s flob.Stores) error) xli.Handler {
	return xli.OnRun(func(ctx context.Context, cmd *xli.Command, next xli.Next) error {
		c := use_config.Must(ctx)

		s, err := c.Stores.Build()
		if err != nil {
			return z.Err(err, "build stores")
		}
		if err := f(ctx, cmd, s); err != nil {
			return z.Err(err, "run command")
		}
		return next(ctx)
	})
}
