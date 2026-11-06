package media

import (
	"context"
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	metaDefaultBaseURL = "https://api.meta.ai/v1"
	metaAPIKeyEnv      = "META_MODEL_API_KEY"
	metaBaseURLEnv     = "META_BASE_URL"
	metaDefaultModel   = "muse-image-1.0"
)

// Auth env vars in the meta-ai chat provider priority order
var metaAPIKeyEnvs = []string{"MODEL_API_KEY", "META_API_KEY", "META_MODEL_API_KEY"}

// Model ids sent verbatim to /v1/images/generations
var metaImageModels = imageModelCatalog{order: []string{metaDefaultModel}, meta: map[string]map[string]any{
	metaDefaultModel: {"display": "Muse Image 1.0", "speed": "~10s", "strengths": "Meta Model API image generation", "price": "$0.01/image"},
}}

// First non empty auth env var through the secret scope
func resolveMetaAPIKey(ctx context.Context) string {
	for _, name := range metaAPIKeyEnvs {
		if v := mustSecret(ctx, name); v != "" {
			return v
		}
	}
	return ""
}

// Base URL override through the secret scope so a routed profile never inherits the launch endpoint
func resolveMetaBaseURL(ctx context.Context) string {
	if v := strings.TrimSpace(provider.GetSecretStr(ctx, metaBaseURLEnv, "")); v != "" {
		return v
	}
	return metaDefaultBaseURL
}

// Caller model, META_IMAGE_MODEL, the scoped and top level config, else muse-image-1.0
func resolveMetaImageModel(env *tools.Env, callerModel string) (string, map[string]any) {
	return resolveStaticModel(env, metaImageModels, metaDefaultModel, staticModelOptions{EnvVar: "META_IMAGE_MODEL", ConfigKey: "meta-ai", Explicit: callerModel})
}

// Meta Model API images.generate backend for muse-image
type metaImageGenProvider struct {
	staticImageGenProvider
	env func() *tools.Env
}

func newMetaImageGenProvider() *metaImageGenProvider {
	return &metaImageGenProvider{env: pluginEnv, staticImageGenProvider: staticImageGenProvider{providerID: "meta-ai", label: "Meta Model API",
		models: metaImageModels, defaultModelID: metaDefaultModel,
		setup: apiKeySetup{Name: "Meta Model API", Badge: "paid", Tag: "Muse Image via Meta Model API (api.meta.ai)",
			Key: metaAPIKeyEnv, Prompt: "Meta Model API key (LLM|... token)", URL: "https://api.meta.ai"}}}
}

// Catalog rows read at call time so catalog additions show up
func (p *metaImageGenProvider) ListModels() []map[string]any {
	return catalogRows(metaImageModels, p.catalogFields, p.price)
}

// Available with any auth alias set and the client linked
func (p *metaImageGenProvider) IsAvailable() bool {
	return resolveMetaAPIKey(context.Background()) != "" && openAIImportable()
}

// Text to image only until image to image is verified against Meta
func (p *metaImageGenProvider) Capabilities() map[string]any {
	return map[string]any{"modalities": []any{"text"}, "max_reference_images": 0}
}

// Generates through Meta's OpenAI compatible endpoint and caches the WebP result
func (p *metaImageGenProvider) Generate(ctx context.Context, env *tools.Env, req ImageGenRequest) (map[string]any, error) {
	if env == nil {
		env = p.env()
	}
	prompt := strings.TrimSpace(req.Prompt)
	aspect := resolveAspectRatio(req.AspectRatio)
	if prompt == "" {
		return promptRequiredError("meta-ai", aspect), nil
	}
	apiKey := resolveMetaAPIKey(ctx)
	if apiKey == "" {
		return errorFactory("meta-ai", aspect, "", "")(metaAPIKeyEnv+" not set. Run `icebath tools` -> Image "+
			"Generation -> Meta Model API to configure.", "auth_required"), nil
	}
	newClient, failure := importOpenAI("meta-ai", aspect)
	if failure != nil {
		return failure, nil
	}
	callerModel, _ := req.Kwargs["model"].(string)
	modelID, _ := resolveMetaImageModel(env, callerModel)
	size := sizeFor(aspect)
	fail := errorFactory("meta-ai", aspect, modelID, prompt)
	baseURL := resolveMetaBaseURL(ctx)
	client, err := newClient(env, openAIClientOptions{APIKey: &apiKey, BaseURL: &baseURL})
	if err != nil {
		return nil, err
	}
	response, err := client.ImagesGenerate(ctx, map[string]any{"model": modelID, "prompt": prompt, "size": size, "n": 1})
	if err != nil {
		logOf(env).Debug("Meta image generation failed", "err", err)
		return fail(fmt.Sprintf("Meta image generation failed: %v", err), "api_error"), nil
	}
	if !response.Object || len(response.Data) == 0 {
		return fail("Meta response contained no image data", "empty_response"), nil
	}
	first := response.Data[0]
	b64, rawURL := openAIItemString(first, "b64_json"), openAIItemString(first, "url")
	var imageRef string
	switch {
	case b64 != "":
		imageRef, err = saveB64Image(env, b64, "meta", "webp")
	case rawURL != "":
		imageRef, err = saveURLImage(ctx, env, rawURL, "meta", 0, 0)
	default:
		return fail("Meta response contained neither b64_json nor URL", "empty_response"), nil
	}
	if err != nil {
		return fail(fmt.Sprintf("Failed to save Meta image: %v", err), "io_error"), nil
	}
	extra := map[string]any{"size": size}
	if revised := openAIItemField(first, "revised_prompt"); pyTruthy(revised) {
		extra["revised_prompt"] = revised
	}
	return imageSuccessResponse(imageRef, modelID, prompt, aspect, "meta-ai", "text", extra), nil
}

func init() {
	plugins.RegisterBundled(imageGenBackendPlugin{name: "meta-ai-image-gen", key: "meta-ai", author: "Meta Platforms, Inc.", requiresEnv: []string{metaAPIKeyEnv},
		description: "Meta Model API image generation backend (muse-image). OpenAI-compatible /v1/images/generations. Saves images to $ICEBATH_HOME/cache/generated/images/.",
		provider:    func() ImageGenProvider { return newMetaImageGenProvider() }})
}
