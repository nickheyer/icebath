package media

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// One FAL video model family with its endpoints and the capability flags that gate payload keys
type falFamily struct {
	Display, Speed, Price, Tier, Strengths string
	TextEndpoint, ImageEndpoint            string
	AspectRatios, Resolutions              []string
	Durations                              []int
	DurationEnum                           []int
	DurationCapByResolution                map[string]int
	Audio, Negative, Seed, AudioNative     bool
	DurationInt                            bool
	DurationSuffix                         string
	ImageParamKey, AudioParamKey           string
	ImageDropKeys                          []string
	ResolutionAliases                      map[string]string
	StaticPayload                          [][2]any
}

// Family with the given picker metadata and endpoints, every capability defaulting off
func newFalFamily(display, speed, tier, strengths, text, image string, caps func(*falFamily)) falFamily {
	f := falFamily{Display: display, Speed: speed, Price: tier, Tier: tier, Strengths: strengths, TextEndpoint: text, ImageEndpoint: image}
	if caps != nil {
		caps(&f)
	}
	return f
}

var falSixAspects = []string{"21:9", "16:9", "4:3", "1:1", "3:4", "9:16"}

var falH3Aliases = map[string]string{"480p": "768P", "540p": "768P", "720p": "768P", "768p": "768P", "1080p": "2K", "2k": "2K", "4k": "4K", "2160p": "4K"}

var falH3MaxAliases = map[string]string{"480p": "480P", "540p": "480P", "720p": "768P", "768p": "768P", "1080p": "768P", "2k": "768P", "4k": "768P", "2160p": "768P"}

var falH3MaxTurboAliases = map[string]string{"480p": "480P", "540p": "480P", "720p": "768P", "768p": "768P", "1080p": "1080P", "2k": "1080P", "4k": "1080P", "2160p": "1080P"}

// Even integers from 6 through 20
func falEvenRange(lo, hi int) []int {
	var out []int
	for d := lo; d <= hi; d += 2 {
		out = append(out, d)
	}
	return out
}

var falFamilyOrder = []string{"ltx-2.3", "ltx-2.5", "pixverse-v6", "seedance-2.0-mini", "veo3.1", "seedance-2.0", "seedance-2.5",
	"minimax-h3", "minimax-h3-max", "minimax-h3-max-turbo", "flux-3", "grok-imagine-1.5", "gemini-omni-flash", "kling-v3",
	"kling-v3-pro", "kling-v3-4k", "kling-o3", "wan-3.0", "wan-3.0-prime", "happy-horse"}

