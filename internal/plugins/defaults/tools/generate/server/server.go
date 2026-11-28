package server

import (
	"context"

	"connectrpc.com/connect/v2"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/generation"
	"github.com/nickheyer/icebath/internal/provider"
	"github.com/nickheyer/icebath/internal/server"
	generatev1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/generate/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/plugins/generate/v1/generatev1connect"
)

type service struct {
	config    *config.Config
	services  *generation.Generation
	providers *provider.Registry
}

func Register(registry *server.Registry, cfg *config.Config, services *generation.Generation, providers *provider.Registry) {
	registry.Register(&service{config: cfg, services: services, providers: providers})
}

func (s *service) Mount(services *connect.Server) {
	generatev1connect.RegisterGenerateServiceHandler(services, s)
}

// Kind a request names, pictures unless it names videos
func kind(k generatev1.Kind) generation.Kind {
	if k == generatev1.Kind_KIND_VIDEO {
		return generation.Video
	}
	return generation.Image
}

var states = map[generation.State]generatev1.State{
	generation.InUse:        generatev1.State_STATE_IN_USE,
	generation.Ready:        generatev1.State_STATE_READY,
	generation.Free:         generatev1.State_STATE_FREE,
	generation.NeedsKey:     generatev1.State_STATE_NEEDS_KEY,
	generation.NotConnected: generatev1.State_STATE_NOT_CONNECTED,
}

func (s *service) List(_ context.Context, request *generatev1.ListRequest) (*generatev1.ListResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	k := kind(request.GetKind())
	if request.GetBare() {
		response := &generatev1.ListResponse{}
		for _, svc := range s.services.Making(k) {
			response.Services = append(response.Services, &generatev1.Service{Name: svc.Name, Title: svc.Title, Description: svc.About(k),
				Section: svc.Section(k), ModelSetting: svc.ModelSetting(k), Key: svc.Setup.Key})
		}
		return response, nil
	}
	response := &generatev1.ListResponse{Chosen: generation.Chosen(cfg, k), On: generation.On(cfg, k), Hindrance: s.services.Hindrance(cfg, k)}
	serving := s.services.Serving(cfg, k)
	for _, svc := range s.services.Making(k) {
		state := s.services.State(cfg, k, svc)
		entry := &generatev1.Service{Name: svc.Name, Title: svc.Title, Description: svc.About(k), State: states[state], Words: state.String(),
			Chosen: svc.Name == response.Chosen, Section: svc.Section(k), ModelSetting: svc.ModelSetting(k), Key: svc.Setup.Key, Link: svc.Setup.Link}
		if svc.Name == serving {
			response.Serving = svc.Title
		}
		s.account(cfg, svc, entry)
		if k == generation.Video {
			entry.Notes = svc.Videos.Takes(cfg).Notes
		} else {
			entry.Notes = svc.Images.Takes(cfg).Notes
		}
		response.Services = append(response.Services, entry)
	}
	return response, nil
}

// The provider of the profile whose account a service uses, the first of its kinds the profile has; the kind a person adds when it has none
func (s *service) account(cfg *config.Config, svc generation.Service, entry *generatev1.Service) {
	kinds := svc.Setup.Providers
	if len(kinds) == 0 {
		return
	}
	entry.ProviderKind = kinds[0]
	for _, k := range kinds {
		for _, instance := range s.providers.Instances(cfg) {
			if instance.Known && instance.Spec.Name == k {
				entry.Provider, entry.ProviderTitle = instance.ID, instance.Label()
				return
			}
		}
	}
	if spec, ok := s.providers.Spec(kinds[0]); ok {
		entry.ProviderTitle = spec.DisplayName
	}
}

func (s *service) Models(ctx context.Context, request *generatev1.ModelsRequest) (*generatev1.ModelsResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	k := kind(request.GetKind())
	svc, ok := s.services.Service(k, request.GetService())
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, "No image or video service is called “"+request.GetService()+"”.")
	}
	var models []generation.Model
	response := &generatev1.ModelsResponse{}
	if k == generation.Video {
		models, response.Default = svc.Videos.Models(ctx, cfg), svc.Videos.Default(ctx, cfg)
	} else {
		models, response.Default = svc.Images.Models(ctx, cfg), svc.Images.Default(ctx, cfg)
	}
	for _, m := range models {
		response.Models = append(response.Models, &generatev1.Model{Id: m.ID, Title: m.Title, Speed: m.Speed, Strengths: m.Strengths, Price: m.Price,
			Modalities: m.Modalities, MinDuration: int32(m.MinDuration), MaxDuration: int32(m.MaxDuration)})
	}
	return response, nil
}

func (s *service) Choose(_ context.Context, request *generatev1.ChooseRequest) (*generatev1.ChooseResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	k := kind(request.GetKind())
	if err := s.services.Choose(cfg, k, request.GetService()); err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err.Error())
	}
	return &generatev1.ChooseResponse{Note: s.services.Note(cfg, k, generation.Chosen(cfg, k))}, nil
}

func (s *service) Videos(_ context.Context, request *generatev1.VideosRequest) (*generatev1.ChooseResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	if err := generation.SetVideos(cfg, request.GetOn()); err != nil {
		return nil, err
	}
	note := "Make videos is now Off."
	if request.GetOn() {
		note = "Make videos is now On."
	}
	return &generatev1.ChooseResponse{Note: note}, nil
}
