package generate

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/generation"
	openaiconfig "github.com/nickheyer/icebath/internal/plugins/defaults/generators/openai/config"
	openaiapiconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/openaiapi/config"
	"github.com/nickheyer/icebath/internal/provider"
	"github.com/nickheyer/icebath/pkg/strs"
)

// Address and key OpenAI image requests take: the address set for them, else that of the provider image_gen.openai.provider names, else the
// OpenAI provider's; the key in the variable image_gen.openai.key_env names, else the named provider's, else the OpenAI key
func (s services) endpoint(cfg *config.Config) provider.Route {
	var named provider.Route
	if name := strings.TrimSpace(cfg.GetString(openaiconfig.Provider)); name != "" {
		if instance, ok := s.providers.Instance(cfg, name); ok {
			if route, err := s.creds.Route(instance, ""); err == nil {
				named = route
			}
		}
	}
	own, _ := s.creds.Kind(s.providers, cfg, openaiapiconfig.Name)
	route := provider.Route{Provider: openaiconfig.Name, Spec: openaiapiconfig.Name}
	route.BaseURL = strings.TrimRight(cmp.Or(strings.TrimSpace(cfg.GetString(openaiconfig.BaseURL)), named.BaseURL, own.BaseURL,
		strings.TrimSpace(cfg.Getenv(openaiapiconfig.BaseURLEnv)), generation.OpenAIBase), "/")
	if variable := strings.TrimSpace(cfg.GetString(openaiconfig.KeyEnv)); variable != "" {
		route.APIKey = strings.TrimSpace(cfg.Getenv(variable))
	}
	route.APIKey = cmp.Or(route.APIKey, named.APIKey, own.APIKey)
	return route
}

// Draws or edits through the OpenAI images API and keeps the picture in the cache
func (s services) draw(ctx context.Context, job *generation.Job, request generation.ImageRequest) generation.Result {
	cfg := job.Config
	prompt, aspect := strings.TrimSpace(request.Prompt), generation.Aspect(request.Aspect)
	if prompt == "" {
		return generation.Call{Provider: openaiconfig.Name, Aspect: aspect}.Fail("Prompt is required and must be a non-empty string", "invalid_argument")
	}
	if name := strings.TrimSpace(cfg.GetString(openaiconfig.Provider)); name != "" {
		if _, ok := s.providers.Instance(cfg, name); !ok {
			slog.Warn(fmt.Sprintf("image_gen.openai.provider '%s' matches no provider of the profile", name))
		}
	}
	route := s.endpoint(cfg)
	if route.APIKey == "" {
		return generation.Call{Provider: openaiconfig.Name, Aspect: aspect}.Fail("OPENAI_API_KEY not set (or the variable named by image_gen.openai.key_env is "+
			"empty). Connect OpenAI in /providers, or pick another image service with /imagegen.", "auth_required")
	}
	t := pick(models, cfg.GetString(openaiconfig.Model), true, request.Model)
	call := generation.Call{Provider: openaiconfig.Name, Model: t.id, Prompt: prompt, Aspect: aspect}
	size := generation.Size(aspect)
	client := generation.OpenAI{BaseURL: route.BaseURL, Key: route.APIKey, Headers: map[string]string{}}
	if org := strings.TrimSpace(cfg.Getenv("OPENAI_ORG_ID")); org != "" {
		client.Headers["OpenAI-Organization"] = org
	}
	body := map[string]any{"model": t.api, "prompt": prompt, "size": size, "n": 1}
	if t.quality != "" {
		body["quality"] = t.quality
	}
	sources := request.Sources(16)
	var reply map[string]any
	var err error
	if len(sources) > 0 {
		var uploads []generation.Upload
		for _, source := range sources {
			data, name, err := job.Read(ctx, source)
			if err != nil {
				return call.Fail(fmt.Sprintf("Could not load source image for editing: %v", err), "io_error")
			}
			uploads = append(uploads, generation.Upload{Name: name, Data: data})
		}
		if reply, err = client.Edit(ctx, body, uploads); err != nil {
			slog.Debug("OpenAI image edit failed", "error", err)
			return call.Fail(fmt.Sprintf("OpenAI image editing failed: %v", err), "api_error")
		}
	} else if reply, err = client.Images(ctx, body); err != nil {
		slog.Debug("OpenAI image generation failed", "error", err)
		return call.Fail(fmt.Sprintf("OpenAI image generation failed: %v", err), "api_error")
	}
	route.Model = t.api
	job.Record(route, reply["usage"])
	data := generation.List(reply, "data")
	if len(data) == 0 {
		return call.Fail("OpenAI returned no image data", "empty_response")
	}
	first := data[0]
	path, failure := job.Picture(ctx, call, generation.Text(first, "b64_json"), generation.Text(first, "url"), "openai_"+t.id, "OpenAI", route.BaseURL)
	if failure != nil {
		return *failure
	}
	extra := map[string]any{"size": size, "quality": strs.Nullable(t.quality)}
	if revised := generation.Text(first, "revised_prompt"); revised != "" {
		extra["revised_prompt"] = revised
	}
	modality := "text"
	if len(sources) > 0 {
		modality = "image"
	}
	return call.Made(path, modality, extra)
}
