package media

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"net/http"
	"net/http/httptrace"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	openRouterVideoDefaultModel   = "minimax/hailuo-3-max"
	openRouterVideoDefaultBaseURL = "https://openrouter.ai/api/v1"
	openRouterVideoCatalogTTL     = 300 * time.Second
	openRouterVideoCatalogTimeout = 10 * time.Second
	openRouterVideoMaxReferences  = 3
)

var openRouterVideoTerminal = map[string]bool{"completed": true, "failed": true, "cancelled": true, "canceled": true, "expired": true}

var openRouterResolutionOrder = []string{"480p", "540p", "720p", "768p", "1080p", "1K", "2K", "4K"}

var openRouterResolutionHeight = map[string]int{"480p": 480, "540p": 540, "720p": 720, "768p": 768, "1080p": 1080, "1K": 1024, "2K": 1440, "4K": 2160}

var openRouterAllAspectRatios = []string{"16:9", "9:16", "1:1", "4:3", "3:4", "3:2", "2:3", "21:9", "9:21"}

// Offline snapshot so the picker and default work before the first successful catalog fetch
func openRouterFallbackCatalog() []map[string]any {
	durations := make([]any, 0, 11)
	for d := 5; d < 16; d++ {
		durations = append(durations, d)
	}
	return []map[string]any{{"id": openRouterVideoDefaultModel, "name": "MiniMax: Hailuo 3 Max", "supported_durations": durations,
		"supported_resolutions": []any{"768p", "480p"}, "supported_aspect_ratios": []any{"21:9", "16:9", "4:3", "1:1", "3:4", "9:16"},
		"supported_frame_images": []any{"first_frame", "last_frame"}, "generate_audio": false, "seed": false,
		"pricing_skus": map[string]any{"duration_seconds_480p": "0.05", "duration_seconds_768p": "0.08"}}}
}

// Compact per second price from pricing_skus, empty for token or megapixel priced shapes
func openRouterPriceLabel(skus any) string {
	m, ok := skus.(map[string]any)
	if !ok {
		return ""
	}
	var perSecond []float64
	for key, value := range m {
		amount, err := pyint.Float(value)
		if err != nil {
			continue
		}
		if strings.HasPrefix(key, "duration_seconds") {
			perSecond = append(perSecond, amount)
		} else if strings.HasPrefix(key, "cents_per_second_output") || strings.HasPrefix(key, "cents_per_video_output_second") {
			perSecond = append(perSecond, amount/100)
		}
	}
	if len(perSecond) == 0 {
		return ""
	}
	lo, hi := perSecond[0], perSecond[0]
	for _, v := range perSecond[1:] {
		lo, hi = math.Min(lo, v), math.Max(hi, v)
	}
	if lo == hi {
		return "$" + strconv.FormatFloat(lo, 'f', 2, 64) + "/s"
	}
	return "$" + strconv.FormatFloat(lo, 'f', 2, 64) + "–" + strconv.FormatFloat(hi, 'f', 2, 64) + "/s"
}

// Numeric width over height of an aspect ratio string, false when it does not parse
func openRouterRatio(value any) (float64, bool) {
	parts := strings.Split(pyrepr.Str(value), ":")
	if len(parts) != 2 {
		return 0, false
	}
	w, err1 := pyint.ParseFloat(parts[0])
	h, err2 := pyint.ParseFloat(parts[1])
	if err1 != nil || err2 != nil || h == 0 {
		return 0, false
	}
	return w / h, true
}

// Whether the value equals any supported entry under Python equality
func pyIn(value any, supported []any) bool {
	for _, s := range supported {
		if pyrepr.Equal(value, s) {
			return true
		}
	}
	return false
}

