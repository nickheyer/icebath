package generate

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/generation"
	falconfig "github.com/nickheyer/icebath/internal/plugins/defaults/generators/fal/config"
)

// One video model family of FAL, its endpoints and the keys each request takes
type family struct {
	id, title, speed, tier, strengths string
	text, image                       string
	aspects, resolutions              []string
	// Bounds of its lengths in seconds, or the lengths it takes when they are few, and a lower bound at some resolutions
	durations []int
	steps     []int
	caps      map[string]int
	// Sends the length as a number rather than text, the text with this after it
	whole  bool
	suffix string
	// Takes the audio toggle, makes audio whatever is asked, takes a negative prompt and a seed
	audio, always, negative, seed bool
	// Keys of the still and the audio toggle when not image_url and generate_audio, keys dropped when a still is sent
	imageKey, audioKey string
	drops              []string
	aliases            map[string]string
	fixed              map[string]any
}

const defaultFamily = "pixverse-v6"

var (
	six       = []string{"21:9", "16:9", "4:3", "1:1", "3:4", "9:16"}
	wide      = []string{"16:9", "9:16"}
	three     = []string{"16:9", "9:16", "1:1"}
	h3        = map[string]string{"480p": "768P", "540p": "768P", "720p": "768P", "768p": "768P", "1080p": "2K", "2k": "2K", "4k": "4K", "2160p": "4K"}
	h3Max     = map[string]string{"480p": "480P", "540p": "480P", "720p": "768P", "768p": "768P", "1080p": "768P", "2k": "768P", "4k": "768P", "2160p": "768P"}
	h3Turbo   = map[string]string{"480p": "480P", "540p": "480P", "720p": "768P", "768p": "768P", "1080p": "1080P", "2k": "1080P", "4k": "1080P", "2160p": "1080P"}
	balanced  = map[string]any{"prompt_expansion_mode": "balanced"}
	evenSteps = []int{6, 8, 10, 12, 14, 16, 18, 20}
)

