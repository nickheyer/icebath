package server

import (
	"context"

	"connectrpc.com/connect/v2"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/generation"
	generationconfig "github.com/nickheyer/icebath/internal/generation/config"
	"github.com/nickheyer/icebath/internal/server"
	generatev1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/generate/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/plugins/generate/v1/generatev1connect"
)

type service struct {
	config   *config.Config
	services *generation.Generation
}

func Register(registry *server.Registry, cfg *config.Config, services *generation.Generation) {
	registry.Register(&service{config: cfg, services: services})
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

func (s *service) List(_ context.Context, request *generatev1.ListRequest) (*generatev1.ListResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	k := kind(request.GetKind())
	active, failed := s.services.Active(cfg, k)
	response := &generatev1.ListResponse{Chosen: generation.Chosen(cfg, k), Model: generation.Named(cfg, k), Enabled: cfg.GetBool(generationconfig.VideoEnabled)}
	switch {
	case failed != nil:
		response.Reason = failed.Error()
	case !active.Set(cfg):
		response.Reason = active.Title + " is chosen but not set up: its key or sign-in is missing."
	case k == generation.Video && !response.Enabled && response.Chosen == "":
		response.Reason = "Video generation is off: choose a video service, or turn on video_gen.enabled."
	default:
		response.Offered = true
	}
	for _, svc := range s.services.Making(k) {
		entry := &generatev1.Service{Name: svc.Name, Title: svc.Title, Description: svc.About(k), Badge: svc.Setup.Badge, Ready: svc.Set(cfg),
			Chosen: svc.Name == response.Chosen, Active: failed == nil && svc.Name == active.Name, Key: svc.Setup.Key, Prompt: svc.Setup.Prompt,
			Link: svc.Setup.Link, Provider: svc.Setup.Provider, ModelSetting: svc.ModelSetting(k)}
		entry.Model = cfg.GetString(entry.ModelSetting)
		if k == generation.Video {
			entry.Settings, entry.Notes = svc.Videos.Settings, svc.Videos.Takes(cfg).Notes
		} else {
			entry.Settings, entry.Notes = svc.Images.Settings, svc.Images.Takes(cfg).Notes
		}
		response.Services = append(response.Services, entry)
	}
	return response, nil
}

func (s *service) Models(ctx context.Context, request *generatev1.ModelsRequest) (*generatev1.ModelsResponse, error) {
	cfg, err := server.Profile(s.config, request.GetProfile())
	if err != nil {
		return nil, err
	}
	k := kind(request.GetKind())
	svc, ok := s.services.Service(k, request.GetService())
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, "no generation service named "+request.GetService())
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
