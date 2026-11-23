package media

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	orImageProvider       = "openrouter"
	orImageLabel          = "OpenRouter"
	orImageDefaultModel   = "openai/gpt-5.4-image-2"
	orImageFallbackModel  = "google/gemini-3-pro-image"
	orImageChatMaxRefs    = 3
	orImageRequestTimeout = 300 * time.Second
	orImageConnectTimeout = 20 * time.Second
	orImageCatalogTTL     = 900 * time.Second
	orImageCatalogTimeout = 30 * time.Second
	orImageLiveTTL        = 300 * time.Second
	orImageLiveTimeout    = 10 * time.Second
	orImageSaveTimeout    = 60 * time.Second
	orImageSaveMaxBytes   = 25 << 20
)

// Chat completions image models of the quality first default chain with their picker text
var orImageChatModels = imageModelCatalog{
	order: []string{orImageDefaultModel, orImageFallbackModel},
	meta: map[string]map[string]any{
		orImageDefaultModel:  {"display": "OpenAI GPT-5.4 Image 2", "strengths": "Highest fidelity; best prompt adherence; slower on OpenRouter"},
		orImageFallbackModel: {"display": "Gemini 3 Pro Image", "strengths": "Fast, reliable fallback with good layout adherence"},
	},
}

// Knobs one Image API model declares, an empty list meaning the model has no such knob
type orImageAPIModel struct {
	display, strengths                                           string
	aspectRatios, resolutions, quality, background, outputFormat []string
	compression, seed                                            bool
	maxN, maxRefs                                                int
}

// Allowed values of an enum knob of the model
func (m orImageAPIModel) enum(name string) []string {
	switch name {
	case "resolution":
		return m.resolutions
	case "quality":
		return m.quality
	case "background":
		return m.background
	case "output_format":
		return m.outputFormat
	}
	return nil
}

var (
	orGeminiRatios  = []string{"1:1", "1:4", "1:8", "2:3", "3:2", "3:4", "4:1", "4:3", "4:5", "5:4", "8:1", "9:16", "16:9", "21:9"}
	orMAIRatios     = []string{"1:1", "4:3", "3:4", "16:9", "9:16", "3:2", "2:3", "auto"}
	orKreaRatios    = []string{"1:1", "4:3", "3:2", "16:9", "4:5", "2:3", "9:16"}
	orOpenAIQuality = []string{"auto", "low", "medium", "high"}
)

// Curated Image API models in picker order, an id missing here still routes by the live catalog
var orImageAPIOrder = []string{"google/gemini-3.1-flash-lite-image", "google/gemini-3.1-flash-image", "openai/gpt-image-2",
	"openai/gpt-image-1-mini", "microsoft/mai-image-2.5", "microsoft/mai-image-2.5-pro", "x-ai/grok-imagine-image-2.0",
	"krea/krea-2-medium", "krea/krea-2-medium-turbo", "qwen/qwen-image-3-pro"}

var orImageAPIModels = map[string]orImageAPIModel{
	"google/gemini-3.1-flash-lite-image": {display: "Nano Banana 2 Lite (Gemini 3.1 Flash Lite Image)",
		strengths: "Cheap and fast; 14 exact aspect ratios; 14 reference images", aspectRatios: orGeminiRatios,
		resolutions: []string{"1K"}, maxN: 1, maxRefs: 14},
	"google/gemini-3.1-flash-image": {display: "Nano Banana 2 (Gemini 3.1 Flash Image)",
		strengths: "Same ratios as Lite plus resolution control (512/1K/2K/4K)", aspectRatios: orGeminiRatios,
		resolutions: []string{"512", "1K", "2K", "4K"}, maxN: 1, maxRefs: 14},
	"openai/gpt-image-2": {display: "OpenAI GPT Image 2",
		strengths:    "Best editing fidelity; up to 16 references; strongest prompt adherence",
		aspectRatios: []string{"1:1", "3:2", "2:3", "4:3", "3:4", "16:9", "9:16", "21:9", "auto"},
		quality:      orOpenAIQuality, background: []string{"auto", "opaque"}, compression: true, maxN: 10, maxRefs: 16},
	"openai/gpt-image-1-mini": {display: "OpenAI GPT Image 1 Mini",
		strengths:    "The only model here with background=transparent (cut-out PNG)",
		aspectRatios: []string{"1:1", "3:2", "2:3", "auto"}, quality: orOpenAIQuality,
		background: []string{"auto", "transparent", "opaque"}, compression: true, maxN: 10, maxRefs: 16},
	"microsoft/mai-image-2.5": {display: "Microsoft MAI-Image-2.5", strengths: "Standard ratios; a good second opinion next to Gemini",
		aspectRatios: orMAIRatios, maxN: 1, maxRefs: 1},
	"microsoft/mai-image-2.5-pro": {display: "Microsoft MAI-Image-2.5 Pro", strengths: "Reach for it when gpt-image-2 misses the brief",
		aspectRatios: orMAIRatios, maxN: 1, maxRefs: 1},
	"x-ai/grok-imagine-image-2.0": {display: "Grok Imagine Image 2.0",
		strengths:    "Photoreal; widest exotic-ratio set (9:19.5, 20:9, 2:1 …); 1K/2K; low/medium quality",
		aspectRatios: []string{"1:1", "3:4", "4:3", "9:16", "16:9", "2:3", "3:2", "9:19.5", "19.5:9", "9:20", "20:9", "1:2", "2:1", "auto"},
		resolutions:  []string{"1K", "2K"}, quality: []string{"low", "medium"}, maxN: 1, maxRefs: 3},
	"krea/krea-2-medium": {display: "Krea 2 Medium", strengths: "Realistic, expressive styles; deterministic via seed",
		aspectRatios: orKreaRatios, resolutions: []string{"1K"}, seed: true, maxN: 1, maxRefs: 1},
	"krea/krea-2-medium-turbo": {display: "Krea 2 Medium Turbo", strengths: "Cheapest here — bulk content, cards, thumbnails; seed support",
		aspectRatios: orKreaRatios, resolutions: []string{"1K"}, seed: true, maxN: 1, maxRefs: 1},
	"qwen/qwen-image-3-pro": {display: "Qwen Image 3 Pro", strengths: "Precise small text and detail rendering; n up to 6; 1K/2K; seed",
		aspectRatios: []string{"1:1", "1:2", "1:4", "2:1", "2:3", "3:2", "3:4", "4:1", "4:3", "4:5", "5:4", "9:16", "16:9"},
		resolutions:  []string{"1K", "2K"}, seed: true, maxN: 6, maxRefs: 4},
}