// FAL's video families in the order the picker lists them
var families = []family{
	{id: "ltx-2.3", title: "LTX 2.3 (22B)", speed: "~30-60s", tier: "cheap", strengths: "22B model with native audio generation. Affordable.",
		text: "fal-ai/ltx-2.3-22b/text-to-video", image: "fal-ai/ltx-2.3-22b/image-to-video", audio: true, negative: true, seed: true},
	{id: "ltx-2.5", title: "LTX 2.5", speed: "~30-90s", tier: "cheap",
		strengths: "Lightricks open-source audio-video model. Native audio, up to 20s / 4K (i2v), camera-motion presets.",
		text:      "lightricks/ltx-2.5/text-to-video/fast", image: "lightricks/ltx-2.5/image-to-video/fast", whole: true, aspects: wide,
		resolutions: []string{"720p", "1080p", "1440p", "2160p"}, aliases: map[string]string{"2k": "1440p", "4k": "2160p"},
		durations: []int{6, 20}, steps: evenSteps, caps: map[string]int{"1440p": 10, "2160p": 10}, audio: true},
	{id: "pixverse-v6", title: "Pixverse v6", speed: "~30-90s", tier: "cheap", strengths: "Affordable. Negative prompts. 1-15s durations.",
		text: "fal-ai/pixverse/v6/text-to-video", image: "fal-ai/pixverse/v6/image-to-video", resolutions: []string{"360p", "540p", "720p", "1080p"},
		durations: []int{1, 15}, audio: true, negative: true, seed: true},
	{id: "seedance-2.0-mini", title: "Seedance 2.0 Mini", speed: "~30-90s", tier: "cheap", strengths: "ByteDance. Faster/cheaper Seedance tier, audio + lip-sync, 4-15s.",
		text: "bytedance/seedance-2.0/mini/text-to-video", image: "bytedance/seedance-2.0/mini/image-to-video", aspects: six, resolutions: []string{"480p", "720p"},
		durations: []int{4, 15}, audio: true},
	{id: "veo3.1", title: "Veo 3.1", speed: "~60-120s", tier: "premium", strengths: "Google DeepMind. Cinematic, native audio, strong prompt adherence.",
		text: "fal-ai/veo3.1", image: "fal-ai/veo3.1/image-to-video", aspects: wide, resolutions: []string{"720p", "1080p", "4k"}, durations: []int{4, 8},
		steps: []int{4, 6, 8}, suffix: "s", audio: true, negative: true, seed: true},
	{id: "seedance-2.0", title: "Seedance 2.0", speed: "~60-120s", tier: "premium", strengths: "ByteDance. Cinematic, synchronized audio + lip-sync, 4-15s.",
		text: "bytedance/seedance-2.0/text-to-video", image: "bytedance/seedance-2.0/image-to-video", aspects: six, resolutions: []string{"480p", "720p", "1080p"},
		durations: []int{4, 15}, audio: true},
	{id: "seedance-2.5", title: "Seedance 2.5", speed: "~60-180s", tier: "premium",
		strengths: "ByteDance flagship. Native 30s single-pass, audio in the same latent space, lip-sync.",
		text:      "bytedance/seedance-2.5/text-to-video", image: "bytedance/seedance-2.5/image-to-video", aspects: six, drops: []string{"aspect_ratio"},
		resolutions: []string{"480p", "720p"}, durations: []int{4, 30}, audio: true},
	{id: "minimax-h3", title: "MiniMax H3", speed: "~60-180s", tier: "premium", strengths: "MiniMax frontier. Native 2K (up to 4K), 5-15s, seven aspect ratios.",
		text: "minimax/h3/text-to-video", image: "minimax/h3/image-to-video", whole: true, drops: []string{"aspect_ratio"}, aspects: six,
		resolutions: []string{"768P", "2K", "4K"}, aliases: h3, durations: []int{5, 15}, always: true},
	{id: "minimax-h3-max", title: "MiniMax H3 Max (fal post-train)", speed: "~5-30s", tier: "premium",
		strengths: "fal's post-trained MiniMax H3. Top-ranked quality/prompt adherence/aesthetics, 768p in seconds, 5-15s.",
		text:      "minimax/h3-max/text-to-video", image: "minimax/h3-max/image-to-video", whole: true, drops: []string{"aspect_ratio"}, aspects: six,
		resolutions: []string{"480P", "768P"}, aliases: h3Max, durations: []int{5, 15}, fixed: balanced, always: true, seed: true},
	{id: "minimax-h3-max-turbo", title: "MiniMax H3 Max Turbo (fal post-train)", speed: "~5-20s", tier: "premium",
		strengths: "fal's throughput-tuned H3 Max variant. Near-Max quality at a fraction of the price/latency, 480P-1080P, 5-15s.",
		text:      "minimax/h3-max-turbo/text-to-video", image: "minimax/h3-max-turbo/image-to-video", whole: true, drops: []string{"aspect_ratio"}, aspects: six,
		resolutions: []string{"480P", "768P", "1080P"}, aliases: h3Turbo, durations: []int{5, 15}, fixed: balanced, always: true, seed: true},
	{id: "flux-3", title: "FLUX 3 (via FAL)", speed: "~60-120s", tier: "premium", strengths: "Black Forest Labs frontier video. Native audio, 5-20s, 8 aspect ratios.",
		text: "blackforestlabs/flux-3/text-to-video", image: "blackforestlabs/flux-3/image-to-video", whole: true,
		aspects: []string{"21:9", "2:1", "16:9", "4:3", "1:1", "3:4", "9:16"}, resolutions: []string{"720p", "1080p"}, durations: []int{5, 20}, audio: true},
	{id: "grok-imagine-1.5", title: "Grok Imagine 1.5 (via FAL)", speed: "~30-90s", tier: "premium", strengths: "xAI. Fast stylized video with audio, 1-15s, cheap per second.",
		text: "xai/grok-imagine-video/v1.5/text-to-video", image: "xai/grok-imagine-video/v1.5/image-to-video", whole: true, drops: []string{"aspect_ratio"},
		aspects: []string{"16:9", "4:3", "3:2", "1:1", "2:3", "3:4", "9:16"}, resolutions: []string{"480p", "720p", "1080p"}, durations: []int{1, 15}, always: true},
	{id: "gemini-omni-flash", title: "Gemini Omni Flash 1.1 (via FAL)", speed: "~60-120s", tier: "premium",
		strengths: "Google. Text & image to video with native audio, physics-grounded motion, up to 4K, 3-10s.",
		text:      "google/gemini-omni-flash/v1.1/text-to-video", image: "google/gemini-omni-flash/v1.1/image-to-video", whole: true, aspects: wide,
		resolutions: []string{"360p", "720p", "1080p", "4k"}, durations: []int{3, 10}, always: true},
	{id: "kling-v3", title: "Kling 3.0 (Standard)", speed: "~60-180s", tier: "premium", strengths: "Kuaishou frontier core model. Cinematic motion, native audio, 3-15s.",
		text: "fal-ai/kling-video/v3/standard/text-to-video", image: "fal-ai/kling-video/v3/standard/image-to-video", imageKey: "start_image_url",
		drops: []string{"aspect_ratio"}, aspects: three, durations: []int{3, 15}, audio: true, negative: true},
	{id: "kling-v3-pro", title: "Kling 3.0 Pro", speed: "~60-180s", tier: "premium", strengths: "Kling 3.0 top quality tier. Cinematic motion, native audio, 3-15s.",
		text: "fal-ai/kling-video/v3/pro/text-to-video", image: "fal-ai/kling-video/v3/pro/image-to-video", imageKey: "start_image_url",
		drops: []string{"aspect_ratio"}, aspects: three, durations: []int{3, 15}, audio: true, negative: true},
	{id: "kling-v3-4k", title: "Kling v3 4K", speed: "~120-300s", tier: "premium", strengths: "4K output, native audio (Chinese/English), 3-15s.",
		text: "fal-ai/kling-video/v3/4k/text-to-video", image: "fal-ai/kling-video/v3/4k/image-to-video", imageKey: "start_image_url", aspects: three,
		durations: []int{3, 15}, audio: true, negative: true, seed: true},
	{id: "kling-o3", title: "Kling O3 (Standard)", speed: "~60-180s", tier: "premium", strengths: "Kuaishou frontier. Multi-shot native storytelling, optional audio, 3-15s.",
		text: "fal-ai/kling-video/o3/standard/text-to-video", image: "fal-ai/kling-video/o3/standard/image-to-video", drops: []string{"aspect_ratio"},
		aspects: three, durations: []int{3, 15}, audio: true},
	{id: "wan-3.0", title: "Wan 3.0", speed: "~60-180s", tier: "premium", strengths: "Alibaba latest gen. 2-30s clips, native audio, up to 1080p, lip-sync.",
		text: "alibaba/wan-3.0/text-to-video", image: "alibaba/wan-3.0/image-to-video", imageKey: "start_image_url", whole: true, audioKey: "audio",
		aspects: []string{"16:9", "4:3", "1:1", "3:4", "9:16"}, resolutions: []string{"480p", "720p", "1080p"}, durations: []int{2, 30}, audio: true, seed: true},
	{id: "wan-3.0-prime", title: "Wan 3.0 Prime", speed: "~60-180s", tier: "premium", strengths: "Alibaba premium tier. Faster iteration, higher fidelity, 2-30s, native audio.",
		text: "alibaba/wan-3.0-prime/text-to-video", image: "alibaba/wan-3.0-prime/image-to-video", imageKey: "start_image_url", whole: true, audioKey: "audio",
		aspects: []string{"16:9", "4:3", "1:1", "3:4", "9:16"}, resolutions: []string{"480p", "720p", "1080p"}, durations: []int{2, 30}, audio: true, seed: true},
	{id: "happy-horse", title: "Happy Horse 1.1", speed: "~60-120s", tier: "premium",
		strengths: "Alibaba flagship. 1080p, native audio + multilingual lip-sync, 3-15s, nine aspect ratios.",
		text:      "alibaba/happy-horse/v1.1/text-to-video", image: "alibaba/happy-horse/v1.1/image-to-video", whole: true, drops: []string{"aspect_ratio"},
		aspects: []string{"16:9", "9:16", "1:1", "4:3", "3:4", "21:9", "9:21", "5:4", "4:5"}, resolutions: []string{"720p", "1080p"}, durations: []int{3, 15},
		always: true, seed: true},
}

