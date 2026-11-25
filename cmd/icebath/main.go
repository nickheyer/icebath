package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"connectrpc.com/connect/v2"

	"github.com/nickheyer/icebath/internal/app"
	"github.com/nickheyer/icebath/internal/cli"
	"github.com/nickheyer/icebath/internal/client"
	"github.com/nickheyer/icebath/internal/config"
	configapp "github.com/nickheyer/icebath/internal/config/app"
	configserver "github.com/nickheyer/icebath/internal/config/server"
	"github.com/nickheyer/icebath/internal/credentials"
	"github.com/nickheyer/icebath/internal/models"
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults"
	"github.com/nickheyer/icebath/internal/provider"
	providerapp "github.com/nickheyer/icebath/internal/provider/app"
	providercli "github.com/nickheyer/icebath/internal/provider/cli"
	providerserver "github.com/nickheyer/icebath/internal/provider/server"
	"github.com/nickheyer/icebath/internal/server"
	"github.com/nickheyer/icebath/internal/sessions"
	sessionsserver "github.com/nickheyer/icebath/internal/sessions/server"
	"github.com/nickheyer/icebath/internal/slash"
	"github.com/nickheyer/icebath/internal/state"
	statecli "github.com/nickheyer/icebath/internal/state/cli"
	"github.com/nickheyer/icebath/internal/tools"
	"github.com/nickheyer/icebath/internal/transport"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	context.AfterFunc(ctx, stop) // a second signal kills
	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "icebath:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) (err error) {
	userDir, err := os.UserConfigDir()
	if err != nil {
		return err
	}
	dir := filepath.Join(userDir, "icebath")

	st, err := state.Load(dir)
	if err != nil {
		return err
	}

	cfg := config.New(dir)
	srv := server.New(cfg)
	a := app.New()
	c := cli.New()
	p := provider.New()
	t := transport.New()
	cr := credentials.New()
	m := models.New()
	tl := tools.New(cfg)
	s := sessions.New(cfg)
	sl := slash.New()
	statecli.Register(c, st)
	configserver.Register(srv, cfg)
	configapp.Register(a)
	providerserver.Register(srv, p, cr, m)
	providerapp.Register(a)
	providercli.Register(c)
	sessionsserver.Register(srv, cfg, s)
	defaults.Register(plugins.New(cfg, srv, a, c, p, t, cr, m, tl, s, sl))
	defer func() {
		err = errors.Join(err, srv.Close(), s.Close())
	}()

	if len(args) > 0 && args[0] == "server" {
		if len(args) > 1 {
			return errors.New("server takes no arguments")
		}
		if err := cfg.Load(); err != nil {
			return err
		}
		return srv.Serve(ctx)
	}

	var conn *connect.Client
	if st.Backend != "" {
		conn = client.Remote(st.Backend)
	} else {
		conn = client.InProcess(srv.Services())
		if err := cfg.Load(); err != nil {
			return err
		}
	}

	if len(args) == 0 {
		return a.Run(ctx, conn)
	}
	return c.Run(ctx, conn, args)
}