// Closest supported value, durations by distance, resolutions by pixel height and aspect ratios by ratio
func openRouterNearest(value any, supported []any, height map[string]int) any {
	if len(supported) == 0 || pyIn(value, supported) {
		return value
	}
	type scored struct {
		dist float64
		s    any
	}
	pick := func(scores []scored) any {
		if len(scores) == 0 {
			return supported[0]
		}
		best := scores[0]
		for _, sc := range scores[1:] {
			if sc.dist < best.dist {
				best = sc
			}
		}
		return best.s
	}
	if height != nil {
		target, ok := height[pyrepr.Str(value)]
		if !ok {
			return supported[0]
		}
		var scores []scored
		for _, s := range supported {
			if h, ok := height[pyrepr.Str(s)]; ok {
				scores = append(scores, scored{math.Abs(float64(h - target)), s})
			}
		}
		return pick(scores)
	}
	if strings.Contains(pyrepr.Str(value), ":") {
		target, ok := openRouterRatio(value)
		if !ok {
			return supported[0]
		}
		var scores []scored
		for _, s := range supported {
			if r, ok := openRouterRatio(s); ok {
				scores = append(scores, scored{math.Abs(r - target), s})
			}
		}
		return pick(scores)
	}
	want, err := pyint.Of(value)
	if err != nil {
		return supported[0]
	}
	var best any
	var bestDist *big.Int
	for i, s := range supported {
		n, err := pyint.Of(s)
		if err != nil {
			return supported[0]
		}
		d := new(big.Int).Abs(new(big.Int).Sub(n, want))
		if i == 0 || d.Cmp(bestDist) < 0 {
			best, bestDist = s, d
			continue
		}
		if d.Cmp(bestDist) == 0 {
			less, ok := pyLess(s, best)
			if !ok {
				return supported[0]
			}
			if less {
				best = s
			}
		}
	}
	return best
}

// Python less than for two numbers or two strings, false ok when the types do not order
func pyLess(a, b any) (bool, bool) {
	as, aStr := a.(string)
	bs, bStr := b.(string)
	if aStr || bStr {
		if aStr && bStr {
			return as < bs, true
		}
		return false, false
	}
	fa, err1 := pyint.Float(a)
	fb, err2 := pyint.Float(b)
	if err1 != nil || err2 != nil {
		return false, false
	}
	return fa < fb, true
}

// Text and image to video entries declare durations, edit, upscale and avatar models do not
func openRouterIsGenerative(entry map[string]any) bool {
	return pyTruthy(entry["supported_durations"])
}

// List value of an entry key, nil when absent or not a list
func entryList(entry map[string]any, key string) []any {
	l, _ := entry[key].([]any)
	return l
}

// Capability dict of a catalog entry
func openRouterEntryCapabilities(entry map[string]any) map[string]any {
	var durations []int
	for _, d := range entryList(entry, "supported_durations") {
		switch d.(type) {
		case string, nil, []any, map[string]any:
			continue
		}
		if n, err := pyint.Of(d); err == nil && n.IsInt64() {
			durations = append(durations, int(n.Int64()))
		}
	}
	modalities := []any{"text"}
	if pyTruthy(entry["supported_frame_images"]) {
		modalities = []any{"text", "image"}
	}
	aspects := entryList(entry, "supported_aspect_ratios")
	if !pyTruthy(entry["supported_aspect_ratios"]) {
		aspects = stringsToAny(openRouterAllAspectRatios)
	}
	resolutions := entryList(entry, "supported_resolutions")
	if !pyTruthy(entry["supported_resolutions"]) {
		resolutions = stringsToAny(openRouterResolutionOrder)
	}
	maxD, minD := 30, 1
	if len(durations) > 0 {
		maxD, minD = durations[0], durations[0]
		for _, d := range durations[1:] {
			maxD, minD = max(maxD, d), min(minD, d)
		}
	}
	return map[string]any{
		"modalities": modalities, "aspect_ratios": append([]any(nil), aspects...), "resolutions": append([]any(nil), resolutions...),
		"max_duration": maxD, "min_duration": minD, "supports_audio": pyTruthy(entry["generate_audio"]),
		"supports_negative_prompt": false, "supports_seed": pyTruthy(entry["seed"]), "supports_upscale": false,
		"max_reference_images": openRouterVideoMaxReferences,
	}
}

// Image content part naming the URL
func openRouterImagePart(u string) *pyjson.Object {
	inner := pyjson.NewObject()
	inner.Set("url", u)
	part := pyjson.NewObject()
	part.Set("type", "image_url")
	part.Set("image_url", inner)
	return part
}

