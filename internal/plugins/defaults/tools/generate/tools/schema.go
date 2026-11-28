package tools

import (
	"fmt"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/internal/generation"
	"github.com/nickheyer/icebath/internal/tools"
)

func promptProperty() map[string]any {
	return tools.Property("string", "The text prompt describing the desired image (text-to-image) or the edit to apply (image-to-image). Be detailed and descriptive.")
}

func aspectProperty() map[string]any {
	return map[string]any{"type": "string", "enum": generation.Aspects, "default": generation.DefaultAspect,
		"description": "The aspect ratio of the generated image. 'landscape' is 16:9 wide, 'portrait' is 16:9 tall, 'square' is 1:1."}
}

// Schema of image_generate before the service in use is known
func imageSchema() map[string]any {
	return tools.Schema("Generate or edit images with the configured model. Edit/reference capabilities are rendered at serving time.", []string{"prompt"},
		map[string]any{"prompt": promptProperty(), "aspect_ratio": aspectProperty()})
}

// Creative controls a service may take, as image_generate offers them
func controls() map[string]map[string]any {
	axis := func(description string) map[string]any {
		return map[string]any{"type": "integer", "minimum": -100, "maximum": 100, "description": description}
	}
	return map[string]map[string]any{
		"creativity": {"type": "string", "enum": []string{"raw", "low", "medium", "high"}, "description": "Prompt expansion: raw (none), low, medium or high."},
		"thinking": {"type": "string", "enum": []string{"fast", "high"},
			"description": "How much the model plans before generating: fast (default) or high for complex multi-step edits."},
		"resolution": {"type": "string", "enum": []string{"1K", "2K"}, "description": "Output size: 1K (default) or 2K when small text or fine detail matters."},
		"intensity":  axis("Style intensity, -100 muted to 100 highly stylized. 0 neutral."),
		"complexity": axis("Composition density, -100 minimal to 100 dense. 0 neutral."),
		"movement":   axis("Motion in the scene, -100 static to 100 dynamic. 0 neutral."),
	}
}

// Schema of image_generate for what the model of the service in use takes: the arguments it can't use left out
func imageSchemaFor(takes generation.ImageTakes) map[string]any {
	properties := map[string]any{"prompt": promptProperty(), "aspect_ratio": aspectProperty()}
	lead, edit := "Generate images from text with the configured model (it cannot edit images).", ""
	if takes.Edits {
		refs := ""
		if takes.Style {
			lead, edit = "Generate images, optionally styled on reference images, via configured model.", "Pass style references in image_url. "
			properties["prompt"] = tools.Property("string", "The subject and scene to generate. Be detailed and descriptive; reference images supply only the look.")
			properties["image_url"] = tools.Property("string", "Style reference: its look is copied, the image itself is not edited. A public URL or an absolute local "+
				"file path. Describe the subject in the prompt. Omit for text-to-image.")
			refs = fmt.Sprintf("Up to %d more style references. URLs or absolute local paths.", takes.References)
		} else {
			lead, edit = "Generate or edit images using the configured image model.", "Pass image_url to edit / transform an existing image. "
			if takes.References > 1 {
				lead = "Generate or edit images (with reference images) using the configured model."
			}
			properties["image_url"] = tools.Property("string", "Source image to edit/transform (image-to-image). A public URL or an absolute local file path from "+
				"the conversation. Omit for text-to-image.")
			refs = fmt.Sprintf("Up to %d additional reference images (style, character, or composition) guiding an edit. URLs or absolute local paths.", takes.References)
		}
		if takes.References > 1 {
			properties["reference_image_urls"] = map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "maxItems": takes.References,
				"description": refs}
		}
	}
	if takes.Upscale {
		properties["upscale"] = tools.Property("boolean", "Post-generation high-resolution pass (~2x, extra cost/latency), off by default. A creative enhancer that can "+
			"alter fine detail (rendered text, faces) — use only when resolution matters more than fidelity.")
	}
	offered := controls()
	for _, name := range takes.Controls {
		if control, ok := offered[name]; ok {
			properties[name] = control
		}
	}
	return tools.Schema(lead+" "+edit+"Use this rather than calling image APIs or scripts directly. Returns the result in the `image` field — a URL or an "+
		"absolute file path; reference it in your response using the current platform's file-delivery convention.", []string{"prompt"}, properties)
}

// Description of video_generate whatever service is in use
const videoDescription = "Generate video from text, a still image or reference images (configured model). Pass `image_url` to animate an image or " +
	"`reference_image_urls` for reference-to-video. Video edit/extend workflows are not part of this unified surface; use a dedicated provider-specific tool " +
	"when one is available. The backend and model family are user-configured via /videos; the agent does not pick them. Long-running generations may take " +
	"30 seconds to several minutes — the call blocks until the video is ready. Returns the result in the `video` field — either an HTTP URL or an absolute " +
	"file path; reference it in your response using the current platform's file-delivery convention."

