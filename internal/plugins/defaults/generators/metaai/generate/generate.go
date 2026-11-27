package generate

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/credentials"
	"github.com/nickheyer/icebath/internal/generation"
	metaaiconfig "github.com/nickheyer/icebath/internal/plugins/defaults/generators/metaai/config"
	metaaiprovider "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/metaai/config"
	"github.com/nickheyer/icebath/internal/provider"
	"github.com/nickheyer/icebath/pkg/strs"
)

type services struct {
	providers *provider.Registry
	creds     *credentials.Registry
}

func Register(registry *generation.Registry, providers *provider.Registry, creds *credentials.Registry) {
	registry.Register(services{providers, creds})
}

// Key and address of the profile's Meta provider or Meta key
func (s services) route(cfg *config.Config) (provider.Route, bool) {
	route, ok := s.creds.Kind(s.providers, cfg, metaaiprovider.Name)
	route.BaseURL = strings.TrimRight(strs.Filled(route.BaseURL, metaaiprovider.BaseURL), "/")
	route.Provider, route.Spec = metaaiprovider.Name, metaaiprovider.Name
	return route, ok
}

func (s services) Services() []generation.Service {
	return []generation.Service{{
		Name:  metaaiconfig.Name,
		Title: "Meta AI",
		Ready: func(cfg *config.Config) bool { _, ok := s.route(cfg); return ok },
		Setup: generation.Setup{Badge: "paid", Prompt: "Meta Model API key", Link: "https://api.meta.ai", Provider: metaaiprovider.Name},
		Images: &generation.Images{
			About:    "Images from Meta's Muse model, needs a Meta API key",
			Settings: []string{metaaiconfig.Model},
			Model:    metaaiconfig.Model,
			Models: func(context.Context, *config.Config) []generation.Model {
				return []generation.Model{{ID: metaaiconfig.DefaultModel, Title: "Muse Image 1.0", Speed: "About 10 seconds", Strengths: "Images from the Meta Model API", Price: "$0.01 an image"}}
			},
			Default:  func(context.Context, *config.Config) string { return metaaiconfig.DefaultModel },
			Takes:    func(*config.Config) generation.ImageTakes { return generation.ImageTakes{} },
			Generate: s.draw,
		},
	}}
}

// Model the call draws with: the one image_gen.meta-ai.model names as it is, else the default, which Image model may name too
func model(cfg *config.Config) string {
	return strs.Filled(cfg.GetString(metaaiconfig.Model), metaaiconfig.DefaultModel)
}

// Draws from text through Meta's OpenAI compatible images API and keeps the WebP picture in the cache
func (s services) draw(ctx context.Context, job *generation.Job, request generation.ImageRequest) generation.Result {
	cfg := job.Config
	prompt, aspect := strings.TrimSpace(request.Prompt), generation.Aspect(request.Aspect)
	if prompt == "" {
		return generation.Call{Provider: metaaiconfig.Name, Aspect: aspect}.Fail("Prompt is required and must be a non-empty string", "invalid_argument")
	}
	route, ok := s.route(cfg)
	if !ok {
		return generation.Call{Provider: metaaiconfig.Name, Aspect: aspect}.Fail(metaaiprovider.MetaModelAPIKeyEnv+" not set. Connect Meta in /providers, or pick "+
			"another image service with /imagegen.", "auth_required")
	}
	id := model(cfg)
	size := generation.Size(aspect)
	call := generation.Call{Provider: metaaiconfig.Name, Model: id, Prompt: prompt, Aspect: aspect}
	reply, err := generation.OpenAI{BaseURL: route.BaseURL, Key: route.APIKey}.Images(ctx, map[string]any{"model": id, "prompt": prompt, "size": size, "n": 1})
	if err != nil {
		slog.Debug("Meta image generation failed", "error", err)
		return call.Fail(fmt.Sprintf("Meta image generation failed: %v", err), "api_error")
	}
	data := generation.List(reply, "data")
	if len(data) == 0 {
		return call.Fail("Meta response contained no image data", "empty_response")
	}
	first := data[0]
	var path string
	switch b64, address := generation.Text(first, "b64_json"), generation.Text(first, "url"); {
	case b64 != "":
		path, err = job.Unpack(generation.Image, b64, "meta", "webp")
	case address != "":
		path, err = job.Download(ctx, generation.Image, address, generation.Fetch{Prefix: "meta", Origin: route.BaseURL})
	default:
		return call.Fail("Meta response contained neither b64_json nor URL", "empty_response")
	}
	if err != nil {
		return call.Fail(fmt.Sprintf("Failed to save Meta image: %v", err), "io_error")
	}
	extra := map[string]any{"size": size}
	if revised := generation.Text(first, "revised_prompt"); revised != "" {
		extra["revised_prompt"] = revised
	}
	return call.Made(path, "text", extra)
}