// Public HTTPS or inline data:image URL, the only frame sources OpenRouter can fetch
func openRouterAcceptableImageRef(value string) bool {
	lowered := strings.ToLower(value)
	return strings.HasPrefix(lowered, "https://") || strings.HasPrefix(lowered, "data:image/")
}

// Request inputs of one OpenRouter submit
type openRouterPayloadInput struct {
	Model, Prompt, ImageURL string
	ReferenceImageURLs      []string
	Duration                *int
	AspectRatio, Resolution string
	Audio                   *bool
	Seed                    *int
}

// OpenRouter body clamped to the model's live limits with unsupported toggles dropped
func openRouterBuildPayload(entry map[string]any, in openRouterPayloadInput) *pyjson.Object {
	payload := pyjson.NewObject()
	payload.Set("model", in.Model)
	payload.Set("prompt", in.Prompt)
	if in.AspectRatio != "" {
		payload.Set("aspect_ratio", openRouterNearest(in.AspectRatio, entryList(entry, "supported_aspect_ratios"), nil))
	}
	if in.Resolution != "" {
		payload.Set("resolution", openRouterNearest(in.Resolution, entryList(entry, "supported_resolutions"), openRouterResolutionHeight))
	}
	if in.Duration != nil && *in.Duration != 0 {
		picked := openRouterNearest(*in.Duration, entryList(entry, "supported_durations"), nil)
		if n, err := pyint.Of(picked); err == nil {
			payload.Set("duration", pyint.Native(n))
		}
	}
	if in.ImageURL != "" {
		frame := openRouterImagePart(in.ImageURL)
		frame.Set("frame_type", "first_frame")
		payload.Set("frame_images", []any{frame})
	}
	if len(in.ReferenceImageURLs) > 0 {
		refs := in.ReferenceImageURLs
		if len(refs) > openRouterVideoMaxReferences {
			refs = refs[:openRouterVideoMaxReferences]
		}
		parts := make([]any, len(refs))
		for i, u := range refs {
			parts[i] = openRouterImagePart(u)
		}
		payload.Set("input_references", parts)
	}
	if in.Audio != nil && pyTruthy(entry["generate_audio"]) {
		payload.Set("generate_audio", *in.Audio)
	}
	if in.Seed != nil && pyTruthy(entry["seed"]) {
		payload.Set("seed", *in.Seed)
	}
	return payload
}

// Every generative model on the OpenRouter video API with catalog and limits discovered live
type openRouterVideoGenProvider struct {
	provider.CatalogProviderDefaults
	pollInterval   time.Duration
	pollDeadline   time.Duration
	requestTimeout time.Duration
	mu             sync.Mutex
	cache          []map[string]any
	cachedAt       time.Time
	catalog        func(ctx context.Context) []map[string]any
	configured     func(ctx context.Context) string
	session        func() *http.Client
	sleep          func(ctx context.Context, d time.Duration) error
}

func newOpenRouterVideoGenProvider() *openRouterVideoGenProvider {
	p := &openRouterVideoGenProvider{pollInterval: 10 * time.Second, pollDeadline: 900 * time.Second, requestTimeout: 60 * time.Second}
	p.Self = p
	p.catalog = p.liveCatalog
	p.configured = openRouterConfiguredModel
	p.session = func() *http.Client {
		return &http.Client{Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, ForceAttemptHTTP2: true}}
	}
	p.sleep = videoPollSleep
	return p
}

func (p *openRouterVideoGenProvider) Name() string        { return "openrouter" }
func (p *openRouterVideoGenProvider) DisplayName() string { return "OpenRouter" }

// API key and base URL from the runtime resolver chat uses, pooled and profile scoped credentials included
func (p *openRouterVideoGenProvider) credentials(ctx context.Context) (string, string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	rt, err := provider.ResolveRuntimeProvider(ctx, home.Dir(ctx), "openrouter", "", "", "")
	if err != nil {
		return "", "", err
	}
	base := strings.TrimSpace(rt.BaseURL)
	if base == "" {
		base = openRouterVideoDefaultBaseURL
	}
	return strings.TrimSpace(rt.APIKey), strings.TrimRight(base, "/"), nil
}

func openRouterVideoHeaders(apiKey string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + apiKey, "Content-Type": "application/json",
		"HTTP-Referer": "https://github.com/NousResearch/hermes-agent", "X-Title": "Hermes Agent"}
}

