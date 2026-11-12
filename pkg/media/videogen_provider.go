package media

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

var videoAspectRatios = []string{"16:9", "9:16", "1:1", "4:3", "3:4", "3:2", "2:3", "21:9"}

const defaultVideoAspectRatio = "16:9"

var videoResolutions = []string{"480p", "540p", "720p", "768p", "1080p"}

const defaultVideoResolution = "720p"

// One video_generate call as the provider sees it, empty strings standing for omitted arguments
type VideoGenRequest struct {
	Prompt             string
	Model              string
	ImageURL           string
	ReferenceImageURLs []string
	Duration           *int
	AspectRatio        string
	Resolution         string
	NegativePrompt     string
	Audio              *bool
	Seed               *int
	Kwargs             map[string]any
}

// Requested aspect ratio, 16:9 when the caller omitted it
func (r VideoGenRequest) aspect() string {
	if r.AspectRatio == "" {
		return defaultVideoAspectRatio
	}
	return r.AspectRatio
}

// Requested resolution, 720p when the caller omitted it
func (r VideoGenRequest) resolution() string {
	if r.Resolution == "" {
		return defaultVideoResolution
	}
	return r.Resolution
}

// Requested duration in seconds, zero when omitted
func (r VideoGenRequest) durationOrZero() int {
	if r.Duration == nil {
		return 0
	}
	return *r.Duration
}

// Video generation backend selected by video_gen.provider
type VideoGenProvider interface {
	provider.CatalogProviderBase
	Capabilities() map[string]any
	Generate(ctx context.Context, env *tools.Env, req VideoGenRequest) (map[string]any, error)
}

// Text only capability set with no optional features that every provider gets unless it overrides it
func defaultVideoCapabilities() map[string]any {
	return map[string]any{
		"modalities": []any{"text"}, "aspect_ratios": stringsToAny(videoAspectRatios),
		"resolutions": stringsToAny(videoResolutions), "max_duration": 10, "min_duration": 1,
		"supports_audio": false, "supports_negative_prompt": false, "supports_seed": false,
		"supports_upscale": false, "max_reference_images": 0,
	}
}

// List of any holding each string
func stringsToAny(list []string) []any {
	out := make([]any, len(list))
	for i, s := range list {
		out[i] = s
	}
	return out
}

var generatedVideoKind = generatedSubdir + "/videos"

// Decodes base64 video data into the generated videos cache
func saveB64Video(env *tools.Env, b64, prefix, extension string) (string, error) {
	if prefix == "" {
		prefix = "video"
	}
	if extension == "" {
		extension = "mp4"
	}
	return saveB64(env, generatedVideoKind, b64, prefix, extension)
}

// Writes raw video bytes into the generated videos cache
func saveBytesVideo(env *tools.Env, raw []byte, prefix, extension string) (string, error) {
	if prefix == "" {
		prefix = "video"
	}
	if extension == "" {
		extension = "mp4"
	}
	return saveBytes(env, generatedVideoKind, raw, prefix, extension)
}

var urlVideoContentTypes = map[string]string{
	"video/mp4": "mp4", "video/webm": "webm", "video/quicktime": "mov", "video/x-matroska": "mkv",
}

// Download options of a provider video URL, zero values taking the 180 s and 200 MB defaults
type videoURLOptions struct {
	Prefix                  string
	Timeout                 time.Duration
	MaxBytes                int64
	Headers                 map[string]string
	RequireVideoContentType bool
	TrustedOrigin           bool
}

// Downloads a provider video URL into the generated videos cache, tests swap it to observe the call
var saveURLVideo = func(ctx context.Context, env *tools.Env, rawURL string, o videoURLOptions) (string, error) {
	if o.Prefix == "" {
		o.Prefix = "video"
	}
	if o.Timeout <= 0 {
		o.Timeout = 180 * time.Second
	}
	if o.MaxBytes <= 0 {
		o.MaxBytes = 200 * 1024 * 1024
	}
	return saveURL(ctx, env, generatedVideoKind, rawURL, saveURLOptions{
		Prefix: o.Prefix, Timeout: o.Timeout, MaxBytes: o.MaxBytes, ChunkSize: 256 * 1024,
		ContentTypes: urlVideoContentTypes, URLExtensions: []string{"mp4", "webm", "mov", "mkv"},
		DefaultExtension: "mp4", Label: "Video", EmptyError: "Video at {url} was empty (0 bytes).",
		Headers: o.Headers, RequireKnownContentType: o.RequireVideoContentType, TrustedOrigin: o.TrustedOrigin,
	})
}