func videoProperties() map[string]map[string]any {
	return map[string]map[string]any{
		"prompt":       tools.Property("string", "Text instruction describing the desired video, motion, subject, style, camera movement, etc."),
		"duration":     tools.Property("integer", "Desired video duration in seconds. Providers clamp to their supported range. Omit for the provider default."),
		"aspect_ratio": {"type": "string", "enum": generation.VideoAspects, "description": "Output aspect ratio.", "default": generation.DefaultVideoAspect},
		"resolution":   {"type": "string", "enum": generation.VideoResolutions, "description": "Output resolution.", "default": generation.DefaultVideoResolution},
	}
}

// Schema of video_generate before the service in use is known
func videoSchema() map[string]any {
	properties := map[string]any{}
	for name, property := range videoProperties() {
		properties[name] = property
	}
	return tools.Schema(videoDescription, []string{"prompt"}, properties)
}

// Schema of video_generate for the service in use and the model the settings name: the description says what it makes videos from, the
// arguments are those it takes with the values it takes
func videoSchemaFor(takes generation.VideoTakes, model *generation.Model) map[string]any {
	static := videoProperties()
	parts := []string{videoDescription}
	modalities := takes.Modalities
	if model != nil && len(model.Modalities) > 0 {
		image, text := slices.Contains(model.Modalities, "image"), slices.Contains(model.Modalities, "text")
		switch {
		case image && !text:
			parts = append(parts, "- this model is image-to-video only — image_url is REQUIRED; text-only calls will be rejected")
		case text && !image:
			parts = append(parts, "- this model is text-to-video only — image_url is not supported")
		}
		modalities = model.Modalities
	}
	stills, text := slices.Contains(modalities, "image"), slices.Contains(modalities, "text")
	switch {
	case stills && !text:
		parts = append(parts, "- image-to-video only: image_url is REQUIRED")
	case !stills:
		parts = append(parts, "- text-to-video only (no image input)")
	}
	for _, note := range takes.Notes {
		parts = append(parts, "- "+note)
	}
	properties := map[string]any{"prompt": static["prompt"]}
	if stills {
		properties["image_url"] = tools.Property("string", "Public HTTPS URL of a still image to animate (image-to-video). Omit for text-to-video.")
		if takes.References > 0 {
			properties["reference_image_urls"] = map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "maxItems": takes.References,
				"description": fmt.Sprintf("Up to %d public HTTPS reference image URLs (style or character refs).", takes.References)}
		}
	}
	lo, hi := takes.MinDuration, takes.MaxDuration
	if model != nil && model.MaxDuration > 0 {
		lo, hi = model.MinDuration, model.MaxDuration
	}
	duration := static["duration"]
	if lo > 0 && hi > 0 {
		duration = map[string]any{"type": "integer", "minimum": lo, "maximum": hi,
			"description": fmt.Sprintf("Video duration in seconds (%d-%d). Omit for the provider default.", lo, hi)}
	}
	properties["duration"] = duration
	aspect, resolution := static["aspect_ratio"], static["resolution"]
	if len(takes.Aspects) > 0 {
		aspect = map[string]any{"type": "string", "enum": takes.Aspects, "description": "Output aspect ratio.", "default": generation.DefaultVideoAspect}
	}
	if len(takes.Resolutions) > 0 {
		resolution = map[string]any{"type": "string", "enum": takes.Resolutions, "description": "Output resolution.", "default": generation.DefaultVideoResolution}
	}
	properties["aspect_ratio"], properties["resolution"] = aspect, resolution
	if takes.Negative {
		properties["negative_prompt"] = tools.Property("string", "Content to avoid in the output.")
	}
	if takes.Audio {
		properties["audio"] = tools.Property("boolean", "Enable native audio generation (affects pricing tier).")
	}
	if takes.Seed {
		properties["seed"] = tools.Property("integer", "Seed for reproducible outputs.")
	}
	if takes.Upscale {
		properties["upscale"] = tools.Property("boolean", "High-resolution pass via the backend's video upscaler (~2x, extra cost/latency). Omit for native resolution.")
	}
	if takes.AudioAlways && !takes.Audio {
		parts = append(parts, "- audio: native stereo audio is generated with every video (always on; no toggle) — describe the desired sound in the prompt")
	}
	return tools.Schema(strings.Join(parts, "\n"), []string{"prompt"}, properties)
}
