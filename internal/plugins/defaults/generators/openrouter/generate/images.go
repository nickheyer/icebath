package generate

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/generation"
	"github.com/nickheyer/icebath/internal/models"
	openrouterconfig "github.com/nickheyer/icebath/internal/plugins/defaults/generators/openrouter/config"
	"github.com/nickheyer/icebath/internal/provider"
	"github.com/nickheyer/icebath/pkg/strs"
)

const (
	// Default chain of chat models, the quality pick first
	chatDefault  = "openai/gpt-5.4-image-2"
	chatFallback = "google/gemini-3-pro-image"
	chatRefs     = 3
	requestLimit = 300 * time.Second
	connectLimit = 20 * time.Second
	idsTTL       = 15 * time.Minute
	idsLimit     = 30 * time.Second
	rowsTTL      = 5 * time.Minute
	rowsLimit    = 10 * time.Second
	label        = "OpenRouter"
)

var chatModels = []generation.Model{
	{ID: chatDefault, Title: "OpenAI GPT-5.4 Image 2", Strengths: "Highest fidelity; best prompt adherence; slower on OpenRouter"},
	{ID: chatFallback, Title: "Gemini 3 Pro Image", Strengths: "Fast, reliable fallback with good layout adherence"},
}

// Knobs an Image API model takes, an empty list meaning it has no such knob
type knobs struct {
	title, strengths                                     string
	aspects, resolutions, quality, background, fileTypes []string
	compression, seed                                    bool
	maxN, maxRefs                                        int
}

func (k knobs) enum(name string) []string {
	switch name {
	case "resolution":
		return k.resolutions
	case "quality":
		return k.quality
	case "background":
		return k.background
	case "output_format":
		return k.fileTypes
	}
	return nil
}

var (
	geminiRatios  = []string{"1:1", "1:4", "1:8", "2:3", "3:2", "3:4", "4:1", "4:3", "4:5", "5:4", "8:1", "9:16", "16:9", "21:9"}
	maiRatios     = []string{"1:1", "4:3", "3:4", "16:9", "9:16", "3:2", "2:3", "auto"}
	kreaRatios    = []string{"1:1", "4:3", "3:2", "16:9", "4:5", "2:3", "9:16"}
	openAIQuality = []string{"auto", "low", "medium", "high"}
)

// Image API models in picker order; one missing here still goes there when the live catalog lists it
var imageAPIOrder = []string{"google/gemini-3.1-flash-lite-image", "google/gemini-3.1-flash-image", "openai/gpt-image-2", "openai/gpt-image-1-mini",
	"microsoft/mai-image-2.5", "microsoft/mai-image-2.5-pro", "x-ai/grok-imagine-image-2.0", "krea/krea-2-medium", "krea/krea-2-medium-turbo", "qwen/qwen-image-3-pro"}