var falFamilies = map[string]falFamily{
	"ltx-2.3": newFalFamily("LTX 2.3 (22B)", "~30-60s", "cheap", "22B model with native audio generation. Affordable.",
		"fal-ai/ltx-2.3-22b/text-to-video", "fal-ai/ltx-2.3-22b/image-to-video", func(f *falFamily) { f.Audio, f.Negative, f.Seed = true, true, true }),
	"ltx-2.5": newFalFamily("LTX 2.5", "~30-90s", "cheap", "Lightricks open-source audio-video model. Native audio, up to 20s / 4K (i2v), camera-motion presets.",
		"lightricks/ltx-2.5/text-to-video/fast", "lightricks/ltx-2.5/image-to-video/fast", func(f *falFamily) {
			f.DurationInt, f.AspectRatios = true, []string{"16:9", "9:16"}
			f.Resolutions, f.ResolutionAliases = []string{"720p", "1080p", "1440p", "2160p"}, map[string]string{"2k": "1440p", "4k": "2160p"}
			f.Durations, f.DurationEnum, f.DurationCapByResolution = []int{6, 20}, falEvenRange(6, 20), map[string]int{"1440p": 10, "2160p": 10}
			f.Audio = true
		}),
	"pixverse-v6": newFalFamily("Pixverse v6", "~30-90s", "cheap", "Affordable. Negative prompts. 1-15s durations.", "fal-ai/pixverse/v6/text-to-video",
		"fal-ai/pixverse/v6/image-to-video", func(f *falFamily) {
			f.Resolutions, f.Durations, f.Audio, f.Negative, f.Seed = []string{"360p", "540p", "720p", "1080p"}, []int{1, 15}, true, true, true
		}),
	"seedance-2.0-mini": newFalFamily("Seedance 2.0 Mini", "~30-90s", "cheap", "ByteDance. Faster/cheaper Seedance tier, audio + lip-sync, 4-15s.",
		"bytedance/seedance-2.0/mini/text-to-video", "bytedance/seedance-2.0/mini/image-to-video", func(f *falFamily) {
			f.AspectRatios, f.Resolutions, f.Durations, f.Audio = falSixAspects, []string{"480p", "720p"}, []int{4, 15}, true
		}),
	"veo3.1": newFalFamily("Veo 3.1", "~60-120s", "premium", "Google DeepMind. Cinematic, native audio, strong prompt adherence.", "fal-ai/veo3.1",
		"fal-ai/veo3.1/image-to-video", func(f *falFamily) {
			f.AspectRatios, f.Resolutions, f.Durations, f.DurationEnum = []string{"16:9", "9:16"}, []string{"720p", "1080p", "4k"}, []int{4, 8}, []int{4, 6, 8}
			f.DurationSuffix, f.Audio, f.Negative, f.Seed = "s", true, true, true
		}),
	"seedance-2.0": newFalFamily("Seedance 2.0", "~60-120s", "premium", "ByteDance. Cinematic, synchronized audio + lip-sync, 4-15s.",
		"bytedance/seedance-2.0/text-to-video", "bytedance/seedance-2.0/image-to-video", func(f *falFamily) {
			f.AspectRatios, f.Resolutions, f.Durations, f.Audio = falSixAspects, []string{"480p", "720p", "1080p"}, []int{4, 15}, true
		}),
	"seedance-2.5": newFalFamily("Seedance 2.5", "~60-180s", "premium", "ByteDance flagship. Native 30s single-pass, audio in the same latent space, lip-sync.",
		"bytedance/seedance-2.5/text-to-video", "bytedance/seedance-2.5/image-to-video", func(f *falFamily) {
			f.AspectRatios, f.ImageDropKeys, f.Resolutions, f.Durations, f.Audio = falSixAspects, []string{"aspect_ratio"}, []string{"480p", "720p"}, []int{4, 30}, true
		}),
	"minimax-h3": newFalFamily("MiniMax H3", "~60-180s", "premium", "MiniMax frontier. Native 2K (up to 4K), 5-15s, seven aspect ratios.",
		"minimax/h3/text-to-video", "minimax/h3/image-to-video", func(f *falFamily) {
			f.DurationInt, f.ImageDropKeys, f.AspectRatios = true, []string{"aspect_ratio"}, falSixAspects
			f.Resolutions, f.ResolutionAliases, f.Durations, f.AudioNative = []string{"768P", "2K", "4K"}, falH3Aliases, []int{5, 15}, true
		}),
	"minimax-h3-max": newFalFamily("MiniMax H3 Max (fal post-train)", "~5-30s", "premium", "fal's post-trained MiniMax H3. Top-ranked quality/prompt "+
		"adherence/aesthetics, 768p in seconds, 5-15s.", "minimax/h3-max/text-to-video", "minimax/h3-max/image-to-video", func(f *falFamily) {
		f.DurationInt, f.ImageDropKeys, f.AspectRatios, f.Resolutions = true, []string{"aspect_ratio"}, falSixAspects, []string{"480P", "768P"}
		f.ResolutionAliases, f.Durations, f.StaticPayload = falH3MaxAliases, []int{5, 15}, [][2]any{{"prompt_expansion_mode", "balanced"}}
		f.AudioNative, f.Seed = true, true
	}),
	"minimax-h3-max-turbo": newFalFamily("MiniMax H3 Max Turbo (fal post-train)", "~5-20s", "premium", "fal's throughput-tuned H3 Max variant. Near-Max "+
		"quality at a fraction of the price/latency, 480P-1080P, 5-15s.", "minimax/h3-max-turbo/text-to-video",
		"minimax/h3-max-turbo/image-to-video", func(f *falFamily) {
			f.DurationInt, f.ImageDropKeys, f.AspectRatios = true, []string{"aspect_ratio"}, falSixAspects
			f.Resolutions, f.ResolutionAliases, f.Durations = []string{"480P", "768P", "1080P"}, falH3MaxTurboAliases, []int{5, 15}
			f.StaticPayload, f.AudioNative, f.Seed = [][2]any{{"prompt_expansion_mode", "balanced"}}, true, true
		}),
	"flux-3": newFalFamily("FLUX 3 (via FAL)", "~60-120s", "premium", "Black Forest Labs frontier video. Native audio, 5-20s, 8 aspect ratios.",
		"blackforestlabs/flux-3/text-to-video", "blackforestlabs/flux-3/image-to-video", func(f *falFamily) {
			f.DurationInt, f.AspectRatios = true, []string{"21:9", "2:1", "16:9", "4:3", "1:1", "3:4", "9:16"}
			f.Resolutions, f.Durations, f.Audio = []string{"720p", "1080p"}, []int{5, 20}, true
		}),
	"grok-imagine-1.5": newFalFamily("Grok Imagine 1.5 (via FAL)", "~30-90s", "premium", "xAI. Fast stylized video with audio, 1-15s, cheap per second.",
		"xai/grok-imagine-video/v1.5/text-to-video", "xai/grok-imagine-video/v1.5/image-to-video", func(f *falFamily) {
			f.DurationInt, f.ImageDropKeys, f.AspectRatios = true, []string{"aspect_ratio"}, []string{"16:9", "4:3", "3:2", "1:1", "2:3", "3:4", "9:16"}
			f.Resolutions, f.Durations, f.AudioNative = []string{"480p", "720p", "1080p"}, []int{1, 15}, true
		}),
	"gemini-omni-flash": newFalFamily("Gemini Omni Flash 1.1 (via FAL)", "~60-120s", "premium", "Google. Text & image to video with native audio, physics-grounded motion, up to 4K, 3-10s.",
		"google/gemini-omni-flash/v1.1/text-to-video", "google/gemini-omni-flash/v1.1/image-to-video", func(f *falFamily) {
			f.DurationInt, f.AspectRatios, f.Resolutions = true, []string{"16:9", "9:16"}, []string{"360p", "720p", "1080p", "4k"}
			f.Durations, f.AudioNative = []int{3, 10}, true
		}),
	"kling-v3": newFalFamily("Kling 3.0 (Standard)", "~60-180s", "premium", "Kuaishou frontier core model. Cinematic motion, native audio, 3-15s.",
		"fal-ai/kling-video/v3/standard/text-to-video", "fal-ai/kling-video/v3/standard/image-to-video", func(f *falFamily) {
			f.ImageParamKey, f.ImageDropKeys, f.AspectRatios = "start_image_url", []string{"aspect_ratio"}, []string{"16:9", "9:16", "1:1"}
			f.Durations, f.Audio, f.Negative = []int{3, 15}, true, true
		}),
	"kling-v3-pro": newFalFamily("Kling 3.0 Pro", "~60-180s", "premium", "Kling 3.0 top quality tier. Cinematic motion, native audio, 3-15s.",
		"fal-ai/kling-video/v3/pro/text-to-video", "fal-ai/kling-video/v3/pro/image-to-video", func(f *falFamily) {
			f.ImageParamKey, f.ImageDropKeys, f.AspectRatios = "start_image_url", []string{"aspect_ratio"}, []string{"16:9", "9:16", "1:1"}
			f.Durations, f.Audio, f.Negative = []int{3, 15}, true, true
		}),
	"kling-v3-4k": newFalFamily("Kling v3 4K", "~120-300s", "premium", "4K output, native audio (Chinese/English), 3-15s.", "fal-ai/kling-video/v3/4k/text-to-video",
		"fal-ai/kling-video/v3/4k/image-to-video", func(f *falFamily) {
			f.ImageParamKey, f.AspectRatios = "start_image_url", []string{"16:9", "9:16", "1:1"}
			f.Durations, f.Audio, f.Negative, f.Seed = []int{3, 15}, true, true, true
		}),
	"kling-o3": newFalFamily("Kling O3 (Standard)", "~60-180s", "premium", "Kuaishou frontier. Multi-shot native storytelling, optional audio, 3-15s.",
		"fal-ai/kling-video/o3/standard/text-to-video", "fal-ai/kling-video/o3/standard/image-to-video", func(f *falFamily) {
			f.ImageDropKeys, f.AspectRatios, f.Durations, f.Audio = []string{"aspect_ratio"}, []string{"16:9", "9:16", "1:1"}, []int{3, 15}, true
		}),
	"wan-3.0": newFalFamily("Wan 3.0", "~60-180s", "premium", "Alibaba latest gen. 2-30s clips, native audio, up to 1080p, lip-sync.",
		"alibaba/wan-3.0/text-to-video", "alibaba/wan-3.0/image-to-video", func(f *falFamily) {
			f.ImageParamKey, f.DurationInt, f.AudioParamKey = "start_image_url", true, "audio"
			f.AspectRatios, f.Resolutions = []string{"16:9", "4:3", "1:1", "3:4", "9:16"}, []string{"480p", "720p", "1080p"}
			f.Durations, f.Audio, f.Seed = []int{2, 30}, true, true
		}),
	"wan-3.0-prime": newFalFamily("Wan 3.0 Prime", "~60-180s", "premium", "Alibaba premium tier. Faster iteration, higher fidelity, 2-30s, native audio.",
		"alibaba/wan-3.0-prime/text-to-video", "alibaba/wan-3.0-prime/image-to-video", func(f *falFamily) {
			f.ImageParamKey, f.DurationInt, f.AudioParamKey = "start_image_url", true, "audio"
			f.AspectRatios, f.Resolutions = []string{"16:9", "4:3", "1:1", "3:4", "9:16"}, []string{"480p", "720p", "1080p"}
			f.Durations, f.Audio, f.Seed = []int{2, 30}, true, true
		}),
	"happy-horse": newFalFamily("Happy Horse 1.1", "~60-120s", "premium", "Alibaba flagship. 1080p, native audio + multilingual lip-sync, 3-15s, nine aspect ratios.",
		"alibaba/happy-horse/v1.1/text-to-video", "alibaba/happy-horse/v1.1/image-to-video", func(f *falFamily) {
			f.DurationInt, f.ImageDropKeys = true, []string{"aspect_ratio"}
			f.AspectRatios = []string{"16:9", "9:16", "1:1", "4:3", "3:4", "21:9", "9:21", "5:4", "4:5"}
			f.Resolutions, f.Durations, f.AudioNative, f.Seed = []string{"720p", "1080p"}, []int{3, 15}, true, true
		}),
}