func (p *openRouterVideoGenProvider) IsAvailable() bool {
	return p.availableIn(context.Background())
}

// Whether a non blank credential resolves for the profile the context routes to
func (p *openRouterVideoGenProvider) availableIn(ctx context.Context) bool {
	key, _, err := p.credentials(ctx)
	if err != nil {
		videoGenLog.Debug(fmt.Sprintf("OpenRouter video credential resolution failed: %v", err))
		return false
	}
	return key != ""
}

// Live videos/models entries cached per TTL, the offline snapshot when unreachable
func (p *openRouterVideoGenProvider) liveCatalog(ctx context.Context) []map[string]any {
	p.mu.Lock()
	if p.cache != nil && time.Since(p.cachedAt) < openRouterVideoCatalogTTL {
		cached := p.cache
		p.mu.Unlock()
		return cached
	}
	p.mu.Unlock()
	entries, err := p.fetchCatalog(ctx)
	if err != nil {
		videoGenLog.Debug(fmt.Sprintf("OpenRouter video catalog unavailable: %v", err))
	}
	if len(entries) == 0 {
		return openRouterFallbackCatalog()
	}
	p.mu.Lock()
	p.cache, p.cachedAt = entries, time.Now()
	p.mu.Unlock()
	return entries
}

// One public catalog fetch keeping object entries with a truthy id
func (p *openRouterVideoGenProvider) fetchCatalog(ctx context.Context) ([]map[string]any, error) {
	_, base, err := p.credentials(ctx)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyFromEnvironment}}
	defer client.CloseIdleConnections()
	resp, raw, err := requestsDo(ctx, client, http.MethodGet, base+"/videos/models", nil, nil, openRouterVideoCatalogTimeout)
	if err != nil {
		return nil, err
	}
	if err := ttsRaiseForStatus(resp); err != nil {
		return nil, err
	}
	obj, err := requestsJSONObject(raw)
	if err != nil {
		return nil, err
	}
	data, _ := pyjson.Plain(obj["data"]).([]any)
	var entries []map[string]any
	for _, e := range data {
		if m, ok := e.(map[string]any); ok && pyTruthy(m["id"]) {
			entries = append(entries, m)
		}
	}
	return entries, nil
}

// Catalog row of the model id, empty for an unknown id so the request passes through unclamped
func (p *openRouterVideoGenProvider) entry(ctx context.Context, modelID string) map[string]any {
	for _, e := range p.catalog(ctx) {
		if pyrepr.Equal(e["id"], modelID) {
			return e
		}
	}
	return map[string]any{}
}

// Stripped video_gen.model from the profile config, the default model otherwise
func openRouterConfiguredModel(ctx context.Context) string {
	if v := readVideoGenKey(ctx, "model"); v != "" {
		return v
	}
	return openRouterVideoDefaultModel
}

// Stripped video_gen key from the active profile config.yaml, empty when unset or unreadable
func readVideoGenKey(ctx context.Context, key string) string {
	return cfgString(videoGenConfig(ctx), key)
}

// Picker rows of the generative catalog entries with duration, resolution, audio and i2v hints
func (p *openRouterVideoGenProvider) ListModels() []map[string]any {
	rows := []map[string]any{}
	for _, entry := range p.catalog(context.Background()) {
		if !openRouterIsGenerative(entry) {
			continue
		}
		caps := openRouterEntryCapabilities(entry)
		var res []string
		for _, r := range caps["resolutions"].([]any) {
			res = append(res, pyrepr.Str(r))
		}
		extras := []string{fmt.Sprintf("%d-%ds", caps["min_duration"], caps["max_duration"]), strings.Join(res, "/")}
		if caps["supports_audio"] == true {
			extras = append(extras, "audio")
		}
		if pyIn("image", caps["modalities"].([]any)) {
			extras = append(extras, "i2v")
		}
		display := entry["name"]
		if !pyTruthy(display) {
			display = entry["id"]
		}
		rows = append(rows, map[string]any{"id": entry["id"], "display": display, "strengths": strings.Join(extras, ", "),
			"price": openRouterPriceLabel(entry["pricing_skus"]), "modalities": caps["modalities"],
			"min_duration": caps["min_duration"], "max_duration": caps["max_duration"]})
	}
	return rows
}

