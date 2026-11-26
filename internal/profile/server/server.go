package server

import (
	"context"

	"connectrpc.com/connect/v2"

	"github.com/nickheyer/icebath/internal/profile"
	"github.com/nickheyer/icebath/internal/server"
	profilev1 "github.com/nickheyer/icebath/pkg/gen/icebath/profile/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/profile/v1/profilev1connect"
)

type service struct {
	profiles *profile.Profiles
}

func Register(host *server.Server, p *profile.Profiles) {
	host.Register(&service{profiles: p})
}

func (s *service) Mount(services *connect.Server) {
	profilev1connect.RegisterProfileServiceHandler(services, s)
}

func (s *service) message(name string) *profilev1.Profile {
	return &profilev1.Profile{Name: name, Active: name == s.profiles.Active(), Default: name == profile.Default}
}

func (s *service) List(context.Context, *profilev1.ListRequest) (*profilev1.ListResponse, error) {
	names, err := s.profiles.List()
	if err != nil {
		return nil, err
	}
	response := &profilev1.ListResponse{}
	for _, name := range names {
		response.Profiles = append(response.Profiles, s.message(name))
	}
	return response, nil
}

func (s *service) Create(_ context.Context, request *profilev1.CreateRequest) (*profilev1.CreateResponse, error) {
	if err := s.profiles.Create(request.GetName(), request.GetFrom()); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err.Error())
	}
	return &profilev1.CreateResponse{Profile: s.message(request.GetName())}, nil
}

func (s *service) Delete(_ context.Context, request *profilev1.DeleteRequest) (*profilev1.DeleteResponse, error) {
	if err := s.profiles.Delete(request.GetName()); err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err.Error())
	}
	return &profilev1.DeleteResponse{}, nil
}

func (s *service) Use(_ context.Context, request *profilev1.UseRequest) (*profilev1.UseResponse, error) {
	if err := s.profiles.Use(request.GetName()); err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err.Error())
	}
	return &profilev1.UseResponse{Profile: s.message(request.GetName())}, nil
}