var imageAPI = map[string]knobs{
	"google/gemini-3.1-flash-lite-image": {title: "Nano Banana 2 Lite (Gemini 3.1 Flash Lite Image)", strengths: "Cheap and fast; 14 exact aspect ratios; 14 reference images",
		aspects: geminiRatios, resolutions: []string{"1K"}, maxN: 1, maxRefs: 14},
	"google/gemini-3.1-flash-image": {title: "Nano Banana 2 (Gemini 3.1 Flash Image)", strengths: "Same ratios as Lite plus resolution control (512/1K/2K/4K)",
		aspects: geminiRatios, resolutions: []string{"512", "1K", "2K", "4K"}, maxN: 1, maxRefs: 14},
	"openai/gpt-image-2": {title: "OpenAI GPT Image 2", strengths: "Best editing fidelity; up to 16 references; strongest prompt adherence",
		aspects: []string{"1:1", "3:2", "2:3", "4:3", "3:4", "16:9", "9:16", "21:9", "auto"}, quality: openAIQuality, background: []string{"auto", "opaque"},
		compression: true, maxN: 10, maxRefs: 16},
	"openai/gpt-image-1-mini": {title: "OpenAI GPT Image 1 Mini", strengths: "The only model here with background=transparent (cut-out PNG)",
		aspects: []string{"1:1", "3:2", "2:3", "auto"}, quality: openAIQuality, background: []string{"auto", "transparent", "opaque"}, compression: true, maxN: 10, maxRefs: 16},
	"microsoft/mai-image-2.5":     {title: "Microsoft MAI-Image-2.5", strengths: "Standard ratios; a good second opinion next to Gemini", aspects: maiRatios, maxN: 1, maxRefs: 1},
	"microsoft/mai-image-2.5-pro": {title: "Microsoft MAI-Image-2.5 Pro", strengths: "Reach for it when gpt-image-2 misses the brief", aspects: maiRatios, maxN: 1, maxRefs: 1},
	"x-ai/grok-imagine-image-2.0": {title: "Grok Imagine Image 2.0", strengths: "Photoreal; widest exotic-ratio set (9:19.5, 20:9, 2:1 …); 1K/2K; low/medium quality",
		aspects:     []string{"1:1", "3:4", "4:3", "9:16", "16:9", "2:3", "3:2", "9:19.5", "19.5:9", "9:20", "20:9", "1:2", "2:1", "auto"},
		resolutions: []string{"1K", "2K"}, quality: []string{"low", "medium"}, maxN: 1, maxRefs: 3},
	"krea/krea-2-medium": {title: "Krea 2 Medium", strengths: "Realistic, expressive styles; deterministic via seed", aspects: kreaRatios, resolutions: []string{"1K"},
		seed: true, maxN: 1, maxRefs: 1},
	"krea/krea-2-medium-turbo": {title: "Krea 2 Medium Turbo", strengths: "Cheapest here — bulk content, cards, thumbnails; seed support", aspects: kreaRatios,
		resolutions: []string{"1K"}, seed: true, maxN: 1, maxRefs: 1},
	"qwen/qwen-image-3-pro": {title: "Qwen Image 3 Pro", strengths: "Precise small text and detail rendering; n up to 6; 1K/2K; seed",
		aspects:     []string{"1:1", "1:2", "1:4", "2:1", "2:3", "3:2", "3:4", "4:1", "4:3", "4:5", "5:4", "9:16", "16:9"},
		resolutions: []string{"1K", "2K"}, seed: true, maxN: 6, maxRefs: 4},
}

// Knobs of a model the curated table doesn't describe: its aspect ratios unknown
var unknownKnobs = knobs{maxN: 1, maxRefs: 16}

// Every exact ratio the Image API takes, for an override on a model whose own are unknown
var endpointRatios = strs.Set("1:1", "1:2", "1:4", "1:8", "2:1", "2:3", "3:2", "3:4", "4:1", "4:3", "4:5", "5:4", "8:1", "9:16", "16:9", "9:19.5", "19.5:9",
	"9:20", "20:9", "9:21", "21:9", "auto")

// Exact ratios for each shape, best first
var preferences = map[string][]string{
	"landscape": {"16:9", "3:2", "4:3", "5:4", "21:9", "2:1", "19.5:9", "20:9", "4:1", "8:1"},
	"portrait":  {"9:16", "2:3", "3:4", "4:5", "9:21", "1:2", "9:19.5", "9:20", "1:4", "1:8"},
	"square":    {"1:1"},
}

var chatRatios = map[string]string{"square": "1:1", "landscape": "16:9", "portrait": "9:16"}

var extensions = map[string]string{"image/png": "png", "image/jpeg": "jpg", "image/jpg": "jpg", "image/webp": "webp", "image/gif": "gif", "image/svg+xml": "svg"}

// Image API statuses worth trying the next model of the chain on, account refusals answered before these
var fallbackStatuses = map[int]bool{402: true, 404: true, 408: true, 409: true, 425: true, 429: true, 500: true, 502: true, 503: true, 504: true}