// Permissive knobs of a catalog model the curated table does not describe, its aspect ratio enum unknown
var orUnknownImageAPIModel = orImageAPIModel{maxN: 1, maxRefs: 16}

// Union of exact ratios across the endpoint, checking an override for a model with an unknown enum
var orEndpointAspectRatios = map[string]bool{"1:1": true, "1:2": true, "1:4": true, "1:8": true, "2:1": true, "2:3": true, "3:2": true,
	"3:4": true, "4:1": true, "4:3": true, "4:5": true, "5:4": true, "8:1": true, "9:16": true, "16:9": true, "9:19.5": true,
	"19.5:9": true, "9:20": true, "20:9": true, "9:21": true, "21:9": true, "auto": true}

// Exact ratios per semantic aspect, best first
var orAspectPreferences = map[string][]string{
	"landscape": {"16:9", "3:2", "4:3", "5:4", "21:9", "2:1", "19.5:9", "20:9", "4:1", "8:1"},
	"portrait":  {"9:16", "2:3", "3:4", "4:5", "9:21", "1:2", "9:19.5", "9:20", "1:4", "1:8"},
	"square":    {"1:1"},
}

var orChatAspectRatios = map[string]string{"square": "1:1", "landscape": "16:9", "portrait": "9:16"}

var orMediaTypeExtensions = map[string]string{"image/png": "png", "image/jpeg": "jpg", "image/jpg": "jpg", "image/webp": "webp",
	"image/gif": "gif", "image/svg+xml": "svg"}

// Image API statuses worth trying on the next chain model, account rejections answered before these
var orImageAPIFallbackStatuses = map[int]bool{402: true, 404: true, 408: true, 409: true, 425: true, 429: true, 500: true, 502: true,
	503: true, 504: true}

// Default chain models that stay on chat completions whatever the image catalog says
var orChatOnlyModels = map[string]bool{orImageDefaultModel: true, orImageFallbackModel: true}

// Payload knobs echoed into the success envelope
var orImageAPIExtraKeys = []string{"resolution", "quality", "background", "output_format", "seed"}

// Setting of how many images one Image API call returns, sent as the payload field n
const orImageCountSetting = "count"

// Curated knobs of an Image API model, permissive defaults when unknown
func orImageAPIModelMeta(modelID string) orImageAPIModel {
	if m, ok := orImageAPIModels[modelID]; ok {
		return m
	}
	return orUnknownImageAPIModel
}

// Knob value from the call, then image_gen.openrouter, blanks counting as unset
func orImageSetting(env *tools.Env, name string, explicit any) any {
	if s, isStr := explicit.(string); explicit != nil && (!isStr || strings.TrimSpace(s) != "") {
		return explicit
	}
	v := loadImageGenConfig(env, orImageProvider)[name]
	if s, ok := v.(string); ok {
		if s = strings.TrimSpace(s); s == "" {
			return nil
		}
		return s
	}
	return v
}

// Integer of a number or numeric string, false for booleans and anything else
func orCoerceInt(v any) (int, bool) {
	switch x := v.(type) {
	case nil, bool:
		return 0, false
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(x))
		return n, err == nil
	}
	f, ok := config.AsFloat(v)
	if !ok || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	return int(f), true
}

