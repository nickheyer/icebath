package generate

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/generation"
	deepinfraconfig "github.com/nickheyer/icebath/internal/plugins/defaults/generators/deepinfra/config"
	"github.com/nickheyer/icebath/pkg/strs"
)

const (
	videoPollEvery = 5 * time.Second
	videoPollFor   = 900 * time.Second
)

var videoEnded = strs.Set("completed", "succeeded", "failed", "error", "cancelled", "canceled")

// Picker row of a catalog model, its price per picture when the catalog gives one
func row(item map[string]any, clip int) generation.Model {
	id := generation.Text(item, "id")
	metadata := generation.Object(item["metadata"])
	description := generation.Text(metadata, "description")
	if clip > 0 {
		description = strs.Head(description, clip)
	}
	out := generation.Model{ID: id, Title: id[strings.LastIndex(id, "/")+1:], Strengths: description}
	if unit, ok := generation.Object(metadata["pricing"])["per_image_unit"].(float64); ok {
		out.Price = fmt.Sprintf("$%.4f an image", unit)
	}
	return out
}

func (s *services) imageModels(ctx context.Context, cfg *config.Config) []generation.Model {
	var rows []generation.Model
	for _, item := range s.tagged(ctx, cfg, "image-gen") {
		rows = append(rows, row(item, 0))
	}
	return rows
}

func (s *services) videoModels(ctx context.Context, cfg *config.Config) []generation.Model {
	var rows []generation.Model
	for _, item := range s.tagged(ctx, cfg, "video-gen") {
		rows = append(rows, row(item, 80))
	}
	return rows
}

// Picture model: the one image_gen.deepinfra.model names, else the first of the catalog; empty when there is none
func (s *services) imageModel(ctx context.Context, cfg *config.Config, model string) string {
	if model = strings.TrimSpace(model); model != "" {
		return model
	}
	if models := s.imageModels(ctx, cfg); len(models) > 0 {
		return models[0].ID
	}
	return ""
}

// First video model of the catalog, empty when it can't be read
func (s *services) videoDefault(ctx context.Context, cfg *config.Config) string {
	if models := s.videoModels(ctx, cfg); len(models) > 0 {
		return models[0].ID
	}
	return ""
}

// Draws from text through the OpenAI compatible images API with the model of the settings or the catalog
func (s *services) draw(ctx context.Context, job *generation.Job, request generation.ImageRequest) generation.Result {
	cfg := job.Config
	prompt, aspect := strings.TrimSpace(request.Prompt), generation.Aspect(request.Aspect)
	call := generation.Call{Provider: deepinfraconfig.Name, Prompt: prompt, Aspect: aspect}
	if len(request.Sources(-1)) > 0 {
		return call.Fail("DeepInfra image generation is text-to-image only in this backend; image_url and reference_image_urls are unsupported.", "modality_unsupported")
	}
	if prompt == "" {
		call.Prompt = ""
		return call.Fail("Prompt is required and must be a non-empty string", "invalid_argument")
	}
	route, ok := s.route(cfg)
	if !ok {
		return generation.Call{Provider: deepinfraconfig.Name, Aspect: aspect}.Fail("DEEPINFRA_API_KEY not set. The user connects DeepInfra in /providers, or picks "+
			"another image service with /images.", "auth_required")
	}
	model := s.imageModel(ctx, cfg, request.Model)
	if model == "" {
		return call.Fail("No DeepInfra image-gen model available. Pin one in image_gen.deepinfra.model, set DEEPINFRA_IMAGE_MODEL, or check connectivity to "+
			"api.deepinfra.com so the live catalog can be fetched.", "no_model_available")
	}
	call.Model = model
	size := generation.Size(aspect)
	base := strings.TrimRight(strs.Filled(cfg.GetString(deepinfraconfig.BaseURL), route.BaseURL), "/")
	reply, err := generation.OpenAI{BaseURL: base, Key: route.APIKey}.Images(ctx, map[string]any{"model": model, "prompt": prompt, "size": size, "n": 1})
	if err != nil {
		slog.Debug("DeepInfra image generation failed", "error", err)
		return call.Fail(fmt.Sprintf("DeepInfra image generation failed: %v", err), "api_error")
	}
	data := generation.List(reply, "data")
	if len(data) == 0 {
		return call.Fail("DeepInfra returned no image data", "empty_response")
	}
	short := model[strings.Index(model, "/")+1:]
	path, failure := job.Picture(ctx, call, generation.Text(data[0], "b64_json"), generation.Text(data[0], "url"), "deepinfra_"+strings.ReplaceAll(short, ":", "_"),
		"DeepInfra", base)
	if failure != nil {
		return *failure
	}
	return call.Made(path, "text", map[string]any{"size": size})
}