// Models of the default chain that stay on chat whatever the image catalog says
var chatOnly = strs.Set(chatDefault, chatFallback)

func knobsOf(model string) knobs {
	if k, ok := imageAPI[model]; ok {
		return k
	}
	return unknownKnobs
}

// Text knob from the call, else its setting, blank counting as unset
func text(cfg *config.Config, key string) string {
	return strings.TrimSpace(cfg.GetString(key))
}

// Whole knob of a setting, false when it is left to the model
func whole(cfg *config.Config, key string) (int, bool) {
	value := cfg.GetInt(key)
	return value, value != openrouterconfig.Unset
}

// Exact ratio for the model: a supported one the settings force, else the best of the shape it takes; empty to leave it out, each
// downgrade noted
func ratio(shape string, k knobs, forced string, notes *[]string) string {
	if forced != "" {
		if len(k.aspects) > 0 && slices.Contains(k.aspects, forced) || len(k.aspects) == 0 && endpointRatios[forced] {
			return forced
		}
		*notes = append(*notes, fmt.Sprintf("requested aspect_ratio '%s' is unsupported by this model; used the '%s' mapping instead", forced, shape))
	}
	if len(k.aspects) == 0 {
		*notes = append(*notes, fmt.Sprintf("model is not in this backend's catalog, so its aspect_ratio enum is unknown; the field was omitted and '%s' was not applied", shape))
		return ""
	}
	for _, candidate := range preferences[shape] {
		if slices.Contains(k.aspects, candidate) {
			return candidate
		}
	}
	if slices.Contains(k.aspects, "auto") {
		return "auto"
	}
	return k.aspects[0]
}

// Value of an enum knob the model takes, empty with a note when it lacks the knob or the value
func enum(value, name string, allowed []string, notes *[]string) string {
	if value == "" {
		return ""
	}
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

// Image API request with every knob checked against what the model declares, and notes of what was dropped
func imagePayload(cfg *config.Config, model, prompt, shape string, refs []string) (map[string]any, []string) {
	k := knobsOf(model)
	var notes []string
	payload := map[string]any{"model": model, "prompt": prompt}
	if r := ratio(shape, k, text(cfg, openrouterconfig.AspectRatio), &notes); r != "" {
		payload["aspect_ratio"] = r
	}
	for name, key := range map[string]string{"resolution": openrouterconfig.Resolution, "quality": openrouterconfig.Quality, "background": openrouterconfig.Background,
		"output_format": openrouterconfig.OutputFormat} {
		if value := enum(text(cfg, key), name, k.enum(name), &notes); value != "" {
			payload[name] = value
		}
	}
	if value, ok := whole(cfg, openrouterconfig.OutputCompression); ok {
		if k.compression {
			payload["output_compression"] = max(0, min(100, value))
		} else {
			notes = append(notes, "'output_compression' is not supported by this model; dropped")
		}
	}
	if value, ok := whole(cfg, openrouterconfig.Seed); ok {
		if k.seed {
			payload["seed"] = value
		} else {
			notes = append(notes, "'seed' is not supported by this model; dropped")
		}
	}
	if count := cfg.GetInt(openrouterconfig.Count); count > 1 {
		most := max(k.maxN, 1)
		if count > most {
			notes = append(notes, fmt.Sprintf("'count=%d' exceeds this model's cap of %d; clamped", count, most))
		}
		payload["n"] = max(1, min(count, most))
	}
	if len(refs) > 0 {
		usable := refs[:min(len(refs), k.maxRefs)]
		if len(refs) > len(usable) {
			notes = append(notes, fmt.Sprintf("%d reference image(s) supplied but this model accepts %d; extras dropped", len(refs), k.maxRefs))
		}
		var parts []any
		for _, u := range usable {
			parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": u}})
		}
		if len(parts) > 0 {
			payload["input_references"] = parts
		}
	}
	return payload, notes
}

// One line of a failed Image API reply: its error message, the issues of a validation error, else the head of the body
func imageAPIMessage(status int, body []byte) string {
	fallback := strings.TrimSpace(fmt.Sprintf("%d %s", status, http.StatusText(status)))
	var parsed any
	if json.Unmarshal(body, &parsed) != nil {
		return strs.Filled(strs.Head(string(body), 300), fallback)
	}
	node := generation.Object(parsed)["error"]
	if text, ok := node.(string); ok && strings.TrimSpace(text) != "" {
		return strings.TrimSpace(text)
	}
	object, ok := node.(map[string]any)
	if !ok {
		if parsed != nil {
			compact, _ := json.Marshal(parsed)
			return strs.Head(string(compact), 300)
		}
		return fallback
	}
	message, _ := object["message"].(string)
	if object["name"] == "ZodError" && message != "" {
		var issues []map[string]any
		if json.Unmarshal([]byte(message), &issues) != nil {
			return strs.Head(message, 300)
		}
		var parts []string
		for _, issue := range issues {
			var path []string
			for _, p := range generation.List(issue, "path") {
				path = append(path, fmt.Sprint(p))
			}
			parts = append(parts, strs.Filled(strings.Join(path, "."), "request")+": "+strs.Filled(generation.Text(issue, "message"), "invalid"))
		}
		return strs.Filled(strs.Head(strings.Join(parts, "; "), 400), strs.Head(message, 300))
	}
	if strings.TrimSpace(message) != "" {
		return strs.Head(strings.TrimSpace(message), 300)
	}
	compact, _ := json.Marshal(object)
	return strs.Head(string(compact), 300)
}

// Extension of a media type, the fallback for an unknown one
func extension(kind, fallback string) string {
	base, _, _ := strings.Cut(kind, ";")
	if ext, ok := extensions[strings.ToLower(strings.TrimSpace(base))]; ok {
		return ext
	}
	return fallback
}

// Model id as a file name part
func slug(model string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_' {
			return r
		}
		return '_'
	}, model)
}