// SeedVR2 upscaler of FAL's videos
const (
	videoUpscaler      = "fal-ai/seedvr/upscale/video"
	videoUpscaleFactor = 2
)

// Family a name gives: its id, one of its endpoints, an endpoint's stem, or a path holding its id, the longest id winning; none when it
// names none
func familyOf(name string) (family, bool) {
	name = strings.TrimSpace(name)
	if name == "" {
		return family{}, false
	}
	if i := slices.IndexFunc(families, func(f family) bool { return f.id == name }); i >= 0 {
		return families[i], true
	}
	for _, f := range families {
		if name == f.text || name == f.image {
			return f, true
		}
	}
	for _, f := range families {
		for _, endpoint := range []string{f.text, f.image} {
			if rest, ok := strings.CutPrefix(endpoint, name+"/"); ok && endpoint != "" {
				if leaf, _, _ := strings.Cut(rest, "/"); leaf == "text-to-video" || leaf == "image-to-video" {
					return f, true
				}
			}
		}
	}
	segments := strings.Split(name, "/")
	var best family
	for _, f := range families {
		if slices.Contains(segments, f.id) && len(f.id) > len(best.id) {
			best = f
		}
	}
	return best, best.id != ""
}

// Family the settings give: video_gen.fal.model, else the one asked for, else the default
func chosenFamily(cfg *config.Config, model string) family {
	for _, name := range []string{cfg.GetString(falconfig.VideoModel), model} {
		if f, ok := familyOf(name); ok {
			return f
		}
	}
	f, _ := familyOf(defaultFamily)
	return f
}

