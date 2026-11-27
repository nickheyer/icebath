package tools

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/generation"
	generationconfig "github.com/nickheyer/icebath/internal/generation/config"
	"github.com/nickheyer/icebath/internal/provider"
	"github.com/nickheyer/icebath/internal/terminal"
	"github.com/nickheyer/icebath/internal/tools"
	"github.com/nickheyer/icebath/pkg/strs"
)

// The tools, by name
const (
	Image = "image_generate"
	Video = "video_generate"
)

// Longest a video call may take: the services wait up to fifteen minutes on a job, then download it
const videoTimeout = 20 * time.Minute

// Helper task the tokens image calls take are counted under
const task = "image_generation"

type registrant struct {
	services *generation.Generation
	terminal *terminal.Terminal
	journal  *tools.Journal
}

func Register(registry *tools.Registry, services *generation.Generation, t *terminal.Terminal) {
	registry.Register(&registrant{services: services, terminal: t, journal: &tools.Journal{Name: "image_tools"}})
}

func (r *registrant) Tools() []tools.Def {
	return []tools.Def{
		{
			Name: Image, Toolset: "image_gen", Title: "Image", Icon: tools.IconWrite, Subject: "prompt",
			Schema:  imageSchema(),
			Dynamic: r.imageSchema,
			Handler: r.image,
			Check:   func(env *tools.Env) bool { return r.ready(env.Config, generation.Image) },
			// Calls of one reply draw at the same time, as many as the setting lets through
			Parallel:  true,
			Unfocused: true,
			Workers: func(env *tools.Env) int {
				return min(max(env.Config.GetInt(generationconfig.ImageParallel), generationconfig.MinParallel), generationconfig.MaxParallel)
			},
		},
		{
			Name: Video, Toolset: "video_gen", Title: "Video", Icon: tools.IconWrite, Subject: "prompt",
			Schema:    videoSchema(),
			Dynamic:   r.videoSchema,
			Handler:   r.video,
			Check:     r.videoOffered,
			Unfocused: true,
			Timeout:   videoTimeout,
		},
	}
}

// Whether the service that makes the kind with the settings is set up
func (r *registrant) ready(cfg *config.Config, kind generation.Kind) bool {
	service, err := r.services.Active(cfg, kind)
	return err == nil && service.Set(cfg)
}

// Whether video_generate is offered: turned on, or a video service chosen, and that service set up
func (r *registrant) videoOffered(env *tools.Env) bool {
	cfg := env.Config
	return (cfg.GetBool(generationconfig.VideoEnabled) || generation.Chosen(cfg, generation.Video) != "") && r.ready(cfg, generation.Video)
}

func (r *registrant) imageSchema(env *tools.Env) map[string]any {
	service, err := r.services.Active(env.Config, generation.Image)
	if err != nil {
		return nil
	}
	return imageSchemaFor(service.Images.Takes(env.Config))
}

func (r *registrant) videoSchema(env *tools.Env) map[string]any {
	cfg := env.Config
	service, err := r.services.Active(cfg, generation.Video)
	if err != nil {
		return tools.Schema(videoDescription+"\n\nNo video backend is available. Calls will return an error until the user picks one with /videogen.", []string{"prompt"},
			map[string]any{"prompt": videoProperties()["prompt"]})
	}
	videos := service.Videos
	model := strs.Filled(cfg.GetString(service.ModelSetting(generation.Video)), generation.Named(cfg, generation.Video))
	if model == "" {
		model = videos.Default(context.Background(), cfg)
	}
	var meta *generation.Model
	for _, m := range videos.Models(context.Background(), cfg) {
		if m.ID == model {
			meta = &m
			break
		}
	}
	return videoSchemaFor(videos.Takes(cfg), meta)
}

// Job of a call: its settings, and the tokens it takes counted against the session under the image generation task
func job(call *tools.Call) *generation.Job {
	j := &generation.Job{Config: call.Env.Config}
	if aux := call.Env.Aux; aux != nil && aux.Used != nil {
		j.Used = func(route provider.Route, usage provider.Usage) { aux.Used(task, route, usage) }
	}
	return j
}