// References as OpenRouter takes them: URLs as they are, files of this machine as data URLs, those that can't be read listed apart; a
// refused read fails
func references(job *generation.Job, request generation.ImageRequest) (parts, unreadable []string, err error) {
	for _, ref := range strs.Unique(request.Sources(-1)) {
		if generation.Remote(ref) {
			parts = append(parts, ref)
			continue
		}
		if refusal := generation.Unreadable(job.Config, ref); refusal != "" {
			return nil, nil, &generation.Refusal{Text: refusal}
		}
		data, readErr := job.Local(ref)
		if readErr != nil {
			slog.Debug(fmt.Sprintf("could not read reference image %s: %v", ref, readErr))
			unreadable = append(unreadable, ref)
			continue
		}
		parts = append(parts, generation.DataURL(strs.Filled(generation.TypeOf(ref), "image/png"), data))
	}
	return parts, unreadable, nil
}

// Chain of models the call tries: the one image_gen.openrouter.model names, else Image model, else the default chain
func chain(cfg *config.Config, asked string) []string {
	if model := strs.Filled(cfg.GetString(openrouterconfig.Model), asked); model != "" {
		return []string{model}
	}
	return []string{chatDefault, chatFallback}
}

func requestHeaders(key string) map[string]string {
	headers := map[string]string{"Authorization": "Bearer " + key, "Content-Type": "application/json"}
	for name, value := range provider.Attribution() {
		headers[name] = value
	}
	return headers
}

