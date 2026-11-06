package media

import (
	"context"
	"fmt"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/tools"
)

// FAL backend delegating to the image_generate pipeline at call time
type falImageGenProvider struct {
	staticImageGenProvider
	env func() *tools.Env
}

func newFalImageGenProvider() *falImageGenProvider {
	return &falImageGenProvider{env: pluginEnv, staticImageGenProvider: staticImageGenProvider{providerID: "fal", label: "FAL.ai",
		setup: apiKeySetup{Name: "FAL.ai", Badge: "paid",
			Tag: "Pick from flux-2-klein, flux-2-pro, gpt-image, nano-banana-2, nano-banana-pro, etc. — text-to-image & image editing",
			Key: "FAL_KEY", Prompt: "FAL API key", URL: "https://fal.ai/dashboard/keys"}}}
}

// Whether a FAL key or the managed gateway serves the configured selection, false when the check panics
func (p *falImageGenProvider) IsAvailable() (ok bool) {
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	return checkFalAPIKeyHook(p.env())
}

func (p *falImageGenProvider) ListModels() []map[string]any {
	return catalogRows(imageModelCatalog{order: falModelOrder, meta: falModels}, nil, nil)
}

func (p *falImageGenProvider) DefaultModel() string { return falDefaultModel }

// Edit modality and reference cap of the configured FAL model, upscale always offered
func (p *falImageGenProvider) Capabilities() map[string]any {
	_, meta := resolveFalModel(p.env())
	if edit, _ := meta["edit_endpoint"].(string); edit != "" {
		refs := positiveInt(meta["max_reference_images"])
		if refs == 0 {
			refs = 1
		}
		return map[string]any{"modalities": []any{"text", "image"}, "max_reference_images": refs, "supports_upscale": true}
	}
	return map[string]any{"modalities": []any{"text"}, "max_reference_images": 0, "supports_upscale": true}
}

// Image tool arguments carrying the non nil passthrough kwargs of the provider call verbatim
func falPassthroughArgs(prompt, aspect string, req ImageGenRequest) imageToolArgs {
	k := req.Kwargs
	a := imageToolArgs{Prompt: prompt, AspectRatio: aspect, NumInferenceSteps: k["num_inference_steps"],
		GuidanceScale: k["guidance_scale"], NumImages: k["num_images"], OutputFormat: k["output_format"],
		Seed: k["seed"], Upscale: k["upscale"]}
	if req.ImageURL != "" {
		a.ImageURL = req.ImageURL
	}
	if req.ReferenceImageURLs != nil {
		a.ReferenceImageURLs = req.ReferenceImageURLs
	}
	return a
}

// Runs image_generate_tool and reshapes its JSON reply into the provider envelope with provider, prompt, aspect and model
func (p *falImageGenProvider) Generate(ctx context.Context, env *tools.Env, req ImageGenRequest) (response map[string]any, _ error) {
	aspect := resolveAspectRatio(req.AspectRatio)
	raw, failure := func() (raw string, failure map[string]any) {
		defer func() {
			if rec := recover(); rec != nil {
				typeName := "Exception"
				if err, ok := rec.(error); ok {
					typeName = pyErrorTypeName(err)
				}
				logOf(env).Warn(fmt.Sprintf("FAL image_generate_tool raised: %v", rec))
				failure = map[string]any{"success": false, "image": nil, "error": fmt.Sprintf("FAL image generation failed: %v", rec),
					"error_type": typeName, "provider": "fal", "prompt": req.Prompt, "aspect_ratio": aspect}
			}
		}()
		return imageGenerateToolHook(ctx, env, falPassthroughArgs(req.Prompt, aspect, req)), nil
	}()
	if failure != nil {
		return failure, nil
	}
	decoded, err := pyjson.Loads(raw)
	if err != nil {
		response = map[string]any{"success": false, "image": nil, "error": "Invalid JSON from FAL pipeline"}
	} else if m, ok := pyjson.Plain(decoded).(map[string]any); ok {
		response = m
	} else {
		response = map[string]any{"success": false, "image": nil, "error": "FAL pipeline returned a non-dict response", "error_type": "provider_contract"}
	}
	for k, v := range map[string]any{"provider": "fal", "prompt": req.Prompt, "aspect_ratio": aspect} {
		if _, has := response[k]; !has {
			response[k] = v
		}
	}
	if _, has := response["model"]; !has {
		response["model"], _ = resolveFalModel(env)
	}
	return response, nil
}

// Compiled in image_gen backend plugin registering one provider
type imageGenBackendPlugin struct {
	name, key, author, description string
	requiresEnv                    []string
	provider                       func() ImageGenProvider
}

func (b imageGenBackendPlugin) Manifest() plugins.Manifest {
	env := make([]plugins.EnvSpec, 0, len(b.requiresEnv))
	for _, n := range b.requiresEnv {
		env = append(env, plugins.EnvSpec{Name: n})
	}
	key, author := b.key, b.author
	if key == "" {
		key = b.name
	}
	if author == "" {
		author = "NousResearch"
	}
	return plugins.Manifest{Name: b.name, Version: "1.0.0", Description: b.description, Author: author, Kind: "backend",
		Source: "bundled", Key: "image_gen/" + key, RequiresEnv: env}
}

func (b imageGenBackendPlugin) Register(ctx *plugins.Context) error {
	_, err := ctx.RegisterImageGenProvider(b.provider())
	return err
}

func init() {
	plugins.RegisterBundled(imageGenBackendPlugin{name: "fal", requiresEnv: []string{"FAL_KEY"},
		description: "FAL.ai image generation backend (flux-2-klein, flux-2-pro, nano-banana-2, nano-banana-pro, gpt-image-1.5, recraft-v3, etc.).",
		provider:    func() ImageGenProvider { return newFalImageGenProvider() }})
}
