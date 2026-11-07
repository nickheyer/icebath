package media

import (
	"context"
	"fmt"
	"math"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Live image-gen tagged models of the DeepInfra catalog, nil when the fetch fails
var deepInfraImageLiveModels = func(ctx context.Context, env *tools.Env) []map[string]any {
	return deepInfraModelsByTag(ctx, env, "image-gen")
}

// Python format of a float with four decimals, inf and nan spelled the Python way
func pyFixed4(f float64) string {
	switch {
	case math.IsNaN(f):
		return "nan"
	case math.IsInf(f, 1):
		return "inf"
	case math.IsInf(f, -1):
		return "-inf"
	}
	return fmt.Sprintf("%.4f", f)
}

// Picker row of a catalog item with its price per image and default dimensions when present
func formatDeepInfraCatalogRow(item map[string]any) map[string]any {
	mid := ""
	if v, ok := item["id"]; ok {
		mid = pyStrOf(v)
	}
	metadata, _ := item["metadata"].(map[string]any)
	if metadata == nil {
		metadata = map[string]any{}
	}
	display := mid
	if _, after, ok := strings.Cut(mid, "/"); ok {
		display = after
	}
	strengths, ok := metadata["description"]
	if !ok {
		strengths = ""
	}
	row := map[string]any{"id": mid, "display": display, "strengths": strengths}
	if pricing, ok := metadata["pricing"].(map[string]any); ok && pricing["per_image_unit"] != nil {
		if f, err := pyint.Float(pricing["per_image_unit"]); err == nil {
			row["price"] = "$" + pyFixed4(f) + "/image"
		}
	}
	for _, key := range []string{"default_width", "default_height", "default_iterations"} {
		if v := metadata[key]; v != nil {
			row[key] = v
		}
	}
	return row
}

// Env override, then the scoped config model, then the first live catalog id, else empty
func resolveDeepInfraImageModel(env *tools.Env, catalog []map[string]any, cfg map[string]any) string {
	if v := strings.TrimSpace(rawEnvValue(env, "DEEPINFRA_IMAGE_MODEL")); v != "" {
		return v
	}
	if s, ok := cfg["model"].(string); ok && strings.TrimSpace(s) != "" {
		return strings.TrimSpace(s)
	}
	if len(catalog) > 0 {
		if first, ok := catalog[0]["id"].(string); ok && first != "" {
			return first
		}
	}
	return ""
}

// DeepInfra images.generations backend over the live image-gen catalog
type deepInfraImageGenProvider struct {
	staticImageGenProvider
	env func() *tools.Env
}

func newDeepInfraImageGenProvider() *deepInfraImageGenProvider {
	return &deepInfraImageGenProvider{env: pluginEnv, staticImageGenProvider: staticImageGenProvider{providerID: "deepinfra", label: "DeepInfra",
		setup: apiKeySetup{Name: "DeepInfra", Badge: "paid", Tag: "FLUX, Qwen-Image, … — live catalog from api.deepinfra.com",
			Key: "DEEPINFRA_API_KEY", Prompt: "DeepInfra API key", URL: "https://deepinfra.com/dash/api_keys"}}}
}

// Available when the DeepInfra key resolves through the process secret scope
func (p *deepInfraImageGenProvider) IsAvailable() bool {
	return deepInfraKeyAvailable(context.Background())
}

// True when the DeepInfra key resolves non blank through the secret scope the context carries
func deepInfraKeyAvailable(ctx context.Context) bool {
	return strings.TrimSpace(mustSecret(ctx, "DEEPINFRA_API_KEY")) != ""
}

// Picker rows of the live catalog
func (p *deepInfraImageGenProvider) ListModels() []map[string]any {
	rows := []map[string]any{}
	for _, item := range deepInfraImageLiveModels(context.Background(), p.env()) {
		rows = append(rows, formatDeepInfraCatalogRow(item))
	}
	return rows
}

// First live catalog id, empty when the catalog is empty
func (p *deepInfraImageGenProvider) DefaultModel() string {
	if rows := p.ListModels(); len(rows) > 0 {
		return pyStrOf(rows[0]["id"])
	}
	return ""
}

// Text only, the OpenAI compatible generation surface takes no source images
func (p *deepInfraImageGenProvider) Capabilities() map[string]any {
	return map[string]any{"modalities": []any{"text"}, "max_reference_images": 0}
}

// Generates through the OpenAI compatible endpoint with the resolved live model and caches the result
func (p *deepInfraImageGenProvider) Generate(ctx context.Context, env *tools.Env, req ImageGenRequest) (map[string]any, error) {
	if env == nil {
		env = p.env()
	}
	prompt := strings.TrimSpace(req.Prompt)
	aspect := resolveAspectRatio(req.AspectRatio)
	fail := errorFactory("deepinfra", aspect, "", "")
	if req.ImageURL != "" || len(req.ReferenceImageURLs) > 0 || pyTruthy(req.Kwargs["image_url"]) || pyTruthy(req.Kwargs["reference_image_urls"]) {
		return fail("DeepInfra image generation is text-to-image only in this "+
			"backend; image_url and reference_image_urls are unsupported.", "modality_unsupported", errorOverride{Prompt: &prompt}), nil
	}
	if prompt == "" {
		return promptRequiredError("deepinfra", aspect), nil
	}
	apiKey := strings.TrimSpace(mustSecret(ctx, "DEEPINFRA_API_KEY"))
	if apiKey == "" {
		return fail("DEEPINFRA_API_KEY not set. Run `icebath tools` → Image "+
			"Generation → DeepInfra to configure, or `icebath setup` to add the key.", "auth_required"), nil
	}
	diCfg := loadImageGenConfig(env, "deepinfra")
	modelID := resolveDeepInfraImageModel(env, deepInfraImageLiveModels(ctx, env), diCfg)
	if modelID == "" {
		return fail("No DeepInfra image-gen model available. Pin one in "+
			"config.yaml under image_gen.deepinfra.model, set "+
			"DEEPINFRA_IMAGE_MODEL, or check connectivity to "+
			"api.deepinfra.com so the live catalog can be fetched.", "no_model_available", errorOverride{Prompt: &prompt}), nil
	}
	size := sizeFor(aspect)
	newClient, failure := importOpenAI("deepinfra", aspect)
	if failure != nil {
		return failure, nil
	}
	fail = errorFactory("deepinfra", aspect, modelID, prompt)
	baseURL := deepInfraBaseURL(env, config.Tree(diCfg))
	client, err := newClient(env, openAIClientOptions{APIKey: &apiKey, BaseURL: &baseURL})
	if err != nil {
		return nil, err
	}
	response, err := client.ImagesGenerate(ctx, map[string]any{"model": modelID, "prompt": prompt, "size": size, "n": 1})
	client.Close()
	if err != nil {
		logOf(env).Debug("DeepInfra image generation failed", "err", err)
		return fail(fmt.Sprintf("DeepInfra image generation failed: %v", err), "api_error"), nil
	}
	if len(response.Data) == 0 {
		return fail("DeepInfra returned no image data", "empty_response"), nil
	}
	first := response.Data[0]
	short := modelID
	if _, after, ok := strings.Cut(modelID, "/"); ok {
		short = after
	}
	imageRef, failure := materializeImage(ctx, env, openAIItemString(first, "b64_json"), openAIItemString(first, "url"),
		materializeOptions{Prefix: "deepinfra_" + strings.ReplaceAll(short, ":", "_"), Label: "DeepInfra", Provider: "deepinfra",
			Model: modelID, Prompt: prompt, Aspect: aspect, OnURLFail: func(err error) {
				logOf(env).Debug(fmt.Sprintf("DeepInfra: caching delivery URL failed (%v); returning URL", err))
			}})
	if failure != nil {
		return failure, nil
	}
	return imageSuccessResponse(imageRef, modelID, prompt, aspect, "deepinfra", "text", map[string]any{"size": size}), nil
}

func init() {
	plugins.RegisterBundled(imageGenBackendPlugin{name: "deepinfra", author: "Georgi Atsev", requiresEnv: []string{"DEEPINFRA_API_KEY"},
		description: "DeepInfra image generation backend (FLUX, Qwen-Image, …) via OpenAI-compatible /v1/images/generations. Catalog discovered live from api.deepinfra.com.",
		provider:    func() ImageGenProvider { return newDeepInfraImageGenProvider() }})
}
