package cmd

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/lesomnus/flob"
	"github.com/lesomnus/flob/cmd/configs"
	"github.com/lesomnus/otx"
	"github.com/lesomnus/otx/log"
	"github.com/lesomnus/otx/otxhttp"
	"github.com/lesomnus/xddr"
	"github.com/lesomnus/xli"
	"github.com/lesomnus/z"
)

func NewCmdServe() *xli.Command {
	return &xli.Command{
		Name: "serve",
		Handler: xli.OnRun(func(ctx context.Context, cmd *xli.Command, next xli.Next) error {
			c := UseConfig.Must(ctx)
			s, err := c.Stores.Use(ctx, c.Server.Use)
			if err != nil {
				return z.Err(err, "use stores: %s", c.Server.Use)
			}

			s, err = configs.NewStoresMeter(ctx, s)
			if err != nil {
				return z.Err(err, "new stores meter")
			}

			l := log.From(ctx)

			h := flob.HttpHandler{Stores: s, Redirect: c.Server.Redirect}
			mux := http.NewServeMux()
			x := otx.From(ctx)
			mux.Handle("/", otxhttp.NewHandler(x, otxhttp.BoundaryLogger(x)(h), "/"))

			// Listen on the configured address (server.addr, e.g. "tcp4:0.0.0.0:8087";
			// defaults to "0.0.0.0:8080" — see configs.ServerConfig.Evaluate). Split
			// into network + address by xddr so a non-default port actually takes
			// effect instead of the old hardcoded ":8080".
			l.Info("serve", slog.String("addr", string(c.Server.Addr)))
			ln, err := xddr.Listen(c.Server.Addr)
			if err != nil {
				return z.Err(err, "listen: %s", c.Server.Addr)
			}
			defer ln.Close()
			if err := http.Serve(ln, mux); err != nil {
				return z.Err(err, "start http server")
			}

			return nil
		}),
	}
}
