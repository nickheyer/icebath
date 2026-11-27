package server

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strings"
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

// Settings of the profile a request names, the one in use when it names none; a profile that is not there is not found
func Profile(cfg *config.Config, name string) (*config.Config, error) {
	found, err := cfg.Of(strings.TrimSpace(name))
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err.Error())
	}
	return found, nil
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
	address := s.config.GetString(serverconfig.Address)
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return fmt.Errorf("icebath can't serve at %s: %w. Stop the program that uses the address, or change Address in /settings under Server.", address, err)
	}
	token := s.config.GetString(serverconfig.Token)
	if token == "" {
		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			return err
		}
		token = hex.EncodeToString(raw)
		if err := s.config.Set(serverconfig.Token, token); err != nil {
			return err
		}
	}
	mux := http.NewServeMux()
	connecthttp.Mount(mux, s.Services())
	guarded := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, password, ok := r.BasicAuth(); !ok || subtle.ConstantTimeCompare([]byte(password), []byte(token)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="icebath"`)
			http.Error(w, "The password is wrong. The password of this server is in /settings under Server.", http.StatusUnauthorized)
			return
		}
		mux.ServeHTTP(w, r)
	})
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)
	httpServer := &http.Server{
		Handler:           guarded,
		Protocols:         protocols,
		ReadHeaderTimeout: 10 * time.Second,
	}

	shutdown := make(chan error, 1)
	go func() {
		<-ctx.Done()
		shutdown <- httpServer.Shutdown(context.Background())
	}()
	slog.Info("serving", "address", listener.Addr().String(), "connect", "icebath connect http://icebath:"+token+"@"+listener.Addr().String())
	if err := httpServer.Serve(listener); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return <-shutdown
}