// Text of an argument, other values written out; empty when absent
func text(args map[string]any, name string) string {
	switch value := args[name].(type) {
	case nil:
		return ""
	case string:
		return value
	default:
		return fmt.Sprint(value)
	}
}

// Texts of an argument that is one text or a list of them, blanks left out
func texts(value any) []string {
	var out []string
	switch v := value.(type) {
	case string:
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	case []any:
		for _, item := range v {
			if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, strings.TrimSpace(s))
			}
		}
	}
	return out
}

// True or false of an argument that is one, or a word that says one; nil otherwise
func flag(value any) *bool {
	var on bool
	switch v := value.(type) {
	case bool:
		on = v
	case string:
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "true", "1", "yes", "on":
			on = true
		case "false", "0", "no", "off":
		default:
			return nil
		}
	default:
		return nil
	}
	return &on
}

// Envelope of a failure before any service was asked
func failure(kind generation.Kind, text, errorType, provider string) string {
	return tools.JSON(generation.Call{Provider: provider}.Fail(text, errorType).Envelope(kind))
}

// Draws or edits a picture with the service in use: its sources read where the agent saw them, its result kept in the cache, shown in the
// chat and handed to the model with the path the agent's commands see it at
func (r *registrant) image(ctx context.Context, call *tools.Call) string {
	cfg, args := call.Env.Config, call.Args
	prompt := text(args, "prompt")
	if strings.TrimSpace(prompt) == "" {
		return tools.ErrorResult(errors.New("prompt is required for image generation"))
	}
	aspect := generation.DefaultAspect
	if value, ok := args["aspect_ratio"].(string); ok {
		aspect = value
	}
	sources := append(texts(args["image_url"]), texts(args["reference_image_urls"])...)
	confined, err := r.confine(ctx, call, sources)
	var refused *refusal
	if errors.As(err, &refused) {
		return tools.JSON(map[string]any{"success": false, "image": nil, "error": "Could not read source image: " + refused.text, "error_type": refused.kind})
	}
	if err != nil {
		return tools.ErrorResult(fmt.Errorf("Tool execution failed: %v", err))
	}
	request := generation.ImageRequest{Prompt: prompt, Aspect: aspect, Model: generation.Named(cfg, generation.Image)}
	if len(texts(args["image_url"])) > 0 {
		request.Image, confined = confined[0], confined[1:]
	}
	request.References = confined
	if on, ok := args["upscale"].(bool); ok {
		request.Upscale = &on
	}
	service, err := r.services.Active(cfg, generation.Image)
	if err != nil {
		kind := "no_provider_configured"
		var unregistered *generation.Unregistered
		if errors.As(err, &unregistered) {
			kind = "provider_not_registered"
		}
		return failure(generation.Image, err.Error(), kind, generation.Chosen(cfg, generation.Image))
	}
	takes := service.Images.Takes(cfg)
	for _, name := range generation.Controls {
		if value, ok := args[name]; ok && slices.Contains(takes.Controls, name) {
			if request.Controls == nil {
				request.Controls = map[string]any{}
			}
			request.Controls[name] = value
		}
	}
	started := time.Now()
	result := service.Images.Generate(ctx, job(call), request)
	if result.Provider == "" {
		result.Provider = service.Name
	}
	envelope := result.Envelope(generation.Image)
	if result.OK {
		kept := cmp.Or(result.Copy, result.Media)
		r.shown(call, envelope, "image", kept)
		call.Display = preview(kept)
		slog.Info(fmt.Sprintf("Generated image via %s (%s) in %.1fs", result.Provider, result.Model, time.Since(started).Seconds()))
	} else {
		slog.Error("Error generating image: " + result.Error)
	}
	r.journal.Record(cfg, generationconfig.ImageDebug, Image, map[string]any{"provider": result.Provider, "model": result.Model, "success": result.OK,
		"error": strs.Nullable(result.Error), "generation_time": time.Since(started).Seconds(),
		"parameters": map[string]any{"prompt": prompt, "aspect_ratio": aspect, "modality": result.Modality, "source_images": len(sources), "upscale": request.Upscale,
			"controls": request.Controls}})
	return tools.JSON(envelope)
}