// What the family makes videos from, text before image
func (f family) modalities() []string {
	var out []string
	if f.text != "" {
		out = append(out, "text")
	}
	if f.image != "" {
		out = append(out, "image")
	}
	return out
}

// Shortest and longest length, one second when it says none
func (f family) bounds() (int, int) {
	if len(f.durations) == 0 {
		return 1, 1
	}
	return min(f.durations[0], f.durations[1]), max(f.durations[0], f.durations[1])
}

func videoModels(context.Context, *config.Config) []generation.Model {
	models := make([]generation.Model, len(families))
	for i, f := range families {
		lo, hi := f.bounds()
		models[i] = generation.Model{ID: f.id, Title: f.title, Speed: f.speed, Strengths: f.strengths, Price: f.tier, Modalities: f.modalities(), MinDuration: lo, MaxDuration: hi}
		if len(f.durations) == 0 {
			models[i].MinDuration, models[i].MaxDuration = 0, 0
		}
	}
	return models
}

// What the family the settings give takes, so the arguments of video_generate follow the model in use
func videoTakes(cfg *config.Config) generation.VideoTakes {
	f := chosenFamily(cfg, generation.Named(cfg, generation.Video))
	lo, hi := f.bounds()
	return generation.VideoTakes{Modalities: f.modalities(), Aspects: f.aspects, Resolutions: f.resolutions, MinDuration: lo, MaxDuration: hi,
		Audio: f.audio, AudioAlways: f.always, Negative: f.negative, Seed: f.seed, Upscale: true}
}

