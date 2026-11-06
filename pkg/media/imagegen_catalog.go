package media

import "github.com/nickheyer/icebath/pkg/internal/pystr"

var falPresetSizes = map[string]any{"landscape": "landscape_16_9", "square": "square_hd", "portrait": "portrait_16_9"}

var falAspectSizes = map[string]any{"landscape": "16:9", "square": "1:1", "portrait": "9:16"}

var falDefaultSizes = map[string]map[string]any{"image_size_preset": falPresetSizes, "aspect_ratio": falAspectSizes}

// Optional catalog fields of one FAL model entry
type falModelSpec struct {
	style              string
	sizes              map[string]any
	defaults           map[string]any
	supports           []string
	editEndpoint       string
	editSupports       []string
	maxReferenceImages int
	editImageParam     string
}

// String set used for supports and edit_supports whitelists
func falKeySet(keys []string) map[string]bool {
	out := make(map[string]bool, len(keys))
	for _, k := range keys {
		out[k] = true
	}
	return out
}

// Catalog entry whose edit keys exist only for edit capable models
func falModel(display, speed, strengths, price string, s falModelSpec) map[string]any {
	if s.style == "" {
		s.style = "image_size_preset"
	}
	sizes := s.sizes
	if sizes == nil {
		sizes = falDefaultSizes[s.style]
	}
	entry := map[string]any{
		"display": display, "speed": speed, "strengths": strengths, "price": price,
		"size_style": s.style, "sizes": sizes, "defaults": s.defaults, "supports": falKeySet(s.supports), "upscale": false,
	}
	if s.editEndpoint != "" {
		entry["edit_endpoint"] = s.editEndpoint
		entry["edit_supports"] = falKeySet(s.editSupports)
		entry["max_reference_images"] = s.maxReferenceImages
		if s.editImageParam != "" {
			entry["edit_image_param"] = s.editImageParam
		}
	}
	return entry
}

// FAL model ids in catalog order
var falModelOrder []string

// FAL image models keyed by endpoint id
var falModels = map[string]map[string]any{}

// Appends one model in catalog order
func addFalModel(id string, entry map[string]any) {
	falModelOrder = append(falModelOrder, id)
	falModels[id] = entry
}