// Makes a video through the OpenAI compatible videos API: creates the job, polls it to its end and keeps its output in the cache, the
// delivery address when that can't be done
func (s *services) makeVideo(ctx context.Context, job *generation.Job, request generation.VideoRequest) generation.Result {
	cfg := job.Config
	call := generation.Call{Provider: deepinfraconfig.Name}
	if strings.TrimSpace(request.Prompt) == "" {
		return call.Fail("prompt is required", "invalid_request")
	}
	route, ok := s.route(cfg)
	if !ok {
		return call.Fail("DEEPINFRA_API_KEY is not set", "missing_credentials")
	}
	model := strs.Filled(request.Model, s.videoDefault(ctx, cfg))
	if model == "" {
		return call.Fail("no deepinfra video model available (live catalog empty?)", "no_model")
	}
	call = generation.Call{Provider: deepinfraconfig.Name, Model: model, Prompt: request.Prompt, Aspect: request.Aspect}
	body := map[string]any{"model": model, "prompt": request.Prompt, "aspect_ratio": request.Aspect}
	if request.Duration != nil && *request.Duration != 0 {
		body["seconds"] = fmt.Sprint(*request.Duration)
	}
	if request.Resolution != "" {
		body["size"] = request.Resolution
	}
	if request.Negative != "" {
		body["negative_prompt"] = request.Negative
	}
	if request.Image != "" {
		body["image_url"] = request.Image
	}
	if request.Seed != nil {
		body["seed"] = *request.Seed
	}
	client := generation.OpenAI{BaseURL: route.BaseURL, Key: route.APIKey}
	video, err := poll(ctx, client, body)
	if err != nil {
		slog.Debug("deepinfra video generation failed", "error", err)
		return call.Fail(fmt.Sprintf("deepinfra video generation failed: %v", err), "api_error")
	}
	if status := generation.Text(video, "status"); status != "completed" && status != "succeeded" {
		if failed := video["error"]; failed != nil && failed != "" {
			return call.Fail(describe(failed), "job_failed")
		}
		return call.Fail(fmt.Sprintf("video job ended with status='%s'", status), "job_failed")
	}
	delivery := ""
	for _, item := range generation.List(video, "data") {
		if u := generation.Text(item, "url"); u != "" {
			delivery = u
			break
		}
	}
	var path string
	if delivery != "" {
		path, err = job.Download(ctx, generation.Video, delivery, generation.Fetch{Prefix: deepinfraconfig.Name, Origin: route.BaseURL})
	} else {
		var raw []byte
		if raw, err = client.Content(ctx, generation.Text(video, "id")); err == nil {
			path, err = job.Save(generation.Video, raw, deepinfraconfig.Name, "mp4")
		}
	}
	if err != nil {
		if delivery == "" {
			return call.Fail(fmt.Sprintf("deepinfra video job succeeded but no output could be retrieved: %v", err), "empty_response")
		}
		slog.Debug(fmt.Sprintf("deepinfra: saving video locally failed (%v); returning URL", err))
		path = delivery
	}
	modality := "text"
	if request.Image != "" {
		modality = "image"
	}
	made := call.Made(path, modality, nil)
	if request.Duration != nil {
		made.Duration = *request.Duration
	}
	return made
}

// Creates the job and reads it again every five seconds until it ends, an error once fifteen minutes pass first
func poll(ctx context.Context, client generation.OpenAI, body map[string]any) (map[string]any, error) {
	video, err := client.CreateVideo(ctx, body)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(videoPollFor)
	for !videoEnded[generation.Text(video, "status")] {
		if !time.Now().Before(deadline) {
			return nil, fmt.Errorf("video job %s did not reach a terminal status within %ds (last status=%s)", strs.Filled(generation.Text(video, "id"), "?"),
				int(videoPollFor.Seconds()), generation.Text(video, "status"))
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(videoPollEvery):
		}
		if video, err = client.Video(ctx, generation.Text(video, "id")); err != nil {
			return nil, err
		}
	}
	return video, nil
}

// Error of a job as text: code and message first for an object
func describe(failed any) string {
	object, ok := failed.(map[string]any)
	if !ok {
		return fmt.Sprint(failed)
	}
	var parts []string
	for _, key := range []string{"code", "message"} {
		if value, ok := object[key]; ok {
			parts = append(parts, fmt.Sprintf("%s=%v", key, value))
		}
	}
	for key, value := range object {
		if key != "code" && key != "message" {
			parts = append(parts, fmt.Sprintf("%s=%v", key, value))
		}
	}
	return strings.Join(parts, " ")
}
