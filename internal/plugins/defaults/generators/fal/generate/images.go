package generate

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/generation"
	falconfig "github.com/nickheyer/icebath/internal/plugins/defaults/generators/fal/config"
	"github.com/nickheyer/icebath/pkg/strs"
)

// One picture model of FAL: its endpoints, how it takes a size, what it is sent by default and the keys it takes
type picture struct {
	id, title, speed, strengths, price string
	// image_size by preset name, image_size by literal size, or aspect_ratio
	style    string
	sizes    map[string]any
	defaults map[string]any
	supports []string
	// Edit endpoint, the keys it takes, the most pictures it takes and the key they go under, image_urls when empty
	edit         string
	editSupports []string
	references   int
	editParam    string
}

var (
	presetSizes = map[string]any{"landscape": "landscape_16_9", "square": "square_hd", "portrait": "portrait_16_9"}
	aspectSizes = map[string]any{"landscape": "16:9", "square": "1:1", "portrait": "9:16"}
	fourThirds  = map[string]any{"landscape": "landscape_4_3", "square": "square_hd", "portrait": "portrait_4_3"}
)

// Key the size goes under for each style
var sizeKeys = map[string]string{"image_size_preset": "image_size", "gpt_literal": "image_size", "aspect_ratio": "aspect_ratio"}

const defaultPicture = "fal-ai/flux-2/klein/9b"