// Adds where the file is on this machine and where the agent's commands see it, when they run in a sandbox that sees it elsewhere
func (r *registrant) shown(call *tools.Call, envelope map[string]any, field, media string) {
	if call.Env.Visible == nil || !filepath.IsAbs(media) {
		return
	}
	if visible := call.Env.Visible(media); visible != "" && visible != media {
		envelope["host_"+field], envelope["agent_visible_"+field] = media, visible
	}
}

// Makes a video with the service in use from text, a still or reference pictures
func (r *registrant) video(ctx context.Context, call *tools.Call) string {
	cfg, args := call.Env.Config, call.Args
	for _, name := range []string{"prompt", "image_url", "aspect_ratio", "resolution", "negative_prompt"} {
		if value, ok := args[name]; ok && value != nil && value != "" {
			if _, isText := value.(string); !isText {
				return tools.ErrorResult(fmt.Errorf("Tool execution failed: %s must be text, got %T", name, value))
			}
		}
	}
	prompt := strings.TrimSpace(text(args, "prompt"))
	image := strings.TrimSpace(text(args, "image_url"))
	sources := append(texts(image), texts(args["reference_image_urls"])...)
	confined, err := r.confine(ctx, call, sources)
	var refused *refusal
	if errors.As(err, &refused) {
		return tools.JSON(map[string]any{"success": false, "video": nil, "error": "Could not read source image: " + refused.text, "error_type": refused.kind})
	}
	if err != nil {
		return tools.ErrorResult(fmt.Errorf("Tool execution failed: %v", err))
	}
	request := generation.VideoRequest{Prompt: prompt, Model: generation.Named(cfg, generation.Video), Duration: generation.Whole(args["duration"]),
		Aspect:     strings.TrimSpace(text(args, "aspect_ratio")),
		Resolution: strings.TrimSpace(text(args, "resolution")), Negative: strings.TrimSpace(text(args, "negative_prompt")), Audio: flag(args["audio"]),
		Seed: generation.Whole(args["seed"]), Upscale: flag(args["upscale"])}
	if _, ok := args["duration"].(bool); ok {
		request.Duration = nil
	}
	if image != "" {
		request.Image, confined = confined[0], confined[1:]
	}
	request.References = confined
	if request.Aspect == "" {
		request.Aspect = generation.DefaultVideoAspect
	}
	if request.Resolution == "" {
		request.Resolution = generation.DefaultVideoResolution
	}
	if prompt == "" {
		return tools.ErrorResult(errors.New("prompt is required for video generation"))
	}
	_, operation := args["operation"]
	_, address := args["video_url"]
	if operation || address {
		return tools.ErrorResult(errors.New("video_generate only supports text-to-video, image-to-video, and reference-to-video; use a provider-specific tool " +
			"for video edit/extend"))
	}
	service, err := r.services.Active(cfg, generation.Video)
	if err != nil {
		var unregistered *generation.Unregistered
		if errors.As(err, &unregistered) {
			return failure(generation.Video, err.Error(), "provider_not_registered", unregistered.Name)
		}
		return failure(generation.Video, "No video generation backend is configured. Pick one with /videogen (xAI, FAL, OpenRouter, or DeepInfra).",
			"no_provider_configured", "")
	}
	result := service.Videos.Generate(ctx, job(call), request)
	if result.Provider == "" {
		result.Provider = service.Name
	}
	envelope := result.Envelope(generation.Video)
	if result.OK {
		r.shown(call, envelope, "video", result.Media)
	} else {
		slog.Warn(fmt.Sprintf("video_gen provider '%s' failed: %s", service.Name, result.Error))
	}
	return tools.JSON(envelope)
}