// Ids of the Image API catalog for the address and key, read again after fifteen minutes; none when it can't be read, so chat stays the
// way
func (s *Services) imageIDs(ctx context.Context, route provider.Route) map[string]bool {
	key := route.BaseURL + "#" + models.Identity(route.APIKey) + "#ids"
	s.mu.Lock()
	held, ok := s.ids[key]
	s.mu.Unlock()
	if ok && time.Since(held.at) < idsTTL {
		return held.ids
	}
	ids := map[string]bool{}
	reply, err := generation.Get(ctx, route.BaseURL+"/images/models", map[string]string{"Authorization": "Bearer " + route.APIKey}, idsLimit)
	if err != nil {
		slog.Debug(fmt.Sprintf("image API catalog probe failed for %s: %v", route.BaseURL, err))
	}
	for _, entry := range generation.List(reply, "data") {
		if id := generation.Text(entry, "id"); id != "" {
			ids[id] = true
		}
	}
	s.mu.Lock()
	s.ids[key] = idSet{at: time.Now(), ids: ids}
	s.mu.Unlock()
	return ids
}

// Endpoint a model goes to: the one the settings force, chat for the default chain, the Image API for a model it lists, chat otherwise
func (s *Services) surface(ctx context.Context, cfg *config.Config, model string, route provider.Route) string {
	if forced := strings.ToLower(text(cfg, openrouterconfig.Surface)); forced == "images" || forced == "chat" {
		return forced
	}
	if model == "" || chatOnly[model] {
		return "chat"
	}
	if _, curated := imageAPI[model]; curated || s.imageIDs(ctx, route)[model] {
		return "images"
	}
	return "chat"
}