// FAL's picture models in the order the picker lists them
var pictures = func() []picture {
	gpt25 := func(variant, speed, strengths string) picture {
		return picture{id: "openai/gpt-image-2.5/" + variant + "/text-to-image", title: "GPT Image 2.5 " + strings.ToUpper(variant[:1]) + variant[1:], speed: speed,
			strengths: strengths, price: "Priced by tokens", sizes: fourThirds, defaults: map[string]any{"quality": "medium", "num_images": 1, "output_format": "png"},
			supports: []string{"prompt", "image_size", "quality", "num_images", "output_format", "background", "output_compression", "sync_mode"},
			edit:     "openai/gpt-image-2.5/" + variant + "/edit", references: 16,
			editSupports: []string{"prompt", "image_urls", "image_size", "quality", "num_images", "output_format", "background", "output_compression", "sync_mode",
				"mask_url", "input_fidelity"}}
	}
	list := []picture{
		{id: "fal-ai/flux-2/klein/9b", title: "FLUX 2 Klein 9B", speed: "Under 1 second", strengths: "Fast, with crisp text", price: "$0.006 a megapixel",
			defaults: map[string]any{"num_inference_steps": 4, "output_format": "png", "enable_safety_checker": false},
			supports: []string{"prompt", "image_size", "num_inference_steps", "seed", "output_format", "enable_safety_checker"},
			edit:     "fal-ai/flux-2/klein/9b/edit", references: 9,
			editSupports: []string{"prompt", "image_urls", "num_inference_steps", "seed", "output_format", "enable_safety_checker"}},
		{id: "fal-ai/flux-2-pro", title: "FLUX 2 Pro", speed: "About 6 seconds", strengths: "Studio photorealism", price: "$0.03 a megapixel",
			defaults: map[string]any{"num_inference_steps": 50, "guidance_scale": 4.5, "num_images": 1, "output_format": "png", "enable_safety_checker": false,
				"safety_tolerance": "5", "sync_mode": true},
			supports: []string{"prompt", "image_size", "num_inference_steps", "guidance_scale", "num_images", "output_format", "enable_safety_checker",
				"safety_tolerance", "sync_mode", "seed"},
			edit: "fal-ai/flux-2-pro/edit", references: 9,
			editSupports: []string{"prompt", "image_urls", "num_inference_steps", "guidance_scale", "num_images", "output_format", "enable_safety_checker",
				"safety_tolerance", "sync_mode", "seed"}},
		{id: "fal-ai/z-image/turbo", title: "Z-Image Turbo", speed: "About 2 seconds", strengths: "Text in English and Chinese, a 6B model", price: "$0.005 a megapixel",
			defaults: map[string]any{"num_inference_steps": 8, "num_images": 1, "output_format": "png", "enable_safety_checker": false, "enable_prompt_expansion": false},
			supports: []string{"prompt", "image_size", "num_inference_steps", "num_images", "seed", "output_format", "enable_safety_checker", "enable_prompt_expansion"}},
		{id: "fal-ai/nano-banana-pro", title: "Nano Banana Pro", speed: "About 8 seconds", strengths: "Gemini 3 Pro Image, deep reasoning and good text",
			price: "$0.15 an image at 1K", style: "aspect_ratio", defaults: map[string]any{"num_images": 1, "output_format": "png", "safety_tolerance": "5", "resolution": "1K"},
			supports: []string{"prompt", "aspect_ratio", "num_images", "output_format", "safety_tolerance", "seed", "sync_mode", "resolution", "enable_web_search",
				"limit_generations"},
			edit: "fal-ai/nano-banana-pro/edit", references: 2,
			editSupports: []string{"prompt", "image_urls", "aspect_ratio", "num_images", "output_format", "safety_tolerance", "seed", "sync_mode", "resolution",
				"enable_web_search", "limit_generations"}},
		{id: "fal-ai/nano-banana-2", title: "Nano Banana 2", speed: "About 3 seconds", strengths: "Gemini 3.1 Flash Image, fast reasoning, text in many languages, infographics",
			price: "The cheaper Flash plan", style: "aspect_ratio",
			defaults: map[string]any{"num_images": 1, "output_format": "png", "safety_tolerance": "4", "resolution": "1K", "limit_generations": true},
			supports: []string{"prompt", "aspect_ratio", "num_images", "output_format", "safety_tolerance", "seed", "sync_mode", "system_prompt", "resolution",
				"enable_web_search", "limit_generations", "thinking_level"},
			edit: "fal-ai/nano-banana-2/edit", references: 14,
			editSupports: []string{"prompt", "image_urls", "aspect_ratio", "num_images", "output_format", "safety_tolerance", "seed", "sync_mode", "system_prompt",
				"resolution", "enable_web_search", "limit_generations", "thinking_level"}},
		{id: "fal-ai/gpt-image-1.5", title: "GPT Image 1.5", speed: "About 15 seconds", strengths: "Follows prompts closely", price: "$0.034 an image", style: "gpt_literal",
			sizes:    map[string]any{"landscape": "1536x1024", "square": "1024x1024", "portrait": "1024x1536"},
			defaults: map[string]any{"quality": "medium", "num_images": 1, "output_format": "png"},
			supports: []string{"prompt", "image_size", "quality", "num_images", "output_format", "background", "sync_mode"},
			edit:     "fal-ai/gpt-image-1.5/edit", references: 16,
			editSupports: []string{"prompt", "image_urls", "image_size", "quality", "num_images", "output_format", "sync_mode"}},
		{id: "fal-ai/gpt-image-2", title: "GPT Image 2", speed: "About 20 seconds", strengths: "Leading text, including Chinese, Japanese and Korean, and true-to-life photorealism", price: "$0.04 to $0.06 an image",
			sizes: fourThirds, defaults: map[string]any{"quality": "medium", "num_images": 1, "output_format": "png"},
			supports: []string{"prompt", "image_size", "quality", "num_images", "output_format", "sync_mode"},
			edit:     "openai/gpt-image-2/edit", references: 16,
			editSupports: []string{"prompt", "image_urls", "quality", "num_images", "output_format", "sync_mode", "mask_image_url"}},
		gpt25("flare", "Fast", "Everyday creation, natural lighting and textures"),
		gpt25("sunburst", "Slower", "Precision editing, subject and composition consistency"),
		{id: "fal-ai/ideogram/v3", title: "Ideogram V3", speed: "About 5 seconds", strengths: "The best lettering", price: "$0.03 to $0.09 an image",
			defaults: map[string]any{"rendering_speed": "BALANCED", "expand_prompt": true, "style": "AUTO"},
			supports: []string{"prompt", "image_size", "rendering_speed", "expand_prompt", "style", "seed"},
			edit:     "fal-ai/ideogram/v3/edit", references: 1,
			editSupports: []string{"prompt", "image_urls", "rendering_speed", "expand_prompt", "style", "seed"}},
		{id: "fal-ai/recraft/v4/pro/text-to-image", title: "Recraft V4 Pro", speed: "About 8 seconds", strengths: "Design and brand work, ready to use", price: "$0.25 an image",
			defaults: map[string]any{"enable_safety_checker": false}, supports: []string{"prompt", "image_size", "enable_safety_checker", "colors", "background_color"}},
		{id: "fal-ai/qwen-image", title: "Qwen Image", speed: "About 12 seconds", strengths: "Built on a language model, complex text", price: "$0.02 a megapixel",
			defaults: map[string]any{"num_inference_steps": 30, "guidance_scale": 2.5, "num_images": 1, "output_format": "png", "acceleration": "regular"},
			supports: []string{"prompt", "image_size", "num_inference_steps", "guidance_scale", "num_images", "output_format", "acceleration", "seed", "sync_mode"},
			edit:     "fal-ai/qwen-image-2/pro/edit", references: 3,
			editSupports: []string{"prompt", "image_urls", "num_inference_steps", "guidance_scale", "num_images", "output_format", "acceleration", "seed", "sync_mode"}},
		{id: "fal-ai/krea/v2/medium/text-to-image", title: "Krea 2 Medium", speed: "15 to 25 seconds", strengths: "Illustration, anime, painting and expressive styles",
			price: "$0.030 from text, $0.035 with style references", style: "aspect_ratio", defaults: map[string]any{"creativity": "medium"},
			supports: []string{"prompt", "aspect_ratio", "creativity", "seed", "image_style_references"}},
		{id: "fal-ai/krea/v2/large/text-to-image", title: "Krea 2 Large", speed: "25 to 60 seconds", strengths: "Photorealism, raw textured looks such as motion blur, grain and film",
			price: "$0.060 from text, $0.065 with style references", style: "aspect_ratio", defaults: map[string]any{"creativity": "medium"},
			supports: []string{"prompt", "aspect_ratio", "creativity", "seed", "image_style_references"}},
		{id: "bytedance/seedream/v5/pro/text-to-image", title: "Seedream 5.0 Pro", speed: "About 10 seconds", strengths: "ByteDance's best, busy layouts, text in 14 languages",
			price: "$0.0675 an image up to 1536 by 1536",
			sizes: map[string]any{"landscape": map[string]any{"width": 2048, "height": 1152}, "square": map[string]any{"width": 1536, "height": 1536},
				"portrait": map[string]any{"width": 1152, "height": 2048}},
			defaults: map[string]any{"num_images": 1, "output_format": "png", "enable_safety_checker": false},
			supports: []string{"prompt", "image_size", "num_images", "output_format", "sync_mode", "enable_safety_checker"},
			edit:     "bytedance/seedream/v5/pro/edit", references: 10,
			editSupports: []string{"prompt", "image_urls", "image_size", "num_images", "output_format", "sync_mode", "enable_safety_checker"}},
		{id: "bytedance/seedream/v5/lite/text-to-image", title: "Seedream 5.0 Lite", speed: "About 5 seconds", strengths: "The fast and cheap Seedream, high resolution", price: "$0.035 an image",
			defaults: map[string]any{"num_images": 1, "enable_safety_checker": false},
			supports: []string{"prompt", "image_size", "num_images", "max_images", "sync_mode", "enable_safety_checker"}},
		{id: "ideogram/v4/instant", title: "Ideogram V4 Instant", speed: "Under 1 second", strengths: "The newest Ideogram lettering, posters and logos, at once", price: "$0.0075 a megapixel",
			defaults: map[string]any{"expansion_model": "Medium", "output_format": "png", "enable_safety_checker": false},
			supports: []string{"prompt", "image_size", "expansion_model", "num_images", "seed", "sync_mode", "enable_safety_checker", "output_format"}},
		{id: "ideogram/v4/fast", title: "Ideogram V4 Fast", speed: "About 1 second", strengths: "Ideogram V4 at each of its quality levels", price: "$0.005 to $0.018 a megapixel",
			defaults: map[string]any{"expansion_model": "Medium", "rendering_speed": "BALANCED"},
			supports: []string{"prompt", "image_size", "expansion_model", "rendering_speed", "num_images", "seed", "sync_mode"}},
		{id: "alibaba/qwen-image-3/text-to-image", title: "Qwen Image 3", speed: "About 8 seconds", strengths: "Complex text in Chinese and English, resolution set by the prompt",
			price:    "$0.04 an image at 1K, $0.075 at 2K",
			defaults: map[string]any{"num_images": 1, "output_format": "png", "enable_prompt_expansion": false, "enable_safety_checker": false},
			supports: []string{"prompt", "negative_prompt", "image_size", "num_images", "seed", "sync_mode", "output_format", "enable_prompt_expansion",
				"enable_safety_checker"},
			edit: "alibaba/qwen-image-3/edit", references: 3,
			editSupports: []string{"prompt", "image_urls", "negative_prompt", "num_images", "seed", "sync_mode", "output_format", "enable_prompt_expansion",
				"enable_safety_checker"}},
		{id: "microsoft/mai-image-2.5-pro", title: "MAI Image 2.5 Pro", speed: "About 10 seconds", strengths: "Microsoft's best, standout images, precise lettering",
			price: "About $0.17 an image", style: "aspect_ratio", defaults: map[string]any{"num_images": 1, "output_format": "png"},
			supports: []string{"prompt", "aspect_ratio", "num_images", "output_format", "sync_mode"}},
		{id: "google/nano-banana-2-lite", title: "Nano Banana 2 Lite", speed: "Under 2 seconds", strengths: "A Gemini image model, under 2 seconds, 14 aspect ratios including extreme ones",
			price: "About $0.04 an image, always at 1K", style: "aspect_ratio", defaults: map[string]any{"num_images": 1, "output_format": "png", "safety_tolerance": "5"},
			supports: []string{"prompt", "aspect_ratio", "num_images", "seed", "output_format", "safety_tolerance", "sync_mode", "system_prompt", "limit_generations",
				"thinking_level"},
			edit: "google/nano-banana-2-lite/edit", references: 4,
			editSupports: []string{"prompt", "image_urls", "aspect_ratio", "num_images", "seed", "output_format", "safety_tolerance", "sync_mode", "system_prompt"}},
		{id: "fal-ai/recraft/v4.1/text-to-image", title: "Recraft V4.1", speed: "About 8 seconds", strengths: "Design-first images, brand work and editorial", price: "$0.035 an image",
			defaults: map[string]any{"enable_safety_checker": false}, supports: []string{"prompt", "image_size", "enable_safety_checker", "colors", "background_color"}},
		{id: "xai/grok-imagine-image/v2.0/text-to-image", title: "Grok Imagine Image 2.0", speed: "About 5 seconds",
			strengths: "xAI, lettering and layout good enough for design, follows instructions", price: "$0.06 an image at 1K medium quality", style: "aspect_ratio",
			defaults: map[string]any{"num_images": 1, "output_format": "png", "resolution": "1k", "quality": "medium"},
			supports: []string{"prompt", "aspect_ratio", "num_images", "output_format", "resolution", "quality", "sync_mode"},
			edit:     "xai/grok-imagine-image/v2.0/edit", references: 3,
			editSupports: []string{"prompt", "image_urls", "num_images", "output_format", "resolution", "quality", "sync_mode"}},
		{id: "fal-ai/kling-image/v3/text-to-image", title: "Kling Image v3", speed: "About 10 seconds", strengths: "Kuaishou, realistic detail, cheap 2K, many aspect ratios",
			price: "$0.028 an image", style: "aspect_ratio", defaults: map[string]any{"num_images": 1, "output_format": "png", "resolution": "2K"},
			supports: []string{"prompt", "aspect_ratio", "num_images", "output_format", "resolution", "negative_prompt", "sync_mode"},
			edit:     "fal-ai/kling-image/v3/image-to-image", references: 1, editParam: "image_url",
			editSupports: []string{"prompt", "image_url", "aspect_ratio", "num_images", "output_format", "resolution", "sync_mode"}},
		{id: "meta/muse-image/text-to-image", title: "Meta Muse Image", speed: "About 5 seconds", strengths: "Meta, realism and lettering at a low price", price: "$0.01 an image",
			style: "aspect_ratio", defaults: map[string]any{"num_images": 1, "output_format": "png"},
			supports: []string{"prompt", "aspect_ratio", "num_images", "output_format", "sync_mode"},
			edit:     "meta/muse-image/edit", references: 10, editSupports: []string{"prompt", "image_urls", "num_images", "output_format", "sync_mode"}},
	}
	for i, p := range list {
		if p.style == "" {
			list[i].style = "image_size_preset"
		}
		if p.sizes == nil {
			list[i].sizes = presetSizes
			if list[i].style == "aspect_ratio" {
				list[i].sizes = aspectSizes
			}
		}
	}
	return list
}()