// Success envelope, extra keys added only where the standard keys leave them unset
func videoSuccessResponse(video, model, prompt, modality, aspectRatio string, duration int, providerName string, extra map[string]any) map[string]any {
	if modality == "" {
		modality = "text"
	}
	payload := map[string]any{
		"success": true, "video": video, "model": model, "prompt": prompt, "modality": modality,
		"aspect_ratio": aspectRatio, "duration": duration, "provider": providerName,
	}
	for k, v := range extra {
		if _, ok := payload[k]; !ok {
			payload[k] = v
		}
	}
	return payload
}

// Failure envelope with a null video and the provider_error type by default
func videoErrorResponse(errText, errorType, providerName, model, prompt, aspectRatio string) map[string]any {
	if errorType == "" {
		errorType = "provider_error"
	}
	return map[string]any{
		"success": false, "video": nil, "error": errText, "error_type": errorType, "model": model,
		"prompt": prompt, "aspect_ratio": aspectRatio, "provider": providerName,
	}
}

var videoTerminalStatuses = map[string]bool{"completed": true, "succeeded": true, "failed": true, "error": true, "cancelled": true, "canceled": true}

// Sleep between job polls, tests replace it to skip the wait
var videoPollSleep = func(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Text and image to video over the OpenAI videos API shared by DeepInfra, Sora and OpenRouter style backends
type openAICompatibleVideoGenProvider struct {
	provider.CatalogProviderDefaults
	providerID     string
	label          string
	envKey         string
	defaultBaseURL string
	pollInterval   time.Duration
	pollDeadline   time.Duration
	models         func(ctx context.Context) []map[string]any
	caps           func() map[string]any
	setup          func() map[string]any
}

// OpenAI compatible provider bound to itself with the 5 s poll interval and 900 s deadline
func newOpenAICompatibleVideoGenProvider(p openAICompatibleVideoGenProvider) *openAICompatibleVideoGenProvider {
	if p.envKey == "" {
		p.envKey = "OPENAI_API_KEY"
	}
	if p.defaultBaseURL == "" {
		p.defaultBaseURL = "https://api.openai.com/v1"
	}
	if p.pollInterval == 0 {
		p.pollInterval = 5 * time.Second
	}
	if p.pollDeadline == 0 {
		p.pollDeadline = 900 * time.Second
	}
	out := &p
	out.Self = out
	return out
}

func (p *openAICompatibleVideoGenProvider) Name() string { return p.providerID }

func (p *openAICompatibleVideoGenProvider) DisplayName() string {
	if p.label != "" {
		return p.label
	}
	return p.CatalogProviderDefaults.DisplayName()
}

func (p *openAICompatibleVideoGenProvider) GetSetupSchema() map[string]any {
	if p.setup != nil {
		return p.setup()
	}
	return p.CatalogProviderDefaults.GetSetupSchema()
}

func (p *openAICompatibleVideoGenProvider) ListModels() []map[string]any {
	if p.models == nil {
		return []map[string]any{}
	}
	return p.models(context.Background())
}

func (p *openAICompatibleVideoGenProvider) Capabilities() map[string]any {
	if p.caps != nil {
		return p.caps()
	}
	return defaultVideoCapabilities()
}

// Credential read through the profile secret scope the context carries
func (p *openAICompatibleVideoGenProvider) apiKey(ctx context.Context) string {
	return strings.TrimSpace(mustSecret(ctx, p.envKey))
}

// Available when the credential resolves non blank through the process secret scope
func (p *openAICompatibleVideoGenProvider) IsAvailable() bool {
	return p.keyAvailable(context.Background())
}

// Whether the credential resolves non blank through the secret scope the context carries
func (p *openAICompatibleVideoGenProvider) keyAvailable(ctx context.Context) bool {
	return p.apiKey(ctx) != ""
}

// NAME_BASE_URL from the secret scope, the provider default otherwise
func (p *openAICompatibleVideoGenProvider) baseURL(ctx context.Context) string {
	if v := strings.TrimSpace(mustSecret(ctx, strings.ToUpper(p.providerID)+"_BASE_URL")); v != "" {
		return v
	}
	return p.defaultBaseURL
}

// Status string of a job, empty with ok false when the status is absent or not a string
func videoStatus(v *openAIVideo) (string, bool) {
	s, ok := v.get("status").(string)
	return s, ok
}

// Creates the job and polls to any terminal status, a TimeoutError once the deadline passes first
func (p *openAICompatibleVideoGenProvider) createAndPoll(ctx context.Context, client *openAIClient, req map[string]any, extra map[string]any, extraOrder []string) (*openAIVideo, error) {
	video, err := client.VideosCreate(ctx, req, extra, extraOrder)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(p.pollDeadline)
	for {
		if s, ok := videoStatus(video); ok && videoTerminalStatuses[s] {
			return video, nil
		}
		if !time.Now().Before(deadline) {
			id := "?"
			if raw, ok := video.attr("id"); ok {
				id = pyrepr.Str(raw)
			}
			return nil, fmt.Errorf("video job %s did not reach a terminal status within %ds (last status=%s)",
				id, int(p.pollDeadline.Seconds()), pyrepr.Repr(video.get("status")))
		}
		if err := videoPollSleep(ctx, p.pollInterval); err != nil {
			return nil, err
		}
		id, err := video.id()
		if err != nil {
			return nil, err
		}
		if video, err = client.VideosRetrieve(ctx, id); err != nil {
			return nil, err
		}
	}
}

// Runs one create, poll and download cycle and caches the output, falling back to the delivery URL
func (p *openAICompatibleVideoGenProvider) Generate(ctx context.Context, env *tools.Env, req VideoGenRequest) (map[string]any, error) {
	name := p.providerID
	prompt, aspect, resolution := req.Prompt, req.aspect(), req.resolution()
	if strings.TrimSpace(prompt) == "" {
		return videoErrorResponse("prompt is required", "invalid_request", name, "", "", ""), nil
	}
	apiKey := p.apiKey(ctx)
	if apiKey == "" {
		return videoErrorResponse(p.envKey+" is not set", "missing_credentials", name, "", "", ""), nil
	}
	newClient := newOpenAIClientHook
	modelID := req.Model
	if modelID == "" {
		modelID = p.DefaultModel()
	}
	if modelID == "" {
		return videoErrorResponse(fmt.Sprintf("no %s video model available (live catalog empty?)", name), "no_model", name, "", "", ""), nil
	}
	fail := func(errText, errorType string) map[string]any {
		return videoErrorResponse(errText, errorType, name, modelID, prompt, aspect)
	}
	extra := map[string]any{"aspect_ratio": aspect}
	if req.NegativePrompt != "" {
		extra["negative_prompt"] = req.NegativePrompt
	}
	if req.ImageURL != "" {
		extra["image_url"] = req.ImageURL
	}
	if req.Seed != nil {
		extra["seed"] = *req.Seed
	}
	call := map[string]any{"model": modelID, "prompt": prompt}
	if d := req.durationOrZero(); d != 0 {
		call["seconds"] = fmt.Sprint(d)
	}
	if resolution != "" {
		call["size"] = resolution
	}
	baseURL := p.baseURL(ctx)
	opts := openAIClientOptions{APIKey: &apiKey, BaseURL: &baseURL}
	if hc := provider.BuildKeepaliveHTTPClient(baseURL, false, provider.TLSVerify{}); hc != nil {
		opts.HTTPClient = hc
	}
	client, err := newClient(env, opts)
	if err != nil {
		return nil, err
	}
	defer client.Close()
	video, err := p.createAndPoll(ctx, client, call, extra, []string{"negative_prompt", "aspect_ratio", "image_url", "seed"})
	if err != nil {
		logOf(env).Debug(name+" video generation failed", "err", err)
		return fail(fmt.Sprintf("%s video generation failed: %v", name, err), "api_error"), nil
	}
	status := video.get("status")
	if s, _ := status.(string); s != "completed" && s != "succeeded" {
		if text, ok := video.errorText(); ok {
			return fail(text, "job_failed"), nil
		}
		return fail("video job ended with status="+pyrepr.Repr(status), "job_failed"), nil
	}
	deliveryURL := ""
	if items, ok := video.get("data").([]any); ok {
		for _, item := range items {
			if m, ok := item.(map[string]any); ok && pyTruthy(m["url"]) {
				deliveryURL = pyrepr.Str(m["url"])
				break
			}
		}
	}
	var videoRef string
	if deliveryURL != "" {
		videoRef, err = saveURLVideo(ctx, env, deliveryURL, videoURLOptions{Prefix: name})
	} else {
		var id string
		if id, err = video.id(); err == nil {
			var raw []byte
			if raw, err = client.VideosDownloadContent(ctx, id); err == nil {
				videoRef, err = saveBytesVideo(env, raw, name, "")
			}
		}
	}
	if err != nil {
		if deliveryURL == "" {
			return fail(fmt.Sprintf("%s video job succeeded but no output could be retrieved: %v", name, err), "empty_response"), nil
		}
		logOf(env).Debug(fmt.Sprintf("%s: saving video locally failed (%v); returning URL", name, err))
		videoRef = deliveryURL
	}
	modality := "text"
	if req.ImageURL != "" {
		modality = "image"
	}
	return videoSuccessResponse(videoRef, modelID, prompt, modality, aspect, req.durationOrZero(), name, nil), nil
}

var videoGenLog = slog.Default().With("logger", "agent.video_gen_registry")

var videoGenRegistry = provider.NewProviderRegistry[VideoGenProvider](provider.ProviderRegistryOptions{
	Label: "Video gen", Logger: videoGenLog,
})

// Registers a video generation provider, a same name registration replacing the earlier one
func RegisterVideoGenProvider(p VideoGenProvider, scope string) error {
	return videoGenRegistry.Register(p, scope)
}

// Registered video generation providers sorted by name
func ListVideoGenProviders(ctx context.Context, scope string) []VideoGenProvider {
	return videoGenRegistry.ListProviders(ctx, scope)
}

// Provider registered under the name, scoped registration first, nil when absent
func GetVideoGenProvider(ctx context.Context, name, scope string) VideoGenProvider {
	return videoGenRegistry.GetProvider(ctx, name, scope)
}

// Plugin facing registry that register_video_gen_provider writes into, adapting hosted Python providers
func VideoGenScopedRegistry() plugins.ScopedRegistry {
	return hostedVideoScopedRegistry{videoGenRegistry.Scoped()}
}

// Scoped registry view that wraps plugin host objects as VideoGenProvider values before delegating
type hostedVideoScopedRegistry struct{ inner plugins.ScopedRegistry }

func (s hostedVideoScopedRegistry) Accepts(p any) bool {
	return s.inner.Accepts(adaptHostedVideoGenProvider(p))
}
func (s hostedVideoScopedRegistry) ProviderName(p any) string {
	return s.inner.ProviderName(adaptHostedVideoGenProvider(p))
}
func (s hostedVideoScopedRegistry) Identity(p any) error {
	return s.inner.Identity(adaptHostedVideoGenProvider(p))
}
func (s hostedVideoScopedRegistry) Snapshot(name, scope string) any {
	return s.inner.Snapshot(name, scope)
}
func (s hostedVideoScopedRegistry) Register(p any, scope string) (bool, error) {
	return s.inner.Register(adaptHostedVideoGenProvider(p), scope)
}
func (s hostedVideoScopedRegistry) Restore(name string, current, replacement any, scope string) bool {
	return s.inner.Restore(name, adaptHostedVideoGenProvider(current), adaptHostedVideoGenProvider(replacement), scope)
}

// Configured provider failing closed when unregistered, else the single available registered provider
func GetActiveVideoGenProvider(ctx context.Context, env *tools.Env) VideoGenProvider {
	ctx = envContext(ctx, env)
	configured := provider.ConfiguredProviderName(ctx, "video_gen", videoGenLog)
	snapshot := videoGenRegistry.Merged(ctx, "")
	if configured != "" {
		p, ok := snapshot.Get(configured)
		if !ok || p == nil {
			videoGenLog.Debug(fmt.Sprintf("video_gen.provider='%s' configured but not registered; failing closed", configured))
			return nil
		}
		return p
	}
	var avail []VideoGenProvider
	for _, p := range snapshot.Values() {
		if provider.IsAvailableSafe(p, videoGenLog, "video_gen provider %s.is_available() raised %s", slog.LevelDebug, false) {
			avail = append(avail, p)
		}
	}
	if len(avail) == 1 {
		return avail[0]
	}
	return nil
}

// Python plugin provider object proxied from the plugin host
type hostedVideoGenProvider struct{ obj *plugins.HostedObject }

// Plugin host object that subclasses VideoGenProvider as a provider, any other value unchanged
func adaptHostedVideoGenProvider(p any) any {
	obj, ok := p.(*plugins.HostedObject)
	if !ok || obj == nil || !obj.HasMethod("generate") {
		return p
	}
	return hostedVideoGenProvider{obj: obj}
}

func (h hostedVideoGenProvider) get(name string) any {
	v, err := h.obj.Get(context.Background(), name)
	if err != nil {
		return nil
	}
	return v
}

func (h hostedVideoGenProvider) call(method string) any {
	v, err := h.obj.Call(context.Background(), method, nil, nil)
	if err != nil {
		return nil
	}
	return v
}

func (h hostedVideoGenProvider) Name() string {
	s, _ := h.get("name").(string)
	return s
}

func (h hostedVideoGenProvider) DisplayName() string {
	if s, ok := h.get("display_name").(string); ok {
		return s
	}
	return pystr.Title(h.Name())
}

func (h hostedVideoGenProvider) GetSetupSchema() map[string]any {
	if m, ok := h.call("get_setup_schema").(map[string]any); ok {
		return m
	}
	return map[string]any{"name": h.DisplayName(), "badge": "", "tag": "", "env_vars": []any{}}
}

func (h hostedVideoGenProvider) IsAvailable() bool {
	v, err := h.obj.Call(context.Background(), "is_available", nil, nil)
	if err != nil {
		panic(err)
	}
	b, _ := v.(bool)
	return b
}

func (h hostedVideoGenProvider) ListModels() []map[string]any {
	rows, _ := h.call("list_models").([]any)
	var out []map[string]any
	for _, r := range rows {
		if m, ok := r.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func (h hostedVideoGenProvider) DefaultModel() string {
	s, _ := h.call("default_model").(string)
	return s
}

func (h hostedVideoGenProvider) Capabilities() map[string]any {
	if m, ok := h.call("capabilities").(map[string]any); ok {
		return m
	}
	return defaultVideoCapabilities()
}

// Calls generate with the prompt and every supplied argument as keywords, upscale and extras included
func (h hostedVideoGenProvider) Generate(ctx context.Context, _ *tools.Env, req VideoGenRequest) (map[string]any, error) {
	v, err := h.obj.Call(ctx, "generate", nil, videoGenKwargs(req))
	if err != nil {
		return nil, err
	}
	m, _ := v.(map[string]any)
	return m, nil
}

// Keyword arguments of a request with omitted values dropped the way the tool drops None entries
func videoGenKwargs(req VideoGenRequest) map[string]any {
	kwargs := map[string]any{}
	for k, v := range req.Kwargs {
		kwargs[k] = v
	}
	kwargs["prompt"] = req.Prompt
	if req.Model != "" {
		kwargs["model"] = req.Model
	}
	if req.ImageURL != "" {
		kwargs["image_url"] = req.ImageURL
	}
	if len(req.ReferenceImageURLs) > 0 {
		kwargs["reference_image_urls"] = stringsToAny(req.ReferenceImageURLs)
	}
	if req.Duration != nil {
		kwargs["duration"] = *req.Duration
	}
	kwargs["aspect_ratio"] = req.aspect()
	kwargs["resolution"] = req.resolution()
	if req.NegativePrompt != "" {
		kwargs["negative_prompt"] = req.NegativePrompt
	}
	if req.Audio != nil {
		kwargs["audio"] = *req.Audio
	}
	if req.Seed != nil {
		kwargs["seed"] = *req.Seed
	}
	return kwargs
}

// Bundled video_gen backend plugin registering its provider through the plugin context
type videoGenBackendPlugin struct {
	name, author, description string
	requiresEnv               []string
	provider                  func() VideoGenProvider
}

func (b videoGenBackendPlugin) Manifest() plugins.Manifest {
	env := make([]plugins.EnvSpec, 0, len(b.requiresEnv))
	for _, n := range b.requiresEnv {
		env = append(env, plugins.EnvSpec{Name: n})
	}
	return plugins.Manifest{Name: b.name, Version: "1.0.0", Description: b.description, Author: b.author, Kind: "backend",
		Source: "bundled", Key: "video_gen/" + b.name, RequiresEnv: env}
}

func (b videoGenBackendPlugin) Register(ctx *plugins.Context) error {
	_, err := ctx.RegisterVideoGenProvider(b.provider())
	return err
}