// Hint for an OpenAI image model the account can't reach though its key is good, empty otherwise
func accessHint(model string, status int, message string) string {
	if !strings.HasPrefix(model, "openai/") {
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
	return fmt.Sprintf("%s can't reach image model '%s' (%d) — enable OpenAI image access in your %s account, or choose %s as the image model with /imagegen.",
		label, model, status, label, chatFallback)
}

// Cost and token count the reply reports
func spent(extra map[string]any, reply any) {
	usage := generation.Object(generation.Object(reply)["usage"])
	if cost, ok := usage["cost"].(float64); ok {
		extra["cost_usd"] = cost
	}
	if total, ok := usage["total_tokens"].(float64); ok && total == float64(int64(total)) {
		extra["total_tokens"] = int64(total)
	}
}

// One try of a model of the chain
type attempt struct {
	model, prompt, shape string
	parts, unreadable    []string
	route                provider.Route
}

// Draws through images/generations, and whether the chain may go on to its next model
func (s *Services) viaImages(ctx context.Context, job *generation.Job, a attempt) (generation.Result, bool) {
	cfg := job.Config
	call := generation.Call{Provider: openrouterconfig.Name, Model: a.model, Prompt: a.prompt, Aspect: a.shape}
	payload, notes := imagePayload(cfg, a.model, a.prompt, a.shape, a.parts)
	if len(a.unreadable) > 0 {
		notes = append([]string{"dropped unreadable reference image(s): " + strings.Join(a.unreadable, ", ")}, notes...)
	}
	limit := time.Duration(max(cfg.GetInt(openrouterconfig.Timeout), 1)) * time.Second
	reply, failure := generation.Post(ctx, a.route.BaseURL+"/images/generations", requestHeaders(a.route.APIKey), payload, min(connectLimit, limit), limit, label,
		imageAPIMessage)
	if failure != nil {
		if failure.Kind != "http" {
			return call.Fail(failure.Error, failure.Type), failure.Kind == "timeout"
		}
		slog.Error(fmt.Sprintf("openrouter image API generation failed (%d) on %s: %s", failure.Status, a.model, failure.Message))
		switch failure.Status {
		case 401, 403:
			return call.Fail(fmt.Sprintf("%s rejected the API key (%d): %s", label, failure.Status, failure.Message), "auth_error"), false
		case 404:
			return call.Fail(fmt.Sprintf("Model '%s' does not exist on the OpenRouter Image API (its catalog is separate from chat-completions — check GET %s/images/models).",
				a.model, a.route.BaseURL), "model_access"), true
		}
		return call.Fail(failure.Error, "api_error"), fallbackStatuses[failure.Status]
	}
	route := a.route
	route.Model = a.model
	job.Record(route, generation.Object(reply)["usage"])
	entries := generation.List(reply, "data")
	if len(entries) == 0 {
		return call.Fail(fmt.Sprintf("%s returned no image data for '%s'.", label, a.model), "empty_response"), true
	}
	prefix := openrouterconfig.Name + "_" + slug(a.model)
	var saved []string
	for _, entry := range entries {
		var path string
		var err error
		switch {
		case generation.Text(entry, "b64_json") != "":
			path, err = job.Unpack(generation.Image, generation.Text(entry, "b64_json"), prefix, extension(generation.Text(entry, "media_type"), "png"))
		case generation.Text(entry, "url") != "":
			path, err = job.Download(ctx, generation.Image, generation.Text(entry, "url"), generation.Fetch{Prefix: prefix, Origin: a.route.BaseURL})
		}
		if err != nil {
			return call.Fail(fmt.Sprintf("Could not save generated image: %v", err), "io_error"), false
		}
		if path != "" {
			saved = append(saved, path)
		}
	}
	if len(saved) == 0 {
		return call.Fail(label+" response carried neither b64_json nor url.", "empty_response"), false
	}
	extra := map[string]any{"endpoint": "images/generations", "exact_aspect_ratio": payload["aspect_ratio"]}
	for _, key := range []string{"resolution", "quality", "background", "output_format", "seed"} {
		if value, ok := payload[key]; ok {
			extra[key] = value
		}
	}
	if n, ok := payload["n"]; ok {
		extra["count"] = n
	}
	if len(saved) > 1 {
		extra["additional_images"] = saved[1:]
	}
	modality := "text"
	if len(a.parts) > 0 {
		modality = "image"
		used, _ := payload["input_references"].([]any)
		extra["reference_images_used"] = len(used)
	}
	if len(notes) > 0 {
		extra["notes"] = notes
	}
	spent(extra, reply)
	return call.Made(saved[0], modality, extra), false
}

// Draws through a chat request that answers with a picture, and why the chain may try its next model, empty when it may not
func (s *Services) viaChat(ctx context.Context, job *generation.Job, a attempt) (generation.Result, string) {
	call := generation.Call{Provider: openrouterconfig.Name, Model: a.model, Prompt: a.prompt, Aspect: a.shape}
	var notes []string
	if len(a.unreadable) > 0 {
		notes = append(notes, "dropped unreadable reference image(s): "+strings.Join(a.unreadable, ", "))
	}
	usable := a.parts
	if len(usable) > chatRefs {
		notes = append(notes, fmt.Sprintf("%d reference image(s) supplied but this model accepts %d; extras dropped", len(usable), chatRefs))
		usable = usable[:chatRefs]
	}
	content := []any{map[string]any{"type": "text", "text": a.prompt}}
	for _, u := range usable {
		content = append(content, map[string]any{"type": "image_url", "image_url": map[string]any{"url": u}})
	}
	r, ok := chatRatios[a.shape]
	if !ok {
		r = "1:1"
	}
	payload := map[string]any{"model": a.model, "modalities": []any{"image", "text"}, "messages": []any{map[string]any{"role": "user", "content": content}},
		"image_config": map[string]any{"aspect_ratio": r}}
	reply, failure := generation.Post(ctx, a.route.BaseURL+"/chat/completions", requestHeaders(a.route.APIKey), payload, requestLimit, requestLimit, label, nil)
	if failure != nil {
		switch {
		case failure.Kind == "timeout":
			return call.Fail(failure.Error, failure.Type), "timed out"
		case failure.Kind != "http":
			return call.Fail(failure.Error, failure.Type), ""
		}
		slog.Error(fmt.Sprintf("openrouter image gen failed (%d) on %s: %s", failure.Status, a.model, failure.Message))
		if hint := accessHint(a.model, failure.Status, failure.Message); hint != "" {
			return call.Fail(hint, "model_access"), "unavailable"
		}
		return call.Fail(failure.Error, "api_error"), ""
	}
	route := a.route
	route.Model = a.model
	job.Record(route, generation.Object(reply)["usage"])
	var images []string
	for _, choice := range generation.List(reply, "choices") {
		for _, image := range generation.List(generation.Object(choice)["message"], "images") {
			if u := generation.Text(generation.Object(image)["image_url"], "url"); u != "" {
				images = append(images, u)
			}
		}
	}
	if len(images) == 0 {
		return call.Fail(fmt.Sprintf("%s returned no image. Ensure the model '%s' supports image output.", label, a.model), "empty_response"), "returned no image"
	}
	prefix := openrouterconfig.Name + "_gen"
	var path string
	var err error
	if first := images[0]; strings.HasPrefix(first, "data:") {
		header, b64, _ := strings.Cut(first, ",")
		if strings.TrimSpace(b64) == "" {
			return call.Fail(fmt.Sprintf("%s returned an empty image data URI for '%s'.", label, a.model), "empty_response"), "returned no image"
		}
		kind, _, _ := strings.Cut(strings.TrimPrefix(header, "data:"), ";")
		path, err = job.Unpack(generation.Image, b64, prefix, extension(kind, "png"))
	} else {
		path, err = job.Download(ctx, generation.Image, first, generation.Fetch{Prefix: prefix, Origin: a.route.BaseURL})
	}
	if err != nil {
		return call.Fail(fmt.Sprintf("Could not save generated image: %v", err), "io_error"), ""
	}
	extra := map[string]any{"endpoint": "chat/completions", "exact_aspect_ratio": r}
	modality := "text"
	if len(usable) > 0 {
		modality = "image"
		extra["reference_images_used"] = len(usable)
	}
	if len(notes) > 0 {
		extra["notes"] = notes
	}
	spent(extra, reply)
	return call.Made(path, modality, extra), ""
}

// Draws from text or with references along the model chain, each model on the endpoint it lives on
func (s *Services) draw(ctx context.Context, job *generation.Job, request generation.ImageRequest) generation.Result {
	cfg := job.Config
	route, ok := s.route(cfg)
	if !ok {
		return generation.Call{Provider: openrouterconfig.Name, Aspect: strs.Filled(request.Aspect, generation.DefaultAspect)}.Fail(
			fmt.Sprintf("No %s credentials found. Connect %s in /providers or set OPENROUTER_API_KEY.", label, label), "missing_api_key")
	}
	ladder := chain(cfg, request.Model)
	shape := generation.Aspect(request.Aspect)
	parts, unreadable, err := references(job, request)
	call := generation.Call{Provider: openrouterconfig.Name, Model: ladder[0], Prompt: request.Prompt, Aspect: shape}
	if err != nil {
		return call.Fail("Could not load reference image: "+err.Error(), "io_error")
	}
	if len(unreadable) > 0 && len(parts) == 0 {
		return call.Fail("Could not read the reference image(s) requested for editing: "+strings.Join(unreadable, ", ")+". Refusing to silently fall back to text-to-image.",
			"io_error")
	}
	try := func(model string) (generation.Result, string) {
		a := attempt{model: model, prompt: request.Prompt, shape: shape, parts: parts, unreadable: unreadable, route: route}
		if s.surface(ctx, cfg, model, route) == "images" {
			result, again := s.viaImages(ctx, job, a)
			if again {
				return result, "failed on the image API"
			}
			return result, ""
		}
		return s.viaChat(ctx, job, a)
	}
	for i, model := range ladder[:len(ladder)-1] {
		result, reason := try(model)
		if result.OK || reason == "" {
			return result
		}
		slog.Info(fmt.Sprintf("openrouter model %s %s; retrying with fallback %s", model, reason, ladder[i+1]))
	}
	result, _ := try(ladder[len(ladder)-1])
	return result
}

// Picker rows of a live catalog, cached per address, key and path for five minutes; none when it can't be read
func (s *Services) liveRows(ctx context.Context, route provider.Route, path string) []generation.Model {
	key := route.BaseURL + "#" + models.Identity(route.APIKey) + "#" + path
	s.mu.Lock()
	held, ok := s.rows[key]
	s.mu.Unlock()
	if ok && time.Since(held.at) < rowsTTL {
		return held.models
	}
	images := path == "/images/models"
	generic := "Image-output model (from live OpenRouter catalog)"
	if images {
		generic = "Image API model (from live OpenRouter catalog)"
	}
	reply, err := generation.Get(ctx, route.BaseURL+path, map[string]string{"Authorization": "Bearer " + route.APIKey}, rowsLimit)
	if err != nil {
		slog.Debug(fmt.Sprintf("openrouter live %s unavailable: %v", path, err))
	}
	var rows []generation.Model
	for _, entry := range generation.List(reply, "data") {
		id := generation.Text(entry, "id")
		if id == "" {
			continue
		}
		architecture := generation.Object(generation.Object(entry)["architecture"])
		var outputs []string
		for _, m := range generation.List(architecture, "output_modalities") {
			if t, ok := m.(string); ok {
				outputs = append(outputs, t)
			}
		}
		if !images && (strings.HasPrefix(id, "openrouter/auto") || !slices.Contains(outputs, "image")) {
			continue
		}
		row := generation.Model{ID: id, Title: strs.Filled(generation.Text(entry, "name"), id), Strengths: generic}
		if k, ok := imageAPI[id]; images && ok {
			row.Title, row.Strengths = k.title, k.strengths
		}
		if i := slices.IndexFunc(chatModels, func(m generation.Model) bool { return m.ID == id }); !images && i >= 0 {
			row.Title, row.Strengths = chatModels[i].Title, chatModels[i].Strengths
		}
		for _, m := range generation.List(architecture, "input_modalities") {
			if t, ok := m.(string); ok {
				row.Modalities = append(row.Modalities, t)
			}
		}
		rows = append(rows, row)
	}
	s.mu.Lock()
	s.rows[key] = modelRows{at: time.Now(), models: rows}
	s.mu.Unlock()
	return rows
}

// Models of both live catalogs merged, the default chain first; the default chain and the curated Image API models when offline
func (s *Services) imageModels(ctx context.Context, cfg *config.Config) []generation.Model {
	route, ok := s.route(cfg)
	merged := map[string]generation.Model{}
	if ok {
		for _, path := range []string{"/images/models", "/models"} {
			for _, row := range s.liveRows(ctx, route, path) {
				if _, seen := merged[row.ID]; !seen {
					merged[row.ID] = row
				}
			}
		}
	}
	if len(merged) > 0 {
		rows := make([]generation.Model, 0, len(merged))
		for _, row := range merged {
			rows = append(rows, row)
		}
		rank := func(id string) int {
			switch id {
			case chatDefault:
				return 0
			case chatFallback:
				return 1
			}
			return 2
		}
		sort.Slice(rows, func(i, j int) bool {
			if a, b := rank(rows[i].ID), rank(rows[j].ID); a != b {
				return a < b
			}
			return rows[i].ID < rows[j].ID
		})
		return rows
	}
	rows := slices.Clone(chatModels)
	for _, id := range imageAPIOrder {
		rows = append(rows, generation.Model{ID: id, Title: imageAPI[id].title, Strengths: imageAPI[id].strengths + " (Image API)"})
	}
	return rows
}

// What the first model of the chain takes: pictures of both kinds, as many references as it reads
func imageTakes(cfg *config.Config) generation.ImageTakes {
	most := chatRefs
	if k, ok := imageAPI[chain(cfg, generation.Named(cfg, generation.Image))[0]]; ok && k.maxRefs > 0 {
		most = k.maxRefs
	}
	return generation.ImageTakes{Edits: true, References: most}
}