// Clarity upscaler of FAL and what it is sent
const (
	upscaler       = "fal-ai/clarity-upscaler"
	upscaleFactor  = 2
	upscalerPrompt = "masterpiece, best quality, highres"
)

var upscalerSettings = map[string]any{"upscale_factor": upscaleFactor, "negative_prompt": "(worst quality, low quality, normal quality:2)", "creativity": 0.35,
	"resemblance": 0.6, "guidance_scale": 4, "num_inference_steps": 18, "enable_safety_checker": false}

// Largest data URLs of source pictures sent inline in all, past which each is shrunk to its share
const inlineBudget = 3_500_000

// Picture model the settings name, the default when they name one FAL doesn't have, said in a warning when warn
func chosenPicture(model string, warn bool) picture {
	if model != "" {
		if i := slices.IndexFunc(pictures, func(p picture) bool { return p.id == model }); i >= 0 {
			return pictures[i]
		}
		if warn {
			slog.Warn(fmt.Sprintf("Unknown FAL model '%s' in config; falling back to %s", model, defaultPicture))
		}
	}
	return pictures[slices.IndexFunc(pictures, func(p picture) bool { return p.id == defaultPicture })]
}

// Most pictures the model takes when it edits, one at least
func (p picture) most() int {
	return max(p.references, 1)
}