// Length the family takes nearest the one asked for: the closest of its lengths, else within its bounds, then under the cap of the
// resolution; nil stays nil
func (f family) length(asked *int, resolution string) *int {
	if asked == nil || len(f.durations) == 0 {
		return nil
	}
	var picked int
	if len(f.steps) > 0 {
		picked = f.steps[0]
		for _, step := range f.steps[1:] {
			if abs(step-*asked) < abs(picked-*asked) {
				picked = step
			}
		}
	} else {
		picked = max(f.durations[0], min(f.durations[1], *asked))
	}
	if limit, ok := f.caps[resolution]; ok {
		picked = min(picked, limit)
	}
	return &picked
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// Request of the family: only the keys it declares, so the endpoint's own default stands for the rest
func (f family) payload(request generation.VideoRequest, prompt, image string) map[string]any {
	resolution := request.Resolution
	if alias, ok := f.aliases[strings.ToLower(resolution)]; ok {
		resolution = alias
	}
	out := map[string]any{}
	if prompt != "" {
		out["prompt"] = prompt
	}
	if image != "" {
		out[cmp.Or(f.imageKey, "image_url")] = image
	}
	if request.Seed != nil && f.seed {
		out["seed"] = *request.Seed
	}
	if slices.Contains(f.aspects, request.Aspect) {
		out["aspect_ratio"] = request.Aspect
	}
	if slices.Contains(f.resolutions, resolution) {
		out["resolution"] = resolution
	}
	if length := f.length(request.Duration, resolution); length != nil {
		if f.whole {
			out["duration"] = *length
		} else {
			out["duration"] = strconv.Itoa(*length) + f.suffix
		}
	}
	if f.audio && request.Audio != nil {
		out[cmp.Or(f.audioKey, "generate_audio")] = *request.Audio
	}
	if f.negative && request.Negative != "" {
		out["negative_prompt"] = request.Negative
	}
	if image != "" {
		for _, key := range f.drops {
			delete(out, key)
		}
	}
	for key, value := range f.fixed {
		if _, set := out[key]; !set {
			out[key] = value
		}
	}
	return out
}

// Address of the video of a result, with the video field it came in
func videoOf(result map[string]any) (map[string]any, string) {
	switch video := result["video"].(type) {
	case map[string]any:
		u, _ := video["url"].(string)
		return video, u
	case string:
		return nil, video
	}
	return nil, ""
}

// Makes a video through the family's endpoint for what it is made from, with the SeedVR2 pass when asked, the native video when it fails
func makeVideo(ctx context.Context, job *generation.Job, request generation.VideoRequest) generation.Result {
	cfg := job.Config
	f := chosenFamily(cfg, request.Model)
	prompt := strings.TrimSpace(request.Prompt)
	call := generation.Call{Provider: falconfig.Name, Prompt: request.Prompt}
	if why := missing(cfg, generation.Video); why != "" {
		if generation.Chosen(cfg, generation.Video) == "" {
			why = "No FAL backend available. Set FAL_KEY (or the FAL key with /videogen) to configure it."
		}
		return call.Fail(why, "auth_required")
	}
	call.Model = f.id
	image := strings.TrimSpace(request.Image)
	modality, endpoint := "text", f.text
	if image != "" {
		modality, endpoint = "image", f.image
	}
	if endpoint == "" {
		if modality == "image" {
			return call.Fail(fmt.Sprintf("FAL family %s has no image-to-video endpoint. Pick a family with image-to-video support with /videogen.", f.id), "modality_unsupported")
		}
		return call.Fail(fmt.Sprintf("FAL family %s has no text-to-video endpoint. Pass an image_url to use its image-to-video endpoint, or pick a different family.", f.id),
			"modality_unsupported")
	}
	if prompt == "" {
		return call.Fail("prompt is required.", "missing_prompt")
	}
	request.Prompt = prompt
	arguments := f.payload(request, prompt, image)
	result, err := run(ctx, cfg, endpoint, arguments)
	call.Aspect = request.Aspect
	if err != nil {
		slog.Warn(fmt.Sprintf("FAL video gen failed (family=%s, endpoint=%s): %v", f.id, endpoint, err))
		if errors.Is(err, errInterrupted) {
			return call.Fail("Video generation interrupted by user — abandoned the in-flight FAL job.", "interrupted")
		}
		return call.Fail(fmt.Sprintf("FAL video generation failed: %v", err), "api_error")
	}
	video, address := videoOf(result)
	if address == "" {
		call.Aspect = ""
		return call.Fail("FAL returned no video URL in response", "empty_response")
	}
	upscaled := false
	if request.Upscale != nil && *request.Upscale {
		if up := enlargeVideo(ctx, cfg, address); up != "" {
			address, upscaled = up, true
		} else {
			slog.Warn("Video upscale pass failed — returning native-resolution video")
		}
	}
	extra := map[string]any{"endpoint": endpoint, "upscaled": upscaled}
	keys := []string{"file_size", "content_type"}
	if upscaled {
		extra["upscale_factor"] = videoUpscaleFactor
		keys = []string{"content_type"}
	}
	for _, key := range keys {
		if value, ok := video[key]; ok && value != nil && value != "" {
			extra[key] = value
		}
	}
	if _, ok := arguments["aspect_ratio"]; !ok {
		call.Aspect = ""
	}
	made := call.Made(address, modality, extra)
	if length, ok := arguments["duration"]; ok {
		digits := strings.Map(func(r rune) rune {
			if r >= '0' && r <= '9' {
				return r
			}
			return -1
		}, fmt.Sprint(length))
		made.Duration, _ = strconv.Atoi(digits)
	}
	return made
}

// SeedVR2 upscale of a finished video, its new address; empty when the pass failed
func enlargeVideo(ctx context.Context, cfg *config.Config, address string) string {
	slog.Info(fmt.Sprintf("Upscaling video with SeedVR2 (%dx)...", videoUpscaleFactor))
	result, err := run(ctx, cfg, videoUpscaler, map[string]any{"video_url": address, "upscale_mode": "factor", "upscale_factor": videoUpscaleFactor})
	if err != nil {
		slog.Warn(fmt.Sprintf("Video upscale failed: %v", err))
		return ""
	}
	_, u := videoOf(result)
	if u == "" {
		slog.Warn("Video upscaler returned no URL")
	}
	return u
}