func init() {
	addFalModel("fal-ai/flux-2/klein/9b", falModel("FLUX 2 Klein 9B", "<1s", "Fast, crisp text", "$0.006/MP", falModelSpec{
		defaults:           map[string]any{"num_inference_steps": 4, "output_format": "png", "enable_safety_checker": false},
		supports:           []string{"prompt", "image_size", "num_inference_steps", "seed", "output_format", "enable_safety_checker"},
		editEndpoint:       "fal-ai/flux-2/klein/9b/edit",
		editSupports:       []string{"prompt", "image_urls", "num_inference_steps", "seed", "output_format", "enable_safety_checker"},
		maxReferenceImages: 9,
	}))
	addFalModel("fal-ai/flux-2-pro", falModel("FLUX 2 Pro", "~6s", "Studio photorealism", "$0.03/MP", falModelSpec{
		defaults: map[string]any{"num_inference_steps": 50, "guidance_scale": 4.5, "num_images": 1,
			"output_format": "png", "enable_safety_checker": false, "safety_tolerance": "5", "sync_mode": true},
		supports: []string{"prompt", "image_size", "num_inference_steps", "guidance_scale", "num_images", "output_format",
			"enable_safety_checker", "safety_tolerance", "sync_mode", "seed"},
		editEndpoint: "fal-ai/flux-2-pro/edit",
		editSupports: []string{"prompt", "image_urls", "num_inference_steps", "guidance_scale", "num_images", "output_format",
			"enable_safety_checker", "safety_tolerance", "sync_mode", "seed"},
		maxReferenceImages: 9,
	}))
	addFalModel("fal-ai/z-image/turbo", falModel("Z-Image Turbo", "~2s", "Bilingual EN/CN, 6B", "$0.005/MP", falModelSpec{
		defaults: map[string]any{"num_inference_steps": 8, "num_images": 1, "output_format": "png",
			"enable_safety_checker": false, "enable_prompt_expansion": false},
		supports: []string{"prompt", "image_size", "num_inference_steps", "num_images", "seed", "output_format",
			"enable_safety_checker", "enable_prompt_expansion"},
	}))
	addFalModel("fal-ai/nano-banana-pro", falModel("Nano Banana Pro (Gemini 3 Pro Image)", "~8s", "Gemini 3 Pro, reasoning depth, text rendering", "$0.15/image (1K)", falModelSpec{
		style:    "aspect_ratio",
		defaults: map[string]any{"num_images": 1, "output_format": "png", "safety_tolerance": "5", "resolution": "1K"},
		supports: []string{"prompt", "aspect_ratio", "num_images", "output_format", "safety_tolerance", "seed", "sync_mode",
			"resolution", "enable_web_search", "limit_generations"},
		editEndpoint: "fal-ai/nano-banana-pro/edit",
		editSupports: []string{"prompt", "image_urls", "aspect_ratio", "num_images", "output_format", "safety_tolerance", "seed",
			"sync_mode", "resolution", "enable_web_search", "limit_generations"},
		maxReferenceImages: 2,
	}))
	addFalModel("fal-ai/nano-banana-2", falModel("Nano Banana 2 (Gemini 3.1 Flash Image)", "~3s", "Fast reasoning, multilingual text, infographics", "Lower-cost Flash tier", falModelSpec{
		style: "aspect_ratio",
		defaults: map[string]any{"num_images": 1, "output_format": "png", "safety_tolerance": "4",
			"resolution": "1K", "limit_generations": true},
		supports: []string{"prompt", "aspect_ratio", "num_images", "output_format", "safety_tolerance", "seed", "sync_mode",
			"system_prompt", "resolution", "enable_web_search", "limit_generations", "thinking_level"},
		editEndpoint: "fal-ai/nano-banana-2/edit",
		editSupports: []string{"prompt", "image_urls", "aspect_ratio", "num_images", "output_format", "safety_tolerance", "seed",
			"sync_mode", "system_prompt", "resolution", "enable_web_search", "limit_generations", "thinking_level"},
		maxReferenceImages: 14,
	}))
	addFalModel("fal-ai/gpt-image-1.5", falModel("GPT Image 1.5", "~15s", "Prompt adherence", "$0.034/image", falModelSpec{
		style:              "gpt_literal",
		sizes:              map[string]any{"landscape": "1536x1024", "square": "1024x1024", "portrait": "1024x1536"},
		defaults:           map[string]any{"quality": "medium", "num_images": 1, "output_format": "png"},
		supports:           []string{"prompt", "image_size", "quality", "num_images", "output_format", "background", "sync_mode"},
		editEndpoint:       "fal-ai/gpt-image-1.5/edit",
		editSupports:       []string{"prompt", "image_urls", "image_size", "quality", "num_images", "output_format", "sync_mode"},
		maxReferenceImages: 16,
	}))
	addFalModel("fal-ai/gpt-image-2", falModel("GPT Image 2", "~20s", "SOTA text rendering + CJK, world-aware photorealism", "$0.04–0.06/image", falModelSpec{
		sizes:              map[string]any{"landscape": "landscape_4_3", "square": "square_hd", "portrait": "portrait_4_3"},
		defaults:           map[string]any{"quality": "medium", "num_images": 1, "output_format": "png"},
		supports:           []string{"prompt", "image_size", "quality", "num_images", "output_format", "sync_mode"},
		editEndpoint:       "openai/gpt-image-2/edit",
		editSupports:       []string{"prompt", "image_urls", "quality", "num_images", "output_format", "sync_mode", "mask_image_url"},
		maxReferenceImages: 16,
	}))
	for _, v := range [][3]string{
		{"flare", "Fast", "Everyday creation, natural lighting and textures"},
		{"sunburst", "Slower", "Precision editing, subject and composition consistency"},
	} {
		addFalModel("openai/gpt-image-2.5/"+v[0]+"/text-to-image", falModel("GPT Image 2.5 "+pystr.Title(v[0]), v[1], v[2], "Token-based pricing", falModelSpec{
			sizes:    map[string]any{"landscape": "landscape_4_3", "square": "square_hd", "portrait": "portrait_4_3"},
			defaults: map[string]any{"quality": "medium", "num_images": 1, "output_format": "png"},
			supports: []string{"prompt", "image_size", "quality", "num_images", "output_format", "background",
				"output_compression", "sync_mode"},
			editEndpoint: "openai/gpt-image-2.5/" + v[0] + "/edit",
			editSupports: []string{"prompt", "image_urls", "image_size", "quality", "num_images", "output_format",
				"background", "output_compression", "sync_mode", "mask_url", "input_fidelity"},
			maxReferenceImages: 16,
		}))
	}
	addFalModel("fal-ai/ideogram/v3", falModel("Ideogram V3", "~5s", "Best typography", "$0.03-0.09/image", falModelSpec{
		defaults:           map[string]any{"rendering_speed": "BALANCED", "expand_prompt": true, "style": "AUTO"},
		supports:           []string{"prompt", "image_size", "rendering_speed", "expand_prompt", "style", "seed"},
		editEndpoint:       "fal-ai/ideogram/v3/edit",
		editSupports:       []string{"prompt", "image_urls", "rendering_speed", "expand_prompt", "style", "seed"},
		maxReferenceImages: 1,
	}))
	addFalModel("fal-ai/recraft/v4/pro/text-to-image", falModel("Recraft V4 Pro", "~8s", "Design, brand systems, production-ready", "$0.25/image", falModelSpec{
		defaults: map[string]any{"enable_safety_checker": false},
		supports: []string{"prompt", "image_size", "enable_safety_checker", "colors", "background_color"},
	}))
	addFalModel("fal-ai/qwen-image", falModel("Qwen Image", "~12s", "LLM-based, complex text", "$0.02/MP", falModelSpec{
		defaults: map[string]any{"num_inference_steps": 30, "guidance_scale": 2.5, "num_images": 1,
			"output_format": "png", "acceleration": "regular"},
		supports: []string{"prompt", "image_size", "num_inference_steps", "guidance_scale", "num_images", "output_format",
			"acceleration", "seed", "sync_mode"},
		editEndpoint: "fal-ai/qwen-image-2/pro/edit",
		editSupports: []string{"prompt", "image_urls", "num_inference_steps", "guidance_scale", "num_images", "output_format",
			"acceleration", "seed", "sync_mode"},
		maxReferenceImages: 3,
	}))
	addFalModel("fal-ai/krea/v2/medium/text-to-image", falModel("Krea 2 Medium", "~15-25s", "Illustration, anime, painting, expressive/artistic styles", "$0.030 (text) / $0.035 (style refs)", falModelSpec{
		style:    "aspect_ratio",
		defaults: map[string]any{"creativity": "medium"},
		supports: []string{"prompt", "aspect_ratio", "creativity", "seed", "image_style_references"},
	}))
	addFalModel("fal-ai/krea/v2/large/text-to-image", falModel("Krea 2 Large", "~25-60s", "Photorealism, raw textured looks (motion blur, grain, film)", "$0.060 (text) / $0.065 (style refs)", falModelSpec{
		style:    "aspect_ratio",
		defaults: map[string]any{"creativity": "medium"},
		supports: []string{"prompt", "aspect_ratio", "creativity", "seed", "image_style_references"},
	}))
	addFalModel("bytedance/seedream/v5/pro/text-to-image", falModel("Seedream 5.0 Pro", "~10s", "ByteDance flagship, dense layouts, native text in 14 languages", "$0.0675/image (≤1536²)", falModelSpec{
		sizes: map[string]any{
			"landscape": map[string]any{"width": 2048, "height": 1152},
			"square":    map[string]any{"width": 1536, "height": 1536},
			"portrait":  map[string]any{"width": 1152, "height": 2048},
		},
		defaults:     map[string]any{"num_images": 1, "output_format": "png", "enable_safety_checker": false},
		supports:     []string{"prompt", "image_size", "num_images", "output_format", "sync_mode", "enable_safety_checker"},
		editEndpoint: "bytedance/seedream/v5/pro/edit",
		editSupports: []string{"prompt", "image_urls", "image_size", "num_images", "output_format", "sync_mode",
			"enable_safety_checker"},
		maxReferenceImages: 10,
	}))
	addFalModel("bytedance/seedream/v5/lite/text-to-image", falModel("Seedream 5.0 Lite", "~5s", "Fast/cheap Seedream tier, high-res output", "$0.035/image", falModelSpec{
		defaults: map[string]any{"num_images": 1, "enable_safety_checker": false},
		supports: []string{"prompt", "image_size", "num_images", "max_images", "sync_mode", "enable_safety_checker"},
	}))
	addFalModel("ideogram/v4/instant", falModel("Ideogram V4 (Instant)", "<1s", "Latest Ideogram typography, posters/logos, instant", "$0.0075/MP", falModelSpec{
		defaults: map[string]any{"expansion_model": "Medium", "output_format": "png", "enable_safety_checker": false},
		supports: []string{"prompt", "image_size", "expansion_model", "num_images", "seed", "sync_mode",
			"enable_safety_checker", "output_format"},
	}))
	addFalModel("ideogram/v4/fast", falModel("Ideogram V4 (Fast)", "~1s", "Ideogram V4 quality tiers via rendering_speed", "$0.005-0.018/MP", falModelSpec{
		defaults: map[string]any{"expansion_model": "Medium", "rendering_speed": "BALANCED"},
		supports: []string{"prompt", "image_size", "expansion_model", "rendering_speed", "num_images", "seed", "sync_mode"},
	}))
	addFalModel("alibaba/qwen-image-3/text-to-image", falModel("Qwen Image 3", "~8s", "Complex CN/EN text rendering, prompt-guided resolution", "$0.04 (1K) / $0.075 (2K) per image", falModelSpec{
		defaults: map[string]any{"num_images": 1, "output_format": "png", "enable_prompt_expansion": false,
			"enable_safety_checker": false},
		supports: []string{"prompt", "negative_prompt", "image_size", "num_images", "seed", "sync_mode", "output_format",
			"enable_prompt_expansion", "enable_safety_checker"},
		editEndpoint: "alibaba/qwen-image-3/edit",
		editSupports: []string{"prompt", "image_urls", "negative_prompt", "num_images", "seed", "sync_mode", "output_format",
			"enable_prompt_expansion", "enable_safety_checker"},
		maxReferenceImages: 3,
	}))
	addFalModel("microsoft/mai-image-2.5-pro", falModel("MAI Image 2.5 Pro", "~10s", "Microsoft flagship, hero imagery, precise typography", "~$0.17/image", falModelSpec{
		style:    "aspect_ratio",
		defaults: map[string]any{"num_images": 1, "output_format": "png"},
		supports: []string{"prompt", "aspect_ratio", "num_images", "output_format", "sync_mode"},
	}))
	addFalModel("google/nano-banana-2-lite", falModel("Nano Banana 2 Lite", "<2s", "Gemini image family, sub-2s, 14 aspect ratios incl. extreme", "~$0.04/image (1K fixed)", falModelSpec{
		style:    "aspect_ratio",
		defaults: map[string]any{"num_images": 1, "output_format": "png", "safety_tolerance": "5"},
		supports: []string{"prompt", "aspect_ratio", "num_images", "seed", "output_format", "safety_tolerance", "sync_mode",
			"system_prompt", "limit_generations", "thinking_level"},
		editEndpoint: "google/nano-banana-2-lite/edit",
		editSupports: []string{"prompt", "image_urls", "aspect_ratio", "num_images", "seed", "output_format", "safety_tolerance",
			"sync_mode", "system_prompt"},
		maxReferenceImages: 4,
	}))
	addFalModel("fal-ai/recraft/v4.1/text-to-image", falModel("Recraft V4.1", "~8s", "Design-first raster, brand systems, editorial", "$0.035/image", falModelSpec{
		defaults: map[string]any{"enable_safety_checker": false},
		supports: []string{"prompt", "image_size", "enable_safety_checker", "colors", "background_color"},
	}))
	addFalModel("xai/grok-imagine-image/v2.0/text-to-image", falModel("Grok Imagine Image 2.0", "~5s", "xAI. Design-grade typography/layout, instruction following", "$0.06/image (1K medium)", falModelSpec{
		style:              "aspect_ratio",
		defaults:           map[string]any{"num_images": 1, "output_format": "png", "resolution": "1k", "quality": "medium"},
		supports:           []string{"prompt", "aspect_ratio", "num_images", "output_format", "resolution", "quality", "sync_mode"},
		editEndpoint:       "xai/grok-imagine-image/v2.0/edit",
		editSupports:       []string{"prompt", "image_urls", "num_images", "output_format", "resolution", "quality", "sync_mode"},
		maxReferenceImages: 3,
	}))
	addFalModel("fal-ai/kling-image/v3/text-to-image", falModel("Kling Image v3", "~10s", "Kuaishou. Realistic detail, cheap native 2K, wide AR set", "$0.028/image", falModelSpec{
		style:              "aspect_ratio",
		defaults:           map[string]any{"num_images": 1, "output_format": "png", "resolution": "2K"},
		supports:           []string{"prompt", "aspect_ratio", "num_images", "output_format", "resolution", "negative_prompt", "sync_mode"},
		editEndpoint:       "fal-ai/kling-image/v3/image-to-image",
		editSupports:       []string{"prompt", "image_url", "aspect_ratio", "num_images", "output_format", "resolution", "sync_mode"},
		maxReferenceImages: 1, editImageParam: "image_url",
	}))
	addFalModel("meta/muse-image/text-to-image", falModel("Meta Muse Image", "~5s", "Meta. Realism + typography at commodity price", "$0.01/image", falModelSpec{
		style:              "aspect_ratio",
		defaults:           map[string]any{"num_images": 1, "output_format": "png"},
		supports:           []string{"prompt", "aspect_ratio", "num_images", "output_format", "sync_mode"},
		editEndpoint:       "meta/muse-image/edit",
		editSupports:       []string{"prompt", "image_urls", "num_images", "output_format", "sync_mode"},
		maxReferenceImages: 10,
	}))
}

const falDefaultModel = "fal-ai/flux-2/klein/9b"

const (
	upscalerModel             = "fal-ai/clarity-upscaler"
	upscalerFactor            = 2
	upscalerSafetyChecker     = false
	upscalerDefaultPrompt     = "masterpiece, best quality, highres"
	upscalerNegativePrompt    = "(worst quality, low quality, normal quality:2)"
	upscalerCreativity        = 0.35
	upscalerResemblance       = 0.6
	upscalerGuidanceScale     = 4
	upscalerNumInferenceSteps = 18
)
