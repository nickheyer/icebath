package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"

	"github.com/nickheyer/icebath/internal/config"
	serverconfig "github.com/nickheyer/icebath/internal/server/config"
)

type Registrant interface {
	Mount(services *connect.Server)
}

type Registry struct {
	registrants []Registrant
}

func (r *Registry) Register(registrant Registrant) {
	r.registrants = append(r.registrants, registrant)
}

func (r *Registry) Mount(services *connect.Server) {
	for _, registrant := range r.registrants {
		registrant.Mount(services)
	}
}

func (r *Registry) Close() error {
	var errs []error
	for _, registrant := range slices.Backward(r.registrants) {
		if closer, ok := registrant.(io.Closer); ok {
			errs = append(errs, closer.Close())
		}
	}
	return errors.Join(errs...)
}

type Server struct {
	Registry
	config *config.Config
}

func New(cfg *config.Config) *Server {
	serverconfig.Register(cfg)
	return &Server{config: cfg}
}

func (s *Server) Services() *connect.Server {
	services := connect.NewServer(public)
	s.Mount(services)
	return services
}

func public(next connect.ServerFunc) connect.ServerFunc {
	return func(ctx context.Context, spec connect.Spec, stream connect.ServerStream) error {
		err := next(ctx, spec, stream)
		if _, ok := errors.AsType[*connect.Error](err); ok || err == nil {
			return err
		}
		return connect.NewError(connect.CodeUnknown, err.Error()).WithCause(err)
	}
}

func (s *Server) Serve(ctx context.Context) error {
	listener, err := net.Listen("tcp", s.config.GetString(serverconfig.Address))
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	connecthttp.Mount(mux, s.Services())
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)
	httpServer := &http.Server{
		Handler:           mux,
		Protocols:         protocols,
		ReadHeaderTimeout: 10 * time.Second,
	}

	shutdown := make(chan error, 1)
	go func() {
		<-ctx.Done()
		shutdown <- httpServer.Shutdown(context.Background())
	}()
	slog.Info("serving", "address", listener.Addr().String())
	if err := httpServer.Serve(listener); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return <-shutdown
}