func (p *openRouterVideoGenProvider) DefaultModel() string { return openRouterVideoDefaultModel }

// Surface of the configured model, the API wide union when the id is unknown or the catalog is down
func (p *openRouterVideoGenProvider) Capabilities() map[string]any {
	ctx := context.Background()
	if e := p.entry(ctx, p.configured(ctx)); len(e) > 0 {
		return openRouterEntryCapabilities(e)
	}
	return map[string]any{"modalities": []any{"text", "image"}, "aspect_ratios": stringsToAny(openRouterAllAspectRatios),
		"resolutions": stringsToAny(openRouterResolutionOrder), "max_duration": 30, "min_duration": 1,
		"supports_audio": true, "supports_negative_prompt": false, "supports_seed": true,
		"supports_upscale": false, "max_reference_images": openRouterVideoMaxReferences}
}

func (p *openRouterVideoGenProvider) GetSetupSchema() map[string]any {
	return map[string]any{"name": "OpenRouter", "badge": "paid",
		"tag": "Veo 3.1, Sora 2 Pro, Kling 3, Seedance 2, Wan 3, Hailuo 3, Grok Imagine & more — live catalog; " +
			"text-to-video, image-to-video & reference-to-video; uses OPENROUTER_API_KEY",
		"env_vars": []any{map[string]any{"key": "OPENROUTER_API_KEY", "prompt": "OpenRouter API key", "url": "https://openrouter.ai/settings/keys"}}}
}

// Polls the job to a terminal status within the deadline, a TimeoutError naming the last status otherwise
func (p *openRouterVideoGenProvider) poll(ctx context.Context, client *http.Client, jobID, base string, headers map[string]string) (*pyjson.Object, error) {
	deadline := time.Now().Add(p.pollDeadline)
	pollURL := base + "/videos/" + jobID
	last := "unknown"
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, fmt.Errorf("video job %s did not finish within %ds (last status=%s)", jobID, int(p.pollDeadline.Seconds()), last)
		}
		resp, raw, err := requestsDo(ctx, client, http.MethodGet, pollURL, headers, nil, max(time.Millisecond, min(p.requestTimeout, remaining)))
		if err != nil {
			return nil, err
		}
		if err := ttsRaiseForStatus(resp); err != nil {
			return nil, err
		}
		payload, err := requestsJSONObjectOrdered(raw)
		if err != nil {
			return nil, err
		}
		status, _ := payload.Get("status")
		last = "unknown"
		if pyTruthy(status) {
			last = strings.ToLower(pyrepr.Str(status))
		}
		if openRouterVideoTerminal[last] {
			return payload, nil
		}
		if err := p.sleep(ctx, min(p.pollInterval, max(0, time.Until(deadline)))); err != nil {
			return nil, err
		}
	}
}

// Downloads the output from the configured origin with the bearer key, never from provider supplied URLs
func (p *openRouterVideoGenProvider) saveCompletedVideo(ctx context.Context, env *tools.Env, jobID, base string, headers map[string]string) (string, error) {
	return saveURLVideo(ctx, env, base+"/videos/"+jobID+"/content", videoURLOptions{Prefix: "openrouter", Headers: headers,
		RequireVideoContentType: true, TrustedOrigin: true})
}