func imageModels(context.Context, *config.Config) []generation.Model {
	models := make([]generation.Model, len(pictures))
	for i, p := range pictures {
		models[i] = generation.Model{ID: p.id, Title: p.title, Speed: p.speed, Strengths: p.strengths, Price: p.price}
	}
	return models
}

// What the picture model the settings name takes: edits when it has an edit endpoint, the upscale pass always
func imageTakes(cfg *config.Config) generation.ImageTakes {
	p := chosenPicture(strings.TrimSpace(cfg.GetString(falconfig.Model)), false)
	if p.edit == "" {
		return generation.ImageTakes{Upscale: true}
	}
	return generation.ImageTakes{Edits: true, References: p.most(), Upscale: true}
}

// Why FAL can't draw, empty when it can
func missing(cfg *config.Config, kind generation.Kind) string {
	if ready(cfg) {
		return ""
	}
	if chosen := generation.Chosen(cfg, kind); chosen != "" {
		section := "image_gen"
		if kind == generation.Video {
			section = "video_gen"
		}
		return section + " is configured to use " + chosen + ", but FAL_KEY is not set. The user sets the FAL API key with /" +
			map[string]string{"image_gen": "images", "video_gen": "videos"}[section] + " or picks another service there."
	}
	return strings.Join([]string{"Image generation is unavailable in this environment.", "", "Missing requirements:", "  - FAL_KEY environment variable is not set",
		"", "To enable image generation, the user does one of:", "  1. Gets an API key at https://fal.ai and sets FAL_KEY=<your-key>, or the FAL API key with /images",
		"  2. Picks another image service with /images"}, "\n")
}

