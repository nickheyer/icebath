package server

import (
	"context"
	"path/filepath"

	"connectrpc.com/connect/v2"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/server"
	"github.com/nickheyer/icebath/internal/sessions"
	sessionsconfig "github.com/nickheyer/icebath/internal/sessions/config"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1/sessionsv1connect"
)

type service struct {
	config   *config.Config
	sessions *sessions.Sessions
}

func Register(host *server.Server, cfg *config.Config, s *sessions.Sessions) {
	host.Register(&service{config: cfg, sessions: s})
}

func (s *service) Mount(services *connect.Server) {
	sessionsv1connect.RegisterSessionServiceHandler(services, s)
}

func (s *service) store() (sessions.Store, error) {
	return s.sessions.Store(s.config.GetString(sessionsconfig.Store))
}

func (s *service) Create(_ context.Context, request *sessionsv1.CreateRequest) (*sessionsv1.CreateResponse, error) {
	if !filepath.IsAbs(request.GetDirectory()) {
		return nil, connect.NewError(connect.CodeInvalidArgument, "directory must be an absolute path")
	}
	store, err := s.store()
	if err != nil {
		return nil, err
	}
	session, err := store.Create(request.GetDirectory())
	if err != nil {
		return nil, err
	}
	return &sessionsv1.CreateResponse{Session: session}, nil
}

func (s *service) List(context.Context, *sessionsv1.ListRequest) (*sessionsv1.ListResponse, error) {
	store, err := s.store()
	if err != nil {
		return nil, err
	}
	list, err := store.List()
	if err != nil {
		return nil, err
	}
	return &sessionsv1.ListResponse{Sessions: list}, nil
}

func (s *service) Get(_ context.Context, request *sessionsv1.GetRequest) (*sessionsv1.GetResponse, error) {
	store, err := s.store()
	if err != nil {
		return nil, err
	}
	session, err := store.Get(request.GetId())
	if err != nil {
		return nil, err
	}
	messages, err := store.Messages(request.GetId())
	if err != nil {
		return nil, err
	}
	return &sessionsv1.GetResponse{Session: session, Messages: messages}, nil
}

func (s *service) Rename(_ context.Context, request *sessionsv1.RenameRequest) (*sessionsv1.RenameResponse, error) {
	store, err := s.store()
	if err != nil {
		return nil, err
	}
	if err := store.Rename(request.GetId(), request.GetTitle()); err != nil {
		return nil, err
	}
	return &sessionsv1.RenameResponse{}, nil
}

func (s *service) Delete(_ context.Context, request *sessionsv1.DeleteRequest) (*sessionsv1.DeleteResponse, error) {
	store, err := s.store()
	if err != nil {
		return nil, err
	}
	if err := store.Delete(request.GetId()); err != nil {
		return nil, err
	}
	return &sessionsv1.DeleteResponse{}, nil
}