// Submits, polls and downloads one video job under a single resolved credential
func (p *openRouterVideoGenProvider) Generate(ctx context.Context, env *tools.Env, req VideoGenRequest) (map[string]any, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	prompt := strings.TrimSpace(req.Prompt)
	modelID := strings.TrimSpace(req.Model)
	if modelID == "" {
		modelID = p.configured(ctx)
	}
	aspect := req.aspect()
	fail := func(errText, errorType string) map[string]any {
		return videoErrorResponse(errText, errorType, "openrouter", modelID, prompt, aspect)
	}
	if prompt == "" {
		return fail("prompt is required", "invalid_request"), nil
	}
	apiKey, base, err := p.credentials(ctx)
	if err != nil {
		return fail(fmt.Sprintf("Could not resolve OpenRouter credentials: %v", err), "missing_credentials"), nil
	}
	if apiKey == "" {
		return fail("No OpenRouter credential: set OPENROUTER_API_KEY or run `icebath auth add openrouter`", "missing_credentials"), nil
	}
	headers := openRouterVideoHeaders(apiKey)
	imageURL := strings.TrimSpace(req.ImageURL)
	var refs []string
	for _, r := range req.ReferenceImageURLs {
		if s := strings.TrimSpace(r); s != "" {
			refs = append(refs, s)
		}
	}
	var inputs []string
	if imageURL != "" {
		inputs = append(inputs, imageURL)
	}
	for _, ref := range append(inputs, refs...) {
		if !openRouterAcceptableImageRef(ref) {
			return fail("image inputs must be public HTTPS URLs or data:image/ URLs (OpenRouter fetches them itself)", "invalid_request"), nil
		}
	}
	payload := openRouterBuildPayload(p.entry(ctx, modelID), openRouterPayloadInput{Model: modelID, Prompt: prompt, ImageURL: imageURL,
		ReferenceImageURLs: refs, Duration: req.Duration, AspectRatio: aspect, Resolution: req.resolution(), Audio: req.Audio, Seed: req.Seed})
	client := p.session()
	jobID, job, videoPath, failure, failureKind, err := p.run(ctx, env, client, base, headers, payload)
	client.CloseIdleConnections()
	if err != nil {
		videoGenLog.Debug("OpenRouter video generation failed", "err", err)
		return fail(fmt.Sprintf("OpenRouter video generation failed: %v", err), "api_error"), nil
	}
	if failure != "" {
		return fail(failure, failureKind), nil
	}
	extra := map[string]any{"job_id": jobID}
	if usage, ok := pyjson.Plain(mustGet(job, "usage")).(map[string]any); ok && usage["cost"] != nil {
		extra["cost"] = usage["cost"]
	}
	modality := "text"
	if imageURL != "" {
		modality = "image"
	}
	aspectOut := ""
	if v := mustGet(payload, "aspect_ratio"); pyTruthy(v) {
		aspectOut = pyrepr.Str(v)
	}
	duration := 0
	if v := mustGet(payload, "duration"); pyTruthy(v) {
		if n, err := pyint.Of(v); err == nil {
			duration = int(n.Int64())
		}
	}
	return videoSuccessResponse(videoPath, modelID, prompt, modality, aspectOut, duration, "openrouter", extra), nil
}

// Submit, poll and download, a failure text for rejected or failed jobs and an error for exceptions
func (p *openRouterVideoGenProvider) run(ctx context.Context, env *tools.Env, client *http.Client, base string, headers map[string]string, payload *pyjson.Object) (jobID string, job *pyjson.Object, videoPath, failure, failureKind string, err error) {
	body, err := pyjson.Dumps(payload)
	if err != nil {
		return "", nil, "", "", "", err
	}
	resp, raw, err := requestsDo(ctx, client, http.MethodPost, base+"/videos", headers, []byte(body), p.requestTimeout)
	if err != nil {
		return "", nil, "", "", "", err
	}
	if resp.StatusCode >= 400 {
		detail := ""
		if obj, err := requestsJSONObject(raw); err == nil {
			errField := obj["error"]
			if !pyTruthy(errField) {
				errField = map[string]any{}
			}
			if m, ok := pyjson.Plain(errField).(map[string]any); ok && pyTruthy(m["message"]) {
				detail = pyrepr.Str(m["message"])
			}
		}
		if detail == "" {
			text, _ := pyjson.DecodeBytes(raw)
			if r := []rune(text); len(r) > 300 {
				text = string(r[:300])
			}
			detail = text
		}
		return "", nil, "", fmt.Sprintf("OpenRouter rejected the request (HTTP %d): %s", resp.StatusCode, detail), "api_error", nil
	}
	submitted, err := requestsJSONObject(raw)
	if err != nil {
		return "", nil, "", "", "", err
	}
	if pyTruthy(submitted["id"]) {
		jobID = strings.TrimSpace(pyrepr.Str(submitted["id"]))
	}
	if jobID == "" {
		return "", nil, "", "OpenRouter submit response did not contain a job id", "api_error", nil
	}
	job, err = p.poll(ctx, client, jobID, base, headers)
	if err != nil {
		return "", nil, "", "", "", err
	}
	status := ""
	if v := mustGet(job, "status"); pyTruthy(v) {
		status = strings.ToLower(pyrepr.Str(v))
	}
	if status != "completed" {
		text := "video job ended with status=" + pyrepr.Repr(status)
		if v := mustGet(job, "error"); pyTruthy(v) {
			text = pyrepr.Str(v)
		}
		return jobID, job, "", text, "job_failed", nil
	}
	videoPath, err = p.saveCompletedVideo(ctx, env, jobID, base, headers)
	if err != nil {
		return "", nil, "", "", "", err
	}
	return jobID, job, videoPath, "", "", nil
}