// Draws or edits through FAL's queue, keeping the picture in the cache with its FAL address beside it
func drawImage(ctx context.Context, job *generation.Job, request generation.ImageRequest) generation.Result {
	cfg := job.Config
	p := chosenPicture(request.Model, true)
	prompt := strings.TrimSpace(request.Prompt)
	aspect := strings.ToLower(strings.TrimSpace(request.Aspect))
	if !slices.Contains(generation.Aspects, aspect) {
		slog.Warn(fmt.Sprintf("Invalid aspect_ratio '%s', defaulting to '%s'", request.Aspect, generation.DefaultAspect))
		aspect = generation.DefaultAspect
	}
	call := generation.Call{Provider: falconfig.Name, Model: p.id, Prompt: request.Prompt, Aspect: aspect}
	if prompt == "" {
		return call.Fail("Prompt is required and must be a non-empty string", "invalid_argument")
	}
	if why := missing(cfg, generation.Image); why != "" {
		return call.Fail(why, "auth_required")
	}
	sources := request.Sources(-1)
	if len(sources) > 0 && p.edit == "" {
		return call.Fail(fmt.Sprintf("Model '%s' (%s) is not capable of image-to-image / editing. Provide a text-only prompt (omit image_url), or the user picks an "+
			"edit-capable model as the model of FAL with /images.", p.title, p.id), "modality_unsupported")
	}
	endpoint, modality := p.id, "text"
	var arguments map[string]any
	if len(sources) > 0 {
		endpoint, modality = p.edit, "image"
		inlined, err := inline(job, sources[:min(len(sources), p.most())])
		if err != nil {
			return call.Fail("Could not read source image: "+err.Error(), "invalid_image_input")
		}
		arguments = payload(p, prompt, aspect, inlined)
		slog.Info(fmt.Sprintf("Editing image with %s (%s) — %d source image(s), prompt: %s", p.title, endpoint, len(inlined), strs.Head(prompt, 80)))
	} else {
		arguments = payload(p, prompt, aspect, nil)
		slog.Info(fmt.Sprintf("Generating image with %s (%s) — prompt: %s", p.title, p.id, strs.Head(prompt, 80)))
	}
	result, err := run(ctx, cfg, endpoint, arguments)
	if err != nil {
		return failed(call, err)
	}
	images, ok := result["images"].([]any)
	switch {
	case !ok:
		return call.Fail("Invalid response from FAL.ai API — no images returned", "empty_response")
	case len(images) == 0:
		return call.Fail("No images were generated", "empty_response")
	}
	upscale := request.Upscale != nil && *request.Upscale
	var address string
	upscaled := false
	for _, raw := range images {
		image, _ := raw.(map[string]any)
		u, _ := image["url"].(string)
		if u == "" {
			continue
		}
		if upscale {
			up, err := enlarge(ctx, cfg, u, prompt)
			if errors.Is(err, errInterrupted) {
				return failed(call, err)
			}
			if up != "" {
				address, upscaled = up, true
				break
			}
			slog.Warn("Using original image as fallback (upscale failed)")
		}
		address = u
		break
	}
	if address == "" {
		return call.Fail("No valid image URLs returned from API", "empty_response")
	}
	extra := map[string]any{"upscaled": upscaled, "endpoint": endpoint}
	if upscaled {
		extra["upscale_factor"] = upscaleFactor
	}
	embedded := strings.HasPrefix(address, "data:")
	if !embedded {
		extra["url"] = address
	}
	saved, err := job.Download(ctx, generation.Image, address, generation.Fetch{Prefix: "fal_" + p.id})
	switch {
	case err != nil && embedded:
		return call.Fail(fmt.Sprintf("Could not save image to cache: %v", err), "io_error")
	case err != nil:
		slog.Warn(fmt.Sprintf("FAL image URL %s could not be cached (%v); falling back to bare URL.", address, err))
		saved = address
	}
	slog.Info(fmt.Sprintf("Generated %d image(s) via %s [%s]", len(images), endpoint, modality))
	return call.Made(saved, modality, extra)
}