// Finite seconds of a number or numeric string, false for booleans and anything else
func orSeconds(v any) (float64, bool) {
	var f float64
	switch x := v.(type) {
	case nil, bool:
		return 0, false
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		if err != nil {
			return 0, false
		}
		f = parsed
	default:
		parsed, ok := config.AsFloat(v)
		if !ok {
			return 0, false
		}
		f = parsed
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	return f, true
}

// Exact aspect ratio for the model or empty to omit it, a forced supported value winning and every downgrade noted
func orPickAspectRatio(semantic string, meta orImageAPIModel, forced any, notes *[]string) string {
	supported := meta.aspectRatios
	if s, ok := forced.(string); ok && strings.TrimSpace(s) != "" {
		value := strings.TrimSpace(s)
		if (len(supported) > 0 && slices.Contains(supported, value)) || (len(supported) == 0 && orEndpointAspectRatios[value]) {
			return value
		}
		*notes = append(*notes, fmt.Sprintf("requested aspect_ratio '%s' is unsupported by this model; used the '%s' mapping instead", value, semantic))
	}
	if len(supported) == 0 {
		*notes = append(*notes, fmt.Sprintf("model is not in this backend's catalog, so its aspect_ratio enum is unknown; the field was "+
			"omitted and '%s' was not applied", semantic))
		return ""
	}
	for _, candidate := range orAspectPreferences[semantic] {
		if slices.Contains(supported, candidate) {
			return candidate
		}
	}
	if slices.Contains(supported, "auto") {
		return "auto"
	}
	return supported[0]
}

// Enum knob value the model accepts, empty with a note when the model lacks the knob or the value
func orImageEnum(env *tools.Env, name string, explicit any, allowed []string, notes *[]string) string {
	value, ok := orImageSetting(env, name, explicit).(string)
	if !ok || strings.TrimSpace(value) == "" {
		return ""
	}
	value = strings.TrimSpace(value)
	if len(allowed) == 0 {
		*notes = append(*notes, fmt.Sprintf("'%s' is not supported by this model; dropped", name))
		return ""
	}
	if !slices.Contains(allowed, value) {
		*notes = append(*notes, fmt.Sprintf("'%s=%s' is not valid for this model (accepts %s); dropped", name, value, strings.Join(allowed, ", ")))
		return ""
	}
	return value
}

// Image API request body with every knob filtered against what the model declares, plus the notes of what was dropped
func orBuildImageAPIPayload(env *tools.Env, modelID, prompt, semantic string, refs []string, kwargs map[string]any) (*pyjson.Object, []string) {
	meta := orImageAPIModelMeta(modelID)
	var notes []string
	payload := pyjson.ObjectOf("model", modelID, "prompt", prompt)
	if ratio := orPickAspectRatio(semantic, meta, orImageSetting(env, "aspect_ratio", kwargs["aspect_ratio_exact"]), &notes); ratio != "" {
		payload.Set("aspect_ratio", ratio)
	}
	for _, name := range []string{"resolution", "quality", "background", "output_format"} {
		if value := orImageEnum(env, name, kwargs[name], meta.enum(name), &notes); value != "" {
			payload.Set(name, value)
		}
	}
	ints := []struct {
		name  string
		gated bool
		clamp func(int) int
	}{
		{"output_compression", meta.compression, func(v int) int { return max(0, min(100, v)) }},
		{"seed", meta.seed, func(v int) int { return v }},
	}
	for _, knob := range ints {
		value, ok := orCoerceInt(orImageSetting(env, knob.name, kwargs[knob.name]))
		if !ok {
			continue
		}
		if knob.gated {
			payload.Set(knob.name, knob.clamp(value))
		} else {
			notes = append(notes, fmt.Sprintf("'%s' is not supported by this model; dropped", knob.name))
		}
	}
	if count, ok := orCoerceInt(orImageSetting(env, orImageCountSetting, kwargs[orImageCountSetting])); ok && count > 1 {
		maxN := max(meta.maxN, 1)
		if count > maxN {
			notes = append(notes, fmt.Sprintf("'%s=%d' exceeds this model's cap of %d; clamped", orImageCountSetting, count, maxN))
		}
		payload.Set("n", max(1, min(count, maxN)))
	}
	if len(refs) > 0 {
		usable := refs[:min(len(refs), meta.maxRefs)]
		if len(refs) > len(usable) {
			notes = append(notes, fmt.Sprintf("%d reference image(s) supplied but this model accepts %d; extras dropped", len(refs), meta.maxRefs))
		}
		if len(usable) > 0 {
			parts := make([]any, len(usable))
			for i, u := range usable {
				parts[i] = pyjson.ObjectOf("type", "image_url", "image_url", pyjson.ObjectOf("url", u))
			}
			payload.Set("input_references", parts)
		}
	}
	return payload, notes
}

// First n characters of the JSON text of a value
func orClippedJSON(v any, n int) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return clipRunes(string(raw), n)
}

// One line error from an error message, a validation issue list, or the body text
func orImageAPIErrorMessage(status int, body []byte) string {
	fallback := strings.TrimSpace(fmt.Sprintf("%d %s", status, http.StatusText(status)))
	var parsed any
	if err := json.Unmarshal(body, &parsed); err != nil {
		if text := clipRunes(string(body), 300); text != "" {
			return text
		}
		return fallback
	}
	obj, isObj := parsed.(map[string]any)
	var errNode any
	if isObj {
		errNode = obj["error"]
	}
	if s, ok := errNode.(string); ok && strings.TrimSpace(s) != "" {
		return strings.TrimSpace(s)
	}
	errObj, ok := errNode.(map[string]any)
	if !ok {
		if pyTruthy(parsed) {
			return orClippedJSON(parsed, 300)
		}
		return fallback
	}
	message, isStr := errObj["message"].(string)
	if errObj["name"] == "ZodError" && isStr {
		var issues any
		if err := json.Unmarshal([]byte(message), &issues); err != nil {
			return clipRunes(message, 300)
		}
		list, _ := issues.([]any)
		var parts []string
		for _, item := range list {
			issue, ok := item.(map[string]any)
			if !ok {
				continue
			}
			var path []string
			for _, p := range listAt(issue, "path") {
				path = append(path, fmt.Sprint(p))
			}
			where := strings.Join(path, ".")
			if where == "" {
				where = "request"
			}
			what := strAt(issue, "message")
			if what == "" {
				what = "invalid"
			}
			parts = append(parts, where+": "+what)
		}
		if joined := clipRunes(strings.Join(parts, "; "), 400); joined != "" {
			return joined
		}
		return clipRunes(message, 300)
	}
	if isStr && strings.TrimSpace(message) != "" {
		return clipRunes(strings.TrimSpace(message), 300)
	}
	return orClippedJSON(errObj, 300)
}