// Value of an ordered object key, nil when absent
func mustGet(o *pyjson.Object, key string) any {
	if o == nil {
		return nil
	}
	v, _ := o.Get(key)
	return v
}

// Body of a requests style response parsed as a JSON object, the AttributeError text get raises otherwise
func requestsJSONObject(raw []byte) (map[string]any, error) {
	obj, err := requestsJSONObjectOrdered(raw)
	if err != nil {
		return nil, err
	}
	m, _ := pyjson.Plain(obj).(map[string]any)
	return m, nil
}

// Ordered JSON object of a response body, the decode error or the AttributeError text get raises
func requestsJSONObjectOrdered(raw []byte) (*pyjson.Object, error) {
	text, _ := pyjson.DecodeBytes(raw)
	parsed, err := pyjson.Loads(text)
	if err != nil {
		return nil, err
	}
	obj, ok := pyjson.AsObject(parsed)
	if !ok {
		return nil, fmt.Errorf("'%s' object has no attribute 'get'", pyrepr.TypeName(parsed))
	}
	return obj, nil
}

// One request with requests style connect and read timeouts, every stall longer than timeout aborting it
func requestsDo(ctx context.Context, client *http.Client, method, rawURL string, headers map[string]string, body []byte, timeout time.Duration) (*http.Response, []byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	actx, cancel := context.WithCancel(ctx)
	defer cancel()
	timer := time.AfterFunc(timeout, cancel)
	defer timer.Stop()
	trace := &httptrace.ClientTrace{
		GotConn:              func(httptrace.GotConnInfo) { timer.Reset(timeout) },
		WroteRequest:         func(httptrace.WroteRequestInfo) { timer.Reset(timeout) },
		GotFirstResponseByte: func() { timer.Reset(timeout) },
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(actx, trace), method, rawURL, reader)
	if err != nil {
		return nil, nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		if errors.Is(actx.Err(), context.Canceled) && ctx.Err() == nil {
			return nil, nil, requestsTimeoutError(req, timeout)
		}
		return nil, nil, err
	}
	defer resp.Body.Close()
	raw, err := readWithIdleTimeout(resp.Body, timeout, cancel)
	if err != nil {
		return nil, nil, err
	}
	return resp, raw, nil
}

// ReadTimeout text urllib3 raises naming the connection pool host, port and read timeout
func requestsTimeoutError(req *http.Request, timeout time.Duration) error {
	pool, port := "HTTPConnectionPool", "80"
	if req.URL.Scheme == "https" {
		pool, port = "HTTPSConnectionPool", "443"
	}
	if p := req.URL.Port(); p != "" {
		port = p
	}
	return fmt.Errorf("%s(host='%s', port=%s): Read timed out. (read timeout=%s)", pool, req.URL.Hostname(), port, pyrepr.Float(timeout.Seconds()))
}

func init() {
	plugins.RegisterBundled(videoGenBackendPlugin{name: "openrouter", author: "Nous Research", requiresEnv: []string{"OPENROUTER_API_KEY"},
		description: "OpenRouter video generation backend. Every generative model on the /api/v1/videos API (Veo, Sora, Kling, Seedance, Wan, Hailuo, Grok Imagine, FLUX Video, …) — text-to-video, image-to-video and reference-to-video; catalog and per-model limits discovered live.",
		provider:    func() VideoGenProvider { return newOpenRouterVideoGenProvider() }})
}