// Result of an application, waited for in the queue
func run(ctx context.Context, cfg *config.Config, application string, arguments map[string]any) (map[string]any, error) {
	h, err := submit(ctx, cfg, application, arguments)
	if err != nil {
		return nil, err
	}
	return h.wait(ctx)
}

// Failure of a call whose FAL request failed, an interruption saying the job was given up
func failed(call generation.Call, err error) generation.Result {
	var status *statusError
	switch {
	case errors.Is(err, errInterrupted):
		return call.Fail("Image generation interrupted by user — abandoned the in-flight FAL job.", "interrupted")
	case errors.As(err, &status):
		return call.Fail(fmt.Sprintf("FAL answered %d: %s", status.Status, status.Message), "api_error")
	case errors.Is(err, context.DeadlineExceeded):
		return call.Fail("FAL request timed out: "+err.Error(), "timeout")
	}
	return call.Fail("Error generating image: "+err.Error(), "api_error")
}

// Request of the model: its defaults, the prompt, the pictures to edit, the size of the shape, then only the keys the endpoint takes
func payload(p picture, prompt, aspect string, images []string) map[string]any {
	supports := p.supports
	if images != nil {
		supports = p.editSupports
	}
	out := maps.Clone(p.defaults)
	if out == nil {
		out = map[string]any{}
	}
	out["prompt"] = prompt
	required := []string{"prompt"}
	if images != nil {
		param := cmp.Or(p.editParam, "image_urls")
		if param == "image_urls" {
			list := make([]any, len(images))
			for i, image := range images {
				list[i] = image
			}
			out[param] = list
		} else {
			out[param] = images[0]
		}
		required = append(required, param)
	}
	if key, ok := sizeKeys[p.style]; ok && (images == nil || slices.Contains(supports, key)) {
		out[key] = p.sizes[aspect]
	}
	for name := range out {
		if !slices.Contains(supports, name) && !slices.Contains(required, name) {
			delete(out, name)
		}
	}
	return out
}