// File extension of a media type, the fallback for an unknown one
func orExtensionFor(mediaType any, fallback string) string {
	s, ok := mediaType.(string)
	if !ok {
		return fallback
	}
	base, _, _ := strings.Cut(s, ";")
	if ext, ok := orMediaTypeExtensions[strings.ToLower(strings.TrimSpace(base))]; ok {
		return ext
	}
	return fallback
}

// Filename safe fragment of a model id for the cache prefix
func orModelSlug(modelID string) string {
	var b strings.Builder
	for _, r := range modelID {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

// Pet references, the edit source, then the reference URLs, each once in first seen order
func orCollectReferences(req ImageGenRequest) []string {
	var out []string
	seen := map[string]bool{}
	add := func(s string) {
		if s = strings.TrimSpace(s); s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, ref := range normalizeReferenceImages(req.Kwargs["reference_images"]) {
		add(ref)
	}
	add(req.ImageURL)
	for _, ref := range req.ReferenceImageURLs {
		add(ref)
	}
	return out
}

// URLs as given, local files inlined as data URIs, unreadable files listed apart, a blocked read failing
func orLoadReferences(env *tools.Env, refs []string) (parts, unreadable []string, err error) {
	for _, ref := range refs {
		if hasAnyPrefix(ref, []string{"http://", "https://", "data:"}) {
			parts = append(parts, ref)
			continue
		}
		if blocked := tools.ReadBlockError(env, ref); blocked != "" {
			return nil, nil, &imageValueError{blocked}
		}
		raw, readErr := os.ReadFile(pypath.Expanduser(ref))
		if readErr != nil {
			pkgLog().Debug(fmt.Sprintf("could not read reference image %s: %v", ref, readErr))
			unreadable = append(unreadable, ref)
			continue
		}
		mediaType, _, _ := strings.Cut(mime.TypeByExtension(strings.ToLower(filepath.Ext(ref))), ";")
		if mediaType = strings.TrimSpace(mediaType); mediaType == "" {
			mediaType = "image/png"
		}
		parts = append(parts, "data:"+mediaType+";base64,"+base64.StdEncoding.EncodeToString(raw))
	}
	return parts, unreadable, nil
}

// Model chain from the call, image_gen.openrouter.model, image_gen.model, else the default chain
func orResolveModelChain(env *tools.Env, explicit any) []string {
	scoped := loadImageGenConfig(env, orImageProvider)
	top := loadImageGenConfig(env, "")
	for _, candidate := range []any{explicit, scoped["model"], top["model"]} {
		if s, ok := candidate.(string); ok && strings.TrimSpace(s) != "" {
			return []string{strings.TrimSpace(s)}
		}
	}
	return []string{orImageDefaultModel, orImageFallbackModel}
}

// API key and base URL the runtime resolver gives OpenRouter for the home, pooled and profile scoped keys included
func orImageCredentials(ctx context.Context, h string) (string, string, error) {
	rt, err := provider.ResolveRuntimeProvider(ctx, h, orImageProvider, "", "", "")
	if err != nil {
		return "", "", err
	}
	base := strings.TrimSpace(rt.BaseURL)
	if base == "" {
		base = openRouterVideoDefaultBaseURL
	}
	return strings.TrimSpace(rt.APIKey), strings.TrimRight(base, "/"), nil
}

func orImageHeaders(apiKey string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + apiKey, "Content-Type": "application/json",
		"HTTP-Referer": "https://github.com/nickheyer/icebath", "X-Title": "Icebath Agent"}
}

// One catalog listing entry with a non blank id
type orCatalogEntry struct {
	id    string
	entry map[string]any
}

// Entries of a catalog listing, an error on any transport or HTTP failure
func orGetCatalog(ctx context.Context, baseURL, path, apiKey string, timeout time.Duration) ([]orCatalogEntry, error) {
	headers := map[string]string{}
	if apiKey != "" {
		headers["Authorization"] = "Bearer " + apiKey
	}
	body, _, err := getJSON(ctx, baseURL+path, headers, timeout)
	if err != nil {
		return nil, err
	}
	var out []orCatalogEntry
	for _, item := range listAt(body, "data") {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if id := stripAt(entry, "id"); id != "" {
			out = append(out, orCatalogEntry{id: id, entry: entry})
		}
	}
	return out, nil
}

// Catalog cache key of an endpoint and the fingerprint of the key that read it
type orCatalogKey struct{ base, key, path string }

type orCatalogIDs struct {
	at  time.Time
	ids map[string]bool
}

var (
	orImageAPIIDsMu sync.Mutex
	orImageAPIIDs   = map[orCatalogKey]orCatalogIDs{}
)

// Image API catalog ids cached per endpoint and key, empty on any failure so chat stays the route
func orImageAPICatalogIDs(ctx context.Context, baseURL, apiKey string) map[string]bool {
	key := orCatalogKey{base: baseURL, key: provider.FingerprintSecretValue(apiKey), path: "/images/models"}
	orImageAPIIDsMu.Lock()
	cached, ok := orImageAPIIDs[key]
	orImageAPIIDsMu.Unlock()
	if ok && time.Since(cached.at) < orImageCatalogTTL {
		return cached.ids
	}
	ids := map[string]bool{}
	entries, err := orGetCatalog(ctx, baseURL, "/images/models", apiKey, orImageCatalogTimeout)
	if err != nil {
		pkgLog().Debug(fmt.Sprintf("image API catalog probe failed for %s: %v", baseURL, err))
	}
	for _, e := range entries {
		ids[e.id] = true
	}
	orImageAPIIDsMu.Lock()
	orImageAPIIDs[key] = orCatalogIDs{at: time.Now(), ids: ids}
	orImageAPIIDsMu.Unlock()
	return ids
}

// images or chat for a model, a forced surface first, curated ids offline, unknown ids by the cached live catalog
func orSelectSurface(ctx context.Context, env *tools.Env, modelID, baseURL, apiKey string) string {
	if modelID == "" {
		return "chat"
	}
	if forced, ok := orImageSetting(env, "surface", nil).(string); ok {
		if f := strings.ToLower(strings.TrimSpace(forced)); f == "images" || f == "chat" {
			return f
		}
	}
	if orChatOnlyModels[modelID] {
		return "chat"
	}
	if _, curated := orImageAPIModels[modelID]; curated || orImageAPICatalogIDs(ctx, baseURL, apiKey)[modelID] {
		return "images"
	}
	return "chat"
}

// Hint for an access gated OpenAI image model whose key is valid, empty otherwise
func orAccessErrorHint(modelID string, status int, message string) string {
	if !strings.HasPrefix(modelID, "openai/") {
		return ""
	}
	low := strings.ToLower(message)
	gated := status == 402 || status == 403 || status == 404
	for _, marker := range []string{"no endpoints", "no allowed", "not a valid model", "data policy"} {
		gated = gated || strings.Contains(low, marker)
	}
	if !gated {
		return ""
	}
	return fmt.Sprintf("%s can't reach image model '%s' (%d) — enable OpenAI image access in your %s account, or choose %s as the image model in /settings.",
		orImageLabel, modelID, status, orImageLabel, orImageFallbackModel)
}

// Cost and token totals the provider reported for the call
func orUsageExtra(extra map[string]any, body any) {
	usage := dictAt(body, "usage")
	if cost, ok := numAt(usage, "cost"); ok {
		extra["cost_usd"] = cost
	}
	switch total := usage["total_tokens"].(type) {
	case float64:
		if total == math.Trunc(total) {
			extra["total_tokens"] = int64(total)
		}
	case int, int64:
		extra["total_tokens"] = total
	}
}

// Generated image URLs of a chat completions answer
func orChatImages(body any) []string {
	var out []string
	for _, choice := range listAt(body, "choices") {
		for _, image := range listAt(dictAt(choice, "message"), "images") {
			if u := stripAt(dictAt(image, "image_url"), "url"); u != "" {
				out = append(out, u)
			}
		}
	}
	return out
}

// Saved path of one Image API data entry of the endpoint at baseURL, empty when it carries neither b64_json nor url
func orSaveImageAPIEntry(ctx context.Context, env *tools.Env, entry map[string]any, prefix, baseURL string) (string, error) {
	if b64 := stripAt(entry, "b64_json"); b64 != "" {
		return saveB64Image(env, b64, prefix, orExtensionFor(entry["media_type"], "png"))
	}
	if u := stripAt(entry, "url"); u != "" {
		return saveURLImage(ctx, env, u, prefix, orImageSaveTimeout, orImageSaveMaxBytes, baseURL)
	}
	return "", nil
}

// OpenRouter image backend over chat completions image output and the dedicated Image API
type openRouterImageGenProvider struct {
	env  func(context.Context) *tools.Env
	mu   sync.Mutex
	rows map[orCatalogKey]orRowsEntry
}

type orRowsEntry struct {
	at   time.Time
	rows []map[string]any
}

func newOpenRouterImageGenProvider() *openRouterImageGenProvider {
	return &openRouterImageGenProvider{env: pluginEnv, rows: map[orCatalogKey]orRowsEntry{}}
}

func (p *openRouterImageGenProvider) Name() string         { return orImageProvider }
func (p *openRouterImageGenProvider) DisplayName() string  { return orImageLabel }
func (p *openRouterImageGenProvider) DefaultModel() string { return orImageDefaultModel }

func (p *openRouterImageGenProvider) GetSetupSchema() map[string]any {
	return apiKeySetupSchema(apiKeySetup{Name: "OpenRouter (image)", Badge: "paid",
		Tag: "Gemini Flash Image, gpt-image-2, Krea 2, Qwen Image 3 & more via OpenRouter; uses OPENROUTER_API_KEY",
		Key: "OPENROUTER_API_KEY", Prompt: "OpenRouter API key", URL: "https://openrouter.ai/keys"})
}

func (p *openRouterImageGenProvider) IsAvailable() bool { return p.AvailableIn(context.Background()) }

// Available when the runtime resolver finds an OpenRouter key in the profile home the context binds
func (p *openRouterImageGenProvider) AvailableIn(ctx context.Context) bool {
	key, _, err := orImageCredentials(ctx, home.Dir(ctx))
	if err != nil {
		pkgLog().Debug(fmt.Sprintf("openrouter runtime resolution failed: %v", err))
		return false
	}
	return key != ""
}

// Text and image modalities with the reference cap of the model that serves the next call
func (p *openRouterImageGenProvider) Capabilities() map[string]any {
	maxRefs := orImageChatMaxRefs
	if m, ok := orImageAPIModels[orResolveModelChain(p.env(context.Background()), nil)[0]]; ok && m.maxRefs > 0 {
		maxRefs = m.maxRefs
	}
	return map[string]any{"modalities": []any{"text", "image"}, "max_reference_images": maxRefs}
}

// Picker rows of a live catalog cached per endpoint and key, empty when unreachable
func (p *openRouterImageGenProvider) liveRows(ctx context.Context, path string) []map[string]any {
	apiKey, baseURL, err := orImageCredentials(ctx, home.Dir(ctx))
	if err != nil || baseURL == "" {
		pkgLog().Debug(fmt.Sprintf("openrouter live %s unavailable: %v", path, err))
		return nil
	}
	key := orCatalogKey{base: baseURL, key: provider.FingerprintSecretValue(apiKey), path: path}
	p.mu.Lock()
	cached, ok := p.rows[key]
	p.mu.Unlock()
	if ok && time.Since(cached.at) < orImageLiveTTL {
		return cached.rows
	}
	imageAPI := path == "/images/models"
	generic := "Image-output model (from live OpenRouter catalog)"
	if imageAPI {
		generic = "Image API model (from live OpenRouter catalog)"
	}
	var rows []map[string]any
	entries, err := orGetCatalog(ctx, baseURL, path, apiKey, orImageLiveTimeout)
	if err != nil {
		pkgLog().Debug(fmt.Sprintf("openrouter live %s unavailable: %v", path, err))
	}
	for _, e := range entries {
		arch := dictAt(e.entry, "architecture")
		if !imageAPI && (strings.HasPrefix(e.id, "openrouter/auto") || !slices.Contains(stringList(arch["output_modalities"]), "image")) {
			continue
		}
		display := stripAt(e.entry, "name")
		if display == "" {
			display = e.id
		}
		strengths := generic
		if imageAPI {
			if m, ok := orImageAPIModels[e.id]; ok {
				display, strengths = m.display, m.strengths
			}
		} else if m, ok := orImageChatModels.get(e.id); ok {
			display, strengths = m["display"].(string), m["strengths"].(string)
		}
		inputs := listAt(arch, "input_modalities")
		if inputs == nil {
			inputs = []any{}
		}
		rows = append(rows, map[string]any{"id": e.id, "display": display, "strengths": strengths, "input_modalities": inputs})
	}
	p.mu.Lock()
	p.rows[key] = orRowsEntry{at: time.Now(), rows: rows}
	p.mu.Unlock()
	return rows
}

// Image API and chat image models of the live catalogs merged, the default chain and curated table when offline
func (p *openRouterImageGenProvider) ListModels() []map[string]any {
	ctx := context.Background()
	merged := map[string]map[string]any{}
	for _, path := range []string{"/images/models", "/models"} {
		for _, row := range p.liveRows(ctx, path) {
			id := row["id"].(string)
			if _, seen := merged[id]; !seen {
				merged[id] = row
			}
		}
	}
	if len(merged) > 0 {
		rows := make([]map[string]any, 0, len(merged))
		for _, row := range merged {
			rows = append(rows, row)
		}
		priority := func(id string) int {
			switch id {
			case orImageDefaultModel:
				return 0
			case orImageFallbackModel:
				return 1
			}
			return 2
		}
		sort.Slice(rows, func(i, j int) bool {
			a, b := rows[i]["id"].(string), rows[j]["id"].(string)
			if pa, pb := priority(a), priority(b); pa != pb {
				return pa < pb
			}
			return a < b
		})
		return rows
	}
	rows := catalogRows(orImageChatModels, []string{"display", "strengths"}, nil)
	for _, id := range orImageAPIOrder {
		m := orImageAPIModels[id]
		rows = append(rows, map[string]any{"id": id, "display": m.display, "strengths": m.strengths + " (Image API)"})
	}
	return rows
}

// One request of the call, its model, prompt, aspect, loaded references and endpoint
type orImageAttempt struct {
	modelID, prompt, aspect string
	parts, unreadable       []string
	baseURL                 string
	headers                 map[string]string
	kwargs                  map[string]any
}

// One images/generations attempt and whether the chain may go on to the next model
func (p *openRouterImageGenProvider) viaImageAPI(ctx context.Context, env *tools.Env, a orImageAttempt) (map[string]any, bool) {
	fail := errorFactory(orImageProvider, a.aspect, a.modelID, a.prompt)
	payload, notes := orBuildImageAPIPayload(env, a.modelID, a.prompt, a.aspect, a.parts, a.kwargs)
	if len(a.unreadable) > 0 {
		notes = append([]string{"dropped unreadable reference image(s): " + strings.Join(a.unreadable, ", ")}, notes...)
	}
	timeout := orImageRequestTimeout
	if configured := orImageSetting(env, "timeout", a.kwargs["timeout"]); configured != nil {
		if seconds, ok := orSeconds(configured); ok {
			timeout = time.Duration(max(1.0, seconds) * float64(time.Second))
		} else {
			pkgLog().Debug(fmt.Sprintf("openrouter: ignoring non-numeric image API timeout %v", configured))
		}
	}
	body, failure, err := postJSON(ctx, a.baseURL+"/images/generations", a.headers, payload,
		requestsTimeout{Connect: min(orImageConnectTimeout, timeout), Read: timeout}, orImageLabel, orImageAPIErrorMessage, true)
	if err != nil {
		return fail(fmt.Sprintf("%s request failed: %v", orImageLabel, err), "api_error"), false
	}
	if failure != nil {
		if failure.Kind != "http" {
			return fail(failure.Error, failure.ErrorType), failure.Kind == "timeout"
		}
		status, message := failure.Status, failure.Message
		pkgLog().Error(fmt.Sprintf("openrouter image API generation failed (%d) on %s: %s", status, a.modelID, message))
		switch {
		case status == 401 || status == 403:
			return fail(fmt.Sprintf("%s rejected the API key (%d): %s", orImageLabel, status, message), "auth_error"), false
		case status == 404:
			return fail(fmt.Sprintf("Model '%s' does not exist on the OpenRouter Image API (its catalog is separate from chat-completions — "+
				"check GET %s/images/models).", a.modelID, a.baseURL), "model_access"), true
		}
		return fail(failure.Error, "api_error"), orImageAPIFallbackStatuses[status]
	}
	plain := pyjson.Plain(body)
	recordTokenUsage(ctx, dictAt(plain, "usage"), a.modelID, orImageProvider, a.baseURL)
	var entries []map[string]any
	for _, item := range listAt(plain, "data") {
		if entry, ok := item.(map[string]any); ok {
			entries = append(entries, entry)
		}
	}
	if len(entries) == 0 {
		return fail(fmt.Sprintf("%s returned no image data for '%s'.", orImageLabel, a.modelID), "empty_response"), true
	}
	prefix := orImageProvider + "_" + orModelSlug(a.modelID)
	var saved []string
	for _, entry := range entries {
		path, err := orSaveImageAPIEntry(ctx, env, entry, prefix, a.baseURL)
		if err != nil {
			return fail(fmt.Sprintf("Could not save generated image: %v", err), "io_error"), false
		}
		if path != "" {
			saved = append(saved, path)
		}
	}
	if len(saved) == 0 {
		return fail(orImageLabel+" response carried neither b64_json nor url.", "empty_response"), false
	}
	ratio, _ := payload.Get("aspect_ratio")
	extra := map[string]any{"endpoint": "images/generations", "exact_aspect_ratio": ratio}
	for _, key := range orImageAPIExtraKeys {
		if v, ok := payload.Get(key); ok {
			extra[key] = v
		}
	}
	if v, ok := payload.Get("n"); ok {
		extra[orImageCountSetting] = v
	}
	if len(saved) > 1 {
		extra["additional_images"] = saved[1:]
	}
	modality := "text"
	if len(a.parts) > 0 {
		modality = "image"
		used, _ := payload.Get("input_references")
		refs, _ := used.([]any)
		extra["reference_images_used"] = len(refs)
	}
	if len(notes) > 0 {
		extra["notes"] = notes
	}
	orUsageExtra(extra, plain)
	return imageSuccessResponse(saved[0], a.modelID, a.prompt, a.aspect, orImageProvider, modality, extra), false
}

// One chat completions attempt and why the chain may try the fallback model, empty when it may not
func (p *openRouterImageGenProvider) viaChat(ctx context.Context, env *tools.Env, a orImageAttempt) (map[string]any, string) {
	fail := errorFactory(orImageProvider, a.aspect, a.modelID, a.prompt)
	var notes []string
	if len(a.unreadable) > 0 {
		notes = append(notes, "dropped unreadable reference image(s): "+strings.Join(a.unreadable, ", "))
	}
	usable := a.parts
	if len(usable) > orImageChatMaxRefs {
		notes = append(notes, fmt.Sprintf("%d reference image(s) supplied but this model accepts %d; extras dropped", len(usable), orImageChatMaxRefs))
		usable = usable[:orImageChatMaxRefs]
	}
	content := []any{pyjson.ObjectOf("type", "text", "text", a.prompt)}
	for _, u := range usable {
		content = append(content, pyjson.ObjectOf("type", "image_url", "image_url", pyjson.ObjectOf("url", u)))
	}
	ratio, ok := orChatAspectRatios[a.aspect]
	if !ok {
		ratio = "1:1"
	}
	payload := pyjson.ObjectOf("model", a.modelID, "modalities", []any{"image", "text"},
		"messages", []any{pyjson.ObjectOf("role", "user", "content", content)}, "image_config", pyjson.ObjectOf("aspect_ratio", ratio))
	body, failure, err := postJSON(ctx, a.baseURL+"/chat/completions", a.headers, payload,
		requestsTimeout{Connect: orImageRequestTimeout, Read: orImageRequestTimeout}, orImageLabel, nil, true)
	if err != nil {
		return fail(fmt.Sprintf("%s request failed: %v", orImageLabel, err), "api_error"), ""
	}
	if failure != nil {
		if failure.Kind != "http" {
			if failure.Kind == "timeout" {
				return fail(failure.Error, failure.ErrorType), "timed out"
			}
			return fail(failure.Error, failure.ErrorType), ""
		}
		pkgLog().Error(fmt.Sprintf("openrouter image gen failed (%d) on %s: %s", failure.Status, a.modelID, failure.Message))
		if hint := orAccessErrorHint(a.modelID, failure.Status, failure.Message); hint != "" {
			return fail(hint, "model_access"), "unavailable"
		}
		return fail(failure.Error, "api_error"), ""
	}
	plain := pyjson.Plain(body)
	recordTokenUsage(ctx, dictAt(plain, "usage"), a.modelID, orImageProvider, a.baseURL)
	images := orChatImages(plain)
	if len(images) == 0 {
		return fail(fmt.Sprintf("%s returned no image. Ensure the model '%s' supports image output.", orImageLabel, a.modelID), "empty_response"),
			"returned no image"
	}
	prefix := orImageProvider + "_gen"
	var path string
	if first := images[0]; strings.HasPrefix(first, "data:") {
		header, b64, _ := strings.Cut(first, ",")
		if strings.TrimSpace(b64) == "" {
			return fail(fmt.Sprintf("%s returned an empty image data URI for '%s'.", orImageLabel, a.modelID), "empty_response"), "returned no image"
		}
		path, err = saveB64Image(env, b64, prefix, orExtensionFor(strings.TrimPrefix(header, "data:"), "png"))
	} else {
		path, err = saveURLImage(ctx, env, first, prefix, orImageSaveTimeout, orImageSaveMaxBytes, a.baseURL)
	}
	if err != nil {
		return fail(fmt.Sprintf("Could not save generated image: %v", err), "io_error"), ""
	}
	extra := map[string]any{"endpoint": "chat/completions", "exact_aspect_ratio": ratio}
	modality := "text"
	if len(usable) > 0 {
		modality = "image"
		extra["reference_images_used"] = len(usable)
	}
	if len(notes) > 0 {
		extra["notes"] = notes
	}
	orUsageExtra(extra, plain)
	return imageSuccessResponse(path, a.modelID, a.prompt, a.aspect, orImageProvider, modality, extra), ""
}

// Text to image or reference grounded generation along the model chain, each model on the surface it lives on
func (p *openRouterImageGenProvider) Generate(ctx context.Context, env *tools.Env, req ImageGenRequest) (map[string]any, error) {
	if env == nil {
		env = p.env(ctx)
	}
	ctx = envContext(ctx, env)
	rawAspect := req.AspectRatio
	if rawAspect == "" {
		rawAspect = defaultImageAspectRatio
	}
	apiKey, baseURL, err := orImageCredentials(ctx, homeOf(env))
	if err != nil {
		return errorFactory(orImageProvider, rawAspect, "", "")(fmt.Sprintf("Could not resolve %s credentials: %s", orImageLabel,
			provider.FormatRuntimeProviderError(err)), "missing_api_key"), nil
	}
	if apiKey == "" || baseURL == "" {
		return errorFactory(orImageProvider, rawAspect, "", "")(fmt.Sprintf("No %s credentials found. Configure %s in `icebath tools` → "+
			"Image Generation.", orImageLabel, orImageLabel), "missing_api_key"), nil
	}
	chain := orResolveModelChain(env, req.Kwargs["model"])
	aspect := resolveAspectRatio(req.AspectRatio)
	parts, unreadable, err := orLoadReferences(env, orCollectReferences(req))
	fail := errorFactory(orImageProvider, aspect, chain[0], req.Prompt)
	if err != nil {
		return fail("Could not load reference image: "+err.Error(), "io_error"), nil
	}
	if len(unreadable) > 0 && len(parts) == 0 {
		return fail("Could not read the reference image(s) requested for editing: "+strings.Join(unreadable, ", ")+
			". Refusing to silently fall back to text-to-image.", "io_error"), nil
	}
	attempt := func(modelID string) (map[string]any, string) {
		a := orImageAttempt{modelID: modelID, prompt: req.Prompt, aspect: aspect, parts: parts, unreadable: unreadable,
			baseURL: baseURL, headers: orImageHeaders(apiKey), kwargs: req.Kwargs}
		if orSelectSurface(ctx, env, modelID, baseURL, apiKey) == "images" {
			outcome, retryable := p.viaImageAPI(ctx, env, a)
			if retryable {
				return outcome, "failed on the image API"
			}
			return outcome, ""
		}
		return p.viaChat(ctx, env, a)
	}
	last := len(chain) - 1
	for i, modelID := range chain[:last] {
		outcome, reason := attempt(modelID)
		if outcome["success"] == true || reason == "" {
			return outcome, nil
		}
		pkgLog().Info(fmt.Sprintf("openrouter model %s %s; retrying with fallback %s", modelID, reason, chain[i+1]))
	}
	outcome, _ := attempt(chain[last])
	return outcome, nil
}

func init() {
	plugins.RegisterBundled(imageGenBackendPlugin{name: orImageProvider, author: "Icebath", version: "1.1.0", requiresEnv: []string{"OPENROUTER_API_KEY"},
		description: "OpenRouter image generation. Chat-completions image output (reference-grounded) plus OpenRouter's Dedicated Image API " +
			"(/images/generations) for gpt-image-2, Krea 2, Qwen Image 3 Pro, MAI-Image-2.5 and Grok Imagine — exact per-model aspect ratios, " +
			"resolution/quality/background/seed/n, up to 16 reference images. Text-to-image and image-to-image.",
		provider: func() ImageGenProvider { return newOpenRouterImageGenProvider() }})
}