const falVideoDefaultModel = "pixverse-v6"

// Duration snapped to the family enum or clamped into its range, then capped for the resolution, nil staying nil
func falClampDuration(family falFamily, duration *int, resolution string) *int {
	if duration == nil {
		return nil
	}
	var clamped int
	if len(family.DurationEnum) > 0 {
		clamped = family.DurationEnum[0]
		for _, d := range family.DurationEnum[1:] {
			if absInt(d-*duration) < absInt(clamped-*duration) {
				clamped = d
			}
		}
	} else {
		clamped = max(family.Durations[0], min(family.Durations[1], *duration))
	}
	if limit, ok := family.DurationCapByResolution[resolution]; ok {
		clamped = min(clamped, limit)
	}
	return &clamped
}

func absInt(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// Modalities with an endpoint, text before image
func falModalities(f falFamily) []any {
	var out []any
	if f.TextEndpoint != "" {
		out = append(out, "text")
	}
	if f.ImageEndpoint != "" {
		out = append(out, "image")
	}
	return out
}

// Known family id from a bare id, a full endpoint path, a truncated stem or a provider prefixed name
func falNormalizeFamilyKey(c string) string {
	c = strings.TrimSpace(c)
	if c == "" {
		return ""
	}
	if _, ok := falFamilies[c]; ok {
		return c
	}
	type endpoint struct{ fid, ep string }
	var endpoints []endpoint
	for _, fid := range falFamilyOrder {
		meta := falFamilies[fid]
		for _, ep := range []string{meta.TextEndpoint, meta.ImageEndpoint} {
			if ep != "" {
				endpoints = append(endpoints, endpoint{fid, ep})
			}
		}
	}
	for _, e := range endpoints {
		if c == e.ep {
			return e.fid
		}
	}
	for _, e := range endpoints {
		if strings.HasPrefix(e.ep, c+"/") {
			leaf, _, _ := strings.Cut(e.ep[len(c)+1:], "/")
			if leaf == "text-to-video" || leaf == "image-to-video" {
				return e.fid
			}
		}
	}
	segments := map[string]bool{}
	for _, s := range strings.Split(c, "/") {
		segments[s] = true
	}
	var hits []string
	for _, fid := range falFamilyOrder {
		if segments[fid] {
			hits = append(hits, fid)
		}
	}
	sort.SliceStable(hits, func(i, j int) bool { return len(hits[i]) > len(hits[j]) })
	if len(hits) > 0 {
		return hits[0]
	}
	return ""
}

// video_gen section of the active profile config.yaml, empty when unreadable or not a mapping
func videoGenConfig(ctx context.Context) config.Tree {
	if ctx == nil {
		ctx = context.Background()
	}
	h := home.Dir(ctx)
	cfg, err := config.Load(h, config.NewSecrets(h).Lookup())
	if err != nil {
		videoGenLog.Debug(fmt.Sprintf("Could not load video_gen config: %v", err))
		return config.Tree{}
	}
	return section(cfg, "video_gen")
}

// Family chosen by the tool model, FAL_VIDEO_MODEL, video_gen.fal.model, video_gen.model, then the default
var falResolveFamily = func(ctx context.Context, env *tools.Env, explicit string) (string, *falFamily, error) {
	cfg := videoGenConfig(ctx)
	candidates := []any{explicit, rawEnvValue(env, "FAL_VIDEO_MODEL"), section(cfg, "fal")["model"], cfg["model"]}
	for _, c := range candidates {
		if s, ok := c.(string); ok {
			if fid := falNormalizeFamilyKey(s); fid != "" {
				f := falFamilies[fid]
				return fid, &f, nil
			}
		}
	}
	f := falFamilies[falVideoDefaultModel]
	return falVideoDefaultModel, &f, nil
}

// Request inputs of one FAL video submit
type falPayloadInput struct {
	Prompt, ImageURL        string
	Duration                *int
	AspectRatio, Resolution string
	NegativePrompt          string
	Audio                   *bool
	Seed                    *int
}

// Family specific payload, keys the family does not declare dropped so the endpoint default applies
func falBuildVideoPayload(family falFamily, in falPayloadInput) *pyjson.Object {
	resolved := in.Resolution
	if alias, ok := family.ResolutionAliases[strings.ToLower(in.Resolution)]; ok {
		resolved = alias
	}
	var clamped *int
	if len(family.Durations) > 0 {
		clamped = falClampDuration(family, in.Duration, resolved)
	}
	payload := pyjson.NewObject()
	if in.Prompt != "" {
		payload.Set("prompt", in.Prompt)
	}
	if in.ImageURL != "" {
		key := family.ImageParamKey
		if key == "" {
			key = "image_url"
		}
		payload.Set(key, in.ImageURL)
	}
	if in.Seed != nil && family.Seed {
		payload.Set("seed", *in.Seed)
	}
	if len(family.AspectRatios) > 0 && containsString(family.AspectRatios, in.AspectRatio) {
		payload.Set("aspect_ratio", in.AspectRatio)
	}
	if len(family.Resolutions) > 0 && containsString(family.Resolutions, resolved) {
		payload.Set("resolution", resolved)
	}
	if clamped != nil {
		if family.DurationInt {
			payload.Set("duration", *clamped)
		} else {
			payload.Set("duration", strconv.Itoa(*clamped)+family.DurationSuffix)
		}
	}
	if family.Audio && in.Audio != nil {
		key := family.AudioParamKey
		if key == "" {
			key = "generate_audio"
		}
		payload.Set(key, *in.Audio)
	}
	if family.Negative && in.NegativePrompt != "" {
		payload.Set("negative_prompt", in.NegativePrompt)
	}
	if in.ImageURL != "" {
		for _, k := range family.ImageDropKeys {
			payload.Delete(k)
		}
	}
	for _, kv := range family.StaticPayload {
		k := kv[0].(string)
		if _, ok := payload.Get(k); !ok {
			payload.Set(k, kv[1])
		}
	}
	return payload
}

// Video field and its URL from a FAL result, the URL empty when absent
func falVideoURLFromResult(result any) (any, string) {
	m, _ := result.(map[string]any)
	if m == nil {
		return nil, ""
	}
	video := m["video"]
	var u any
	switch v := video.(type) {
	case map[string]any:
		u = v["url"]
	case string:
		u = v
	}
	if !pyTruthy(u) {
		return video, ""
	}
	return video, pyrepr.Str(u)
}

var (
	videoFalClient                  *falClientModule
	videoFalClientMu                sync.Mutex
	managedFalVideoClient           falSubmitter
	managedFalVideoClientConfig     [2]string
	managedFalVideoClientConfigured bool
	managedFalVideoClientMu         sync.Mutex
)

// FAL client module loaded once for the video backend
func loadVideoFalClient() *falClientModule {
	videoFalClientMu.Lock()
	defer videoFalClientMu.Unlock()
	if videoFalClient == nil {
		videoFalClient = importFalClient()
	}
	return videoFalClient
}

// Managed route for the stored video_gen selection, nil for direct FAL, an error naming a broken selection
var resolveManagedFalVideoGateway = func(env *tools.Env) (*tools.ManagedToolGatewayConfig, error) {
	selected := tools.ReadSelection(env, "video_gen")
	if selected == tools.NousManagedProvider {
		gw, err := tools.ResolveManagedToolGateway(env, "fal-queue", nil, nil)
		if err != nil {
			return nil, err
		}
		if gw == nil {
			return nil, &imageValueError{tools.SelectionError("video_gen", tools.NousManagedProvider,
				"the Nous Tool Gateway is not available (not entitled or unreachable)")}
		}
		return gw, nil
	}
	if selected != "" {
		if !tools.FalKeyIsConfigured(env) {
			return nil, &imageValueError{tools.SelectionError("video_gen", selected, "FAL_KEY is not set")}
		}
		return nil, nil
	}
	if tools.FalKeyIsConfigured(env) {
		return nil, nil
	}
	return tools.ResolveManagedToolGateway(env, "fal-queue", nil, nil)
}

// Whether the selected or autodetected FAL backend is reachable, an error for a stored but broken selection
var falVideoAvailable = func(env *tools.Env) (bool, error) {
	gw, err := resolveManagedFalVideoGateway(env)
	if err != nil {
		return false, err
	}
	return gw != nil || tools.FalKeyIsConfigured(env), nil
}

// Managed FAL client reused while the gateway origin and token stay the same
func getManagedFalVideoClient(gw *tools.ManagedToolGatewayConfig) (falSubmitter, error) {
	cfg := [2]string{strings.TrimRight(gw.GatewayOrigin, "/"), gw.NousUserToken}
	managedFalVideoClientMu.Lock()
	defer managedFalVideoClientMu.Unlock()
	if !managedFalVideoClientConfigured || managedFalVideoClientConfig != cfg {
		c, err := newManagedFalSyncClient(loadVideoFalClient(), gw.NousUserToken, gw.GatewayOrigin)
		if err != nil {
			return nil, err
		}
		managedFalVideoClient, managedFalVideoClientConfig, managedFalVideoClientConfigured = c, cfg, true
	}
	return managedFalVideoClient, nil
}

// Submits through direct credentials or the managed queue gateway, translating gateway 4xx rejections
var submitFalVideoRequest = func(ctx context.Context, env *tools.Env, endpoint string, arguments map[string]any) (*falRequestHandle, error) {
	mod := loadVideoFalClient()
	headers := map[string]string{"x-idempotency-key": uuid4()}
	gw, err := resolveManagedFalVideoGateway(env)
	if err != nil {
		return nil, err
	}
	if gw == nil {
		return mod.Submit(ctx, env, endpoint, arguments, headers)
	}
	h, err := submitManagedFalWithRateLimitRetry(ctx, func(hdr map[string]string) (*falRequestHandle, error) {
		c, cerr := getManagedFalVideoClient(gw)
		if cerr != nil {
			return nil, cerr
		}
		return c.Submit(ctx, endpoint, arguments, falSubmitOptions{Headers: hdr})
	}, "video endpoint", endpoint)
	if err == nil {
		return h, nil
	}
	status, ok := extractHTTPStatus(err)
	var rl *falRateLimitError
	if errors.As(err, &rl) || !ok || status < 400 || status >= 500 {
		return nil, err
	}
	if billing := managedFalBillingError(err, "endpoint"); billing != "" {
		return nil, &imageValueError{fmt.Sprintf("Nous Subscription gateway rejected endpoint '%s' (HTTP %d): %s", endpoint, status, billing)}
	}
	return nil, &imageValueError{fmt.Sprintf("Nous Subscription gateway rejected endpoint '%s' (HTTP %d). This model may not yet be enabled "+
		"on the Nous Portal's FAL proxy. Either:\n  • Set FAL_KEY in your environment to use FAL.ai directly, or\n"+
		"  • Pick a different model via `icebath tools` → Video Generation.", endpoint, status)}
}

const (
	falUpscalerEndpoint = "fal-ai/seedvr/upscale/video"
	falUpscalerFactor   = 2
)

// SeedVR2 upscale of a finished video, the new URL or empty when the pass fails
var falUpscaleVideo = func(ctx context.Context, env *tools.Env, videoURL, sourceRequestID string) string {
	result, err := func() (any, error) {
		videoGenLog.Info(fmt.Sprintf("Upscaling video with SeedVR2 (%dx)...", falUpscalerFactor))
		arguments := map[string]any{"video_url": videoURL, "upscale_mode": "factor", "upscale_factor": falUpscalerFactor}
		gw, err := resolveManagedFalVideoGateway(env)
		if err != nil {
			return nil, err
		}
		if gw != nil {
			if sourceRequestID == "" {
				return nil, errors.New("Managed SeedVR upscale requires the source FAL request id")
			}
			arguments["source_request_id"] = sourceRequestID
		}
		h, err := submitFalVideoRequest(ctx, env, falUpscalerEndpoint, arguments)
		if err != nil {
			return nil, err
		}
		return waitFalQueueResult(ctx, h, falQueuePollSeconds)
	}()
	if err != nil {
		videoGenLog.Warn(fmt.Sprintf("Video upscale failed: %v", err))
		return ""
	}
	_, u := falVideoURLFromResult(result)
	if u == "" {
		videoGenLog.Warn("Video upscaler returned no URL")
	}
	return u
}

const falNoBackendMsg = "No FAL backend available. Either set FAL_KEY (run `icebath tools` → Video Generation → FAL to configure) " +
	"or sign in to Nous (`icebath setup`) for managed gateway access."

var falModalityMissingMsg = map[string]string{
	"image": "FAL family %s has no image-to-video endpoint. Pick a family with image-to-video support via `icebath tools` → Video Generation.",
	"text":  "FAL family %s has no text-to-video endpoint. Pass an image_url to use its image-to-video endpoint, or pick a different family.",
}

func videoFalError(errText, errorType, prompt, model, aspect string) map[string]any {
	return videoErrorResponse(errText, errorType, "fal", model, prompt, aspect)
}

// FAL multi family backend routing text and image to video on image_url presence
type falVideoGenProvider struct {
	provider.CatalogProviderDefaults
}

func newFalVideoGenProvider() *falVideoGenProvider {
	p := &falVideoGenProvider{}
	p.Self = p
	return p
}

func (p *falVideoGenProvider) Name() string        { return "fal" }
func (p *falVideoGenProvider) DisplayName() string { return "FAL" }

// Unavailable rather than broken when a stored selection cannot run
func (p *falVideoGenProvider) IsAvailable() bool {
	ok, err := falVideoAvailable(nil)
	return err == nil && ok
}

func (p *falVideoGenProvider) ListModels() []map[string]any {
	rows := make([]map[string]any, 0, len(falFamilyOrder))
	for _, fid := range falFamilyOrder {
		meta := falFamilies[fid]
		row := map[string]any{"id": fid, "display": meta.Display, "speed": meta.Speed, "strengths": meta.Strengths,
			"price": meta.Price, "tier": meta.Tier, "modalities": falModalities(meta)}
		if len(meta.Durations) > 0 {
			row["min_duration"], row["max_duration"] = min(meta.Durations[0], meta.Durations[1]), max(meta.Durations[0], meta.Durations[1])
		}
		rows = append(rows, row)
	}
	return rows
}

func (p *falVideoGenProvider) DefaultModel() string { return falVideoDefaultModel }

func (p *falVideoGenProvider) GetSetupSchema() map[string]any {
	return map[string]any{"name": "FAL", "badge": "paid", "env_vars": []any{map[string]any{"key": "FAL_KEY", "prompt": "FAL.ai API key", "url": "https://fal.ai/dashboard/keys"}},
		"tag": "LTX 2.3/2.5, Pixverse, Seedance 2.0/2.5/Mini, Veo 3.1, MiniMax H3, FLUX 3, Kling 3.0/4K/O3, Wan 3.0, Happy Horse, Grok Imagine, " +
			"Gemini Omni — text-to-video & image-to-video"}
}

// Surface of the resolved family so the schema gates params on the selected model, the cross family union otherwise
func (p *falVideoGenProvider) Capabilities() map[string]any {
	_, family, err := falResolveFamily(context.Background(), nil, "")
	if err == nil && family != nil {
		durations := family.Durations
		if len(durations) == 0 {
			durations = []int{1, 1}
		}
		modalities := falModalities(*family)
		if len(modalities) == 0 {
			modalities = []any{"text"}
		}
		return map[string]any{"modalities": modalities, "aspect_ratios": stringsToAny(family.AspectRatios),
			"resolutions": stringsToAny(family.Resolutions), "max_duration": max(durations[0], durations[1]), "min_duration": min(durations[0], durations[1]),
			"supports_audio": family.Audio, "audio_always_on": family.AudioNative, "supports_negative_prompt": family.Negative,
			"supports_seed": family.Seed, "supports_upscale": true, "max_reference_images": 0}
	}
	maxD, minD, found := 1, 0, false
	for _, fid := range falFamilyOrder {
		d := falFamilies[fid].Durations
		if len(d) == 0 {
			continue
		}
		maxD = max(maxD, d[0], d[1])
		if !found || min(d[0], d[1]) < minD {
			minD = min(d[0], d[1])
		}
		found = true
	}
	if !found {
		minD = 1
	}
	return map[string]any{"modalities": []any{"text", "image"}, "aspect_ratios": []any{"16:9", "9:16", "1:1"},
		"resolutions": []any{"360p", "540p", "720p", "1080p"}, "max_duration": maxD, "min_duration": minD,
		"supports_audio": true, "supports_negative_prompt": true, "supports_seed": true, "supports_upscale": true, "max_reference_images": 0}
}

// Generates through the family endpoint for the modality, with the optional SeedVR2 pass falling back to native
func (p *falVideoGenProvider) Generate(ctx context.Context, env *tools.Env, req VideoGenRequest) (map[string]any, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ok, err := falVideoAvailable(env)
	if err != nil {
		var ve *imageValueError
		if !errors.As(err, &ve) {
			return nil, err
		}
		return videoFalError(err.Error(), "auth_required", req.Prompt, "", ""), nil
	}
	if !ok {
		return videoFalError(falNoBackendMsg, "auth_required", req.Prompt, "", ""), nil
	}
	if loadVideoFalClient() == nil {
		return videoFalError("fal_client Python package not installed (pip install fal-client)", "missing_dependency", req.Prompt, "", ""), nil
	}
	prompt := strings.TrimSpace(req.Prompt)
	familyID, family, err := falResolveFamily(envContext(ctx, env), env, req.Model)
	if err != nil {
		return nil, err
	}
	imageURL := strings.TrimSpace(req.ImageURL)
	modality, endpoint := "text", family.TextEndpoint
	if imageURL != "" {
		modality, endpoint = "image", family.ImageEndpoint
	}
	if endpoint == "" {
		return videoFalError(fmt.Sprintf(falModalityMissingMsg[modality], familyID), "modality_unsupported", prompt, familyID, ""), nil
	}
	if prompt == "" {
		return videoFalError("prompt is required.", "missing_prompt", prompt, familyID, ""), nil
	}
	aspect := req.aspect()
	payload := falBuildVideoPayload(*family, falPayloadInput{Prompt: prompt, ImageURL: imageURL, Duration: req.Duration, AspectRatio: aspect,
		Resolution: req.resolution(), NegativePrompt: req.NegativePrompt, Audio: req.Audio, Seed: req.Seed})
	arguments, _ := pyjson.Plain(payload).(map[string]any)
	video, videoURL, sourceRequestID, err := func() (any, string, string, error) {
		h, err := submitFalVideoRequest(ctx, env, endpoint, arguments)
		if err != nil {
			return nil, "", "", err
		}
		result, err := waitFalQueueResult(ctx, h, falQueuePollSeconds)
		if err != nil {
			return nil, "", "", err
		}
		v, u := falVideoURLFromResult(result)
		return v, u, h.RequestID, nil
	}()
	if err != nil {
		videoGenLog.Warn(fmt.Sprintf("FAL video gen failed (family=%s, endpoint=%s): %v", familyID, endpoint, err))
		return videoFalError(fmt.Sprintf("FAL video generation failed: %v", err), "api_error", prompt, familyID, aspect), nil
	}
	if videoURL == "" {
		return videoFalError("FAL returned no video URL in response", "empty_response", prompt, familyID, ""), nil
	}
	upscale := pyTruthy(req.Kwargs["upscale"])
	upscaledURL := ""
	if upscale {
		upscaledURL = falUpscaleVideo(ctx, env, videoURL, sourceRequestID)
	}
	upscaled := upscaledURL != ""
	if upscale && !upscaled {
		videoGenLog.Warn("Video upscale pass failed — returning native-resolution video")
	}
	if upscaled {
		videoURL = upscaledURL
	}
	extra := map[string]any{"endpoint": endpoint, "upscaled": upscaled}
	if upscaled {
		extra["upscale_factor"] = falUpscalerFactor
	}
	if vm, ok := video.(map[string]any); ok {
		keys := []string{"file_size", "content_type"}
		if upscaled {
			keys = []string{"content_type"}
		}
		for _, k := range keys {
			if pyTruthy(vm[k]) {
				extra[k] = vm[k]
			}
		}
	}
	aspectOut := ""
	if _, ok := payload.Get("aspect_ratio"); ok {
		aspectOut = aspect
	}
	duration := 0
	if d, ok := payload.Get("duration"); ok {
		var digits strings.Builder
		for _, r := range pyrepr.Str(d) {
			if unicode.IsDigit(r) {
				digits.WriteRune(r)
			}
		}
		if n, err := strconv.Atoi(digits.String()); err == nil {
			duration = n
		}
	}
	return videoSuccessResponse(videoURL, familyID, prompt, modality, aspectOut, duration, "fal", extra), nil
}

func init() {
	plugins.RegisterBundled(videoGenBackendPlugin{name: "fal", author: "NousResearch", requiresEnv: []string{"FAL_KEY"},
		description: "FAL.ai video generation backend. Multi-model — Veo 3.1, Kling, Pixverse — covering text-to-video and image-to-video via fal_client's queue API.",
		provider:    func() VideoGenProvider { return newFalVideoGenProvider() }})
}