// Sources as FAL takes them: URLs as they are, files of this machine as data URLs, shrunk to share the inline budget when they pass it
func inline(job *generation.Job, sources []string) ([]string, error) {
	out := make([]string, len(sources))
	total, inlined := 0, 0
	for i, source := range sources {
		if generation.Remote(source) {
			out[i] = source
		} else {
			data, err := job.Local(source)
			if err != nil {
				return nil, err
			}
			kind := generation.Sniff(data)
			if kind == "" {
				return nil, &generation.Refusal{Text: "source is not a recognized image: " + source}
			}
			out[i] = generation.DataURL(kind, data)
		}
		if strings.HasPrefix(out[i], "data:") {
			total += len(out[i])
			inlined++
		}
	}
	if total <= inlineBudget {
		return out, nil
	}
	share := inlineBudget / inlined
	for i, u := range out {
		if strings.HasPrefix(u, "data:") && len(u) > share {
			out[i] = shrink(u, share)
		}
	}
	return out, nil
}

// Clarity upscale of a picture, its new address; empty when the pass failed, an interruption passed on
func enlarge(ctx context.Context, cfg *config.Config, address, prompt string) (string, error) {
	slog.Info("Upscaling image with Clarity Upscaler...")
	arguments := maps.Clone(upscalerSettings)
	arguments["image_url"], arguments["prompt"] = address, upscalerPrompt+", "+prompt
	result, err := run(ctx, cfg, upscaler, arguments)
	if errors.Is(err, errInterrupted) {
		return "", err
	}
	if err != nil {
		slog.Error(fmt.Sprintf("Error upscaling image: %v", err))
		return "", nil
	}
	image, ok := result["image"].(map[string]any)
	if !ok {
		slog.Error("Upscaler returned invalid response")
		return "", nil
	}
	u, _ := image["url"].(string)
	if u == "" {
		slog.Error("Error upscaling image: the upscale reply has no url")
		return "", nil
	}
	slog.Info(fmt.Sprintf("Image upscaled successfully to %vx%v", unknown(image["width"]), unknown(image["height"])))
	return u, nil
}

// The value, or the word unknown for none
func unknown(value any) any {
	if value == nil {
		return "unknown"
	}
	return value
}
