package media

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Static video_generate schema the dynamic overrides rebuild at definition time
func videoGenerateSchema() map[string]any {
	return map[string]any{
		"name":        "video_generate",
		"description": "(rebuilt at get_definitions() time — see _build_dynamic_video_schema)",
		"parameters": map[string]any{
			"type":       "object",
			"properties": videoStaticProperties(),
			"required":   []any{"prompt"},
		},
	}
}

// Prompt, duration, aspect ratio and resolution parameters every schema starts from
func videoStaticProperties() map[string]any {
	return map[string]any{
		"prompt": map[string]any{"type": "string",
			"description": "Text instruction describing the desired video, motion, subject, style, camera movement, etc."},
		"duration": map[string]any{"type": "integer",
			"description": "Desired video duration in seconds. Providers clamp to their supported range. Omit for the provider default."},
		"aspect_ratio": map[string]any{"type": "string", "enum": stringsToAny(videoAspectRatios), "description": "Output aspect ratio.",
			"default": defaultVideoAspectRatio},
		"resolution": map[string]any{"type": "string", "enum": stringsToAny(videoResolutions), "description": "Output resolution.",
			"default": defaultVideoResolution},
	}
}

// Configured video_gen.provider of the environment profile, empty when unset
var readConfiguredVideoProvider = func(ctx context.Context, env *tools.Env) string {
	return readVideoGenKey(envContext(ctx, env), "provider")
}

// Configured video_gen.model of the environment profile, empty when unset
var readConfiguredVideoModel = func(ctx context.Context, env *tools.Env) string {
	return readVideoGenKey(envContext(ctx, env), "model")
}

// Provider method result with a panic or empty value replaced by the default
func providerCall[T any](call func() T, empty func(T) bool, def T) (out T) {
	defer func() {
		if recover() != nil {
			out = def
		}
	}()
	v := call()
	if empty(v) {
		return def
	}
	return v
}

// Runs idempotent plugin discovery so user installed providers are visible, returning the discovery call
func videoDiscoveredRegistry(ctx context.Context) func(force bool) {
	ensurePluginsDiscoveredHook(ctx, false)
	return func(force bool) { ensurePluginsDiscoveredHook(ctx, force) }
}

// True when at least one registered provider reports available
func checkVideoGenerationRequirements(env *tools.Env) bool {
	ctx := envContext(context.Background(), env)
	videoDiscoveredRegistry(ctx)
	for _, p := range ListVideoGenProviders(ctx, "") {
		if providerCall(p.IsAvailable, func(b bool) bool { return !b }, false) {
			return true
		}
	}
	return false
}

// Active provider or nil, a miss forcing a discovery refresh for sessions older than the plugin install
var videoResolveActiveProvider = resolveActiveVideoProviderWithRefresh

func resolveActiveVideoProviderWithRefresh(ctx context.Context, env *tools.Env) (p VideoGenProvider) {
	defer func() {
		if r := recover(); r != nil {
			videoGenLog.Debug(fmt.Sprintf("video_gen provider resolution failed: %v", r))
			p = nil
		}
	}()
	ctx = envContext(ctx, env)
	ensureDiscovered := videoDiscoveredRegistry(ctx)
	p = GetActiveVideoGenProvider(ctx, env)
	if p == nil {
		ensureDiscovered(true)
		p = GetActiveVideoGenProvider(ctx, env)
	}
	return p
}

var videoResultKeyOrder = []string{"success", "video", "error", "error_type", "model", "prompt", "modality", "aspect_ratio", "duration", "provider"}

// JSON of an envelope this package built, always serializable
func dumpVideoResult(m map[string]any) string {
	s, _ := encodeVideoResult(m)
	return s
}

// JSON of a result with the standard keys in envelope order and extras after them sorted, the TypeError dumps raises otherwise
func encodeVideoResult(m map[string]any) (string, error) {
	o := pyjson.NewObject()
	for _, k := range videoResultKeyOrder {
		if v, ok := m[k]; ok {
			o.Set(k, v)
		}
	}
	var rest []string
	for k := range m {
		if _, ok := o.Get(k); !ok {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	for _, k := range rest {
		o.Set(k, m[k])
	}
	return pyjson.Dumps(o)
}

// Error JSON naming the unregistered configured provider or pointing at the picker
func videoMissingProviderError(configured string) string {
	if configured != "" {
		return dumpVideoResult(videoErrorResponse(fmt.Sprintf("video_gen.provider='%s' is set but no plugin registered that name. "+
			"Run `icebath plugins list` to see installed video gen backends, or "+
			"`icebath tools` → Video Generation to pick one.", configured), "provider_not_registered", configured, "", "", ""))
	}
	return dumpVideoResult(videoErrorResponse("No video generation backend is configured. Run `icebath tools` → "+
		"Video Generation to enable one (xAI, FAL, OpenRouter, or DeepInfra).", "no_provider_configured", "", "", "", ""))
}

// Python exception raised inside the handler before dispatch
type videoHandlerError struct{ typeName, msg string }

func (e *videoHandlerError) Error() string      { return e.msg }
func (e *videoHandlerError) PyTypeName() string { return e.typeName }

// Stripped string argument with the fallback for falsy values, the AttributeError strip raises on other types
func stripArg(args map[string]any, key, fallback string) (string, error) {
	v := args[key]
	if !pyTruthy(v) {
		return strings.TrimSpace(fallback), nil
	}
	s, ok := v.(string)
	if !ok {
		return "", &videoHandlerError{"AttributeError", fmt.Sprintf("'%s' object has no attribute 'strip'", pyrepr.TypeName(v))}
	}
	return strings.TrimSpace(s), nil
}

var videoBoolWords = map[string]bool{"true": true, "1": true, "yes": true, "on": true, "false": false, "0": false, "no": false, "off": false}

// int of the value, nil for None, empty string and values int rejects with TypeError or ValueError
func videoCoerceInt(value any) (*int, error) {
	if value == nil || value == "" {
		return nil, nil
	}
	n, err := pyint.Of(value)
	if err != nil {
		var pe *pyint.Error
		if errors.As(err, &pe) && pe.Kind == "OverflowError" {
			return nil, &videoHandlerError{"OverflowError", pe.Msg}
		}
		return nil, nil
	}
	if !n.IsInt64() {
		return nil, &videoHandlerError{"OverflowError", "Python int too large to convert to C ssize_t"}
	}
	i := int(n.Int64())
	return &i, nil
}

// Bool of a bool or a known true or false word, nil otherwise
func videoCoerceBool(value any) *bool {
	switch v := value.(type) {
	case bool:
		return &v
	case string:
		if b, ok := videoBoolWords[strings.ToLower(strings.TrimSpace(v))]; ok {
			return &b
		}
	}
	return nil
}

// Tool error JSON of an exception the registry dispatch would have caught
func videoHandlerFailure(env *tools.Env, name string, err error) string {
	logOf(env).Error(fmt.Sprintf("Tool %s dispatch error: %s", name, err))
	return tools.ErrorResult(errors.New(tools.SanitizeToolError(fmt.Sprintf("Tool execution failed: %s: %s", pyErrorTypeName(err), err))))
}

// Generate call with a provider panic surfaced as an error like the exception Python catches
func safeVideoGenerate(ctx context.Context, p VideoGenProvider, env *tools.Env, req VideoGenRequest) (res map[string]any, err error) {
	defer func() {
		if r := recover(); r != nil {
			if e, ok := r.(error); ok {
				err = e
			} else {
				err = fmt.Errorf("%v", r)
			}
		}
	}()
	return p.Generate(ctx, env, req)
}

// video_generate handler confining sources, coercing optional args and dispatching to the active provider
func handleVideoGenerate(ctx context.Context, call *tools.Call) string {
	env, args := call.Env, call.Args
	res, err := videoGenerate(ctx, env, args, call.TaskID)
	if err != nil {
		return videoHandlerFailure(env, "video_generate", err)
	}
	return res
}

func videoGenerate(ctx context.Context, env *tools.Env, args map[string]any, taskID string) (string, error) {
	prompt, err := stripArg(args, "prompt", "")
	if err != nil {
		return "", err
	}
	imageURL, err := stripArg(args, "image_url", "")
	if err != nil {
		return "", err
	}
	var imageArg any
	if imageURL != "" {
		imageArg = imageURL
	}
	var refsArg any
	if refs := normalizeReferenceImages(args["reference_image_urls"]); len(refs) > 0 {
		refsArg = stringsToAny(refs)
	}
	imageArg, refsArg, confineErr, err := confineSourceImages(ctx, env, imageArg, refsArg, taskID)
	if err != nil {
		return "", err
	}
	if confineErr != "" {
		return confineErr, nil
	}
	req := VideoGenRequest{Prompt: prompt, Kwargs: map[string]any{}}
	if s, ok := imageArg.(string); ok {
		req.ImageURL = s
	}
	if list, ok := refsArg.([]any); ok {
		for _, r := range list {
			if s, ok := r.(string); ok {
				req.ReferenceImageURLs = append(req.ReferenceImageURLs, s)
			}
		}
	}
	if req.Duration, err = videoCoerceInt(args["duration"]); err != nil {
		return "", err
	}
	if req.AspectRatio, err = stripArg(args, "aspect_ratio", defaultVideoAspectRatio); err != nil {
		return "", err
	}
	if req.AspectRatio == "" {
		req.AspectRatio = defaultVideoAspectRatio
	}
	if req.Resolution, err = stripArg(args, "resolution", defaultVideoResolution); err != nil {
		return "", err
	}
	if req.Resolution == "" {
		req.Resolution = defaultVideoResolution
	}
	if req.NegativePrompt, err = stripArg(args, "negative_prompt", ""); err != nil {
		return "", err
	}
	req.Audio = videoCoerceBool(args["audio"])
	if req.Seed, err = videoCoerceInt(args["seed"]); err != nil {
		return "", err
	}
	if up := videoCoerceBool(args["upscale"]); up != nil {
		req.Kwargs["upscale"] = *up
	}
	if prompt == "" {
		return tools.ErrorResult(errors.New("prompt is required for video generation")), nil
	}
	_, hasOperation := args["operation"]
	_, hasVideoURL := args["video_url"]
	if hasOperation || hasVideoURL {
		return tools.ErrorResult(errors.New("video_generate only supports text-to-video, image-to-video, and " +
			"reference-to-video; use a provider-specific tool for video edit/extend")), nil
	}
	configured := readConfiguredVideoProvider(ctx, env)
	p := videoResolveActiveProvider(ctx, env)
	if p == nil {
		return videoMissingProviderError(configured), nil
	}
	model := readConfiguredVideoModel(ctx, env)
	if model == "" {
		model = p.DefaultModel()
	}
	req.Model = model
	pname := p.Name()
	fail := func(errText, errorType string) string {
		return dumpVideoResult(videoErrorResponse(errText, errorType, pname, model, prompt, ""))
	}
	result, err := safeVideoGenerate(envContext(ctx, env), p, env, req)
	if err != nil {
		if isPyTypeError(err) {
			videoGenLog.Warn(fmt.Sprintf("video_gen provider '%s' rejected kwargs (signature too narrow): %v", pname, err))
			return fail(fmt.Sprintf("Provider '%s' signature is out of date with the video_generate schema. Report this to the plugin author.", pname),
				"provider_contract"), nil
		}
		videoGenLog.Warn(fmt.Sprintf("video_gen provider '%s' raised: %v", pname, err))
		return fail(fmt.Sprintf("Provider '%s' error: %v", pname, err), "provider_exception"), nil
	}
	if result == nil {
		return fail("Provider returned a non-dict result", "provider_contract"), nil
	}
	return encodeVideoResult(result)
}

var videoCapabilityParams = []struct {
	flag, key string
	param     map[string]any
}{
	{"supports_negative_prompt", "negative_prompt", map[string]any{"type": "string", "description": "Content to avoid in the output."}},
	{"supports_audio", "audio", map[string]any{"type": "boolean", "description": "Enable native audio generation (affects pricing tier)."}},
	{"supports_seed", "seed", map[string]any{"type": "integer", "description": "Seed for reproducible outputs."}},
	{"supports_upscale", "upscale", map[string]any{"type": "boolean",
		"description": "High-resolution pass via the backend's video upscaler (~2x, extra cost/latency). Omit for native resolution."}},
}

const videoGenericDescription = "Generate video from text, a still image or reference images (configured model). " +
	"Pass `image_url` to animate an image or `reference_image_urls` for " +
	"reference-to-video. Video edit/extend workflows are not part of this " +
	"unified surface; use a dedicated provider-specific tool when one is " +
	"available. The backend and model family are user-configured via " +
	"`icebath tools` → Video Generation; the agent does not pick them. " +
	"Long-running generations may take 30 seconds to several minutes — " +
	"the call blocks until the video is ready. Returns the result in the " +
	"`video` field — either an HTTP URL or an absolute file path; reference " +
	"it in your response using the current platform's file-delivery convention."

func videoSchema(description string, properties map[string]any) map[string]any {
	return map[string]any{"description": description,
		"parameters": map[string]any{"type": "object", "properties": properties, "required": []any{"prompt"}}}
}

// Copy of a schema parameter map
func copyParam(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// Strings of a list value, other items skipped
func stringItems(v any) []string {
	var out []string
	switch l := v.(type) {
	case []any:
		for _, item := range l {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
	case []string:
		out = append(out, l...)
	}
	return out
}

// Description and params from the active provider capabilities and the configured model catalog entry
func buildDynamicVideoSchema(env *tools.Env) map[string]any {
	ctx := envContext(context.Background(), env)
	static := videoStaticProperties()
	parts := []string{videoGenericDescription}
	configuredModel := readConfiguredVideoModel(ctx, env)
	p := videoResolveActiveProvider(ctx, env)
	if p == nil {
		parts = append(parts, "\nNo video backend is available. Calls will return an error "+
			"until the user picks one via `icebath tools` → Video Generation.")
		return videoSchema(strings.Join(parts, "\n"), map[string]any{"prompt": static["prompt"]})
	}
	caps := providerCall(p.Capabilities, func(m map[string]any) bool { return len(m) == 0 }, map[string]any{})
	models := providerCall(p.ListModels, func(m []map[string]any) bool { return len(m) == 0 }, nil)
	activeModel := configuredModel
	if activeModel == "" {
		activeModel = p.DefaultModel()
	}
	modelMeta := map[string]any{}
	for _, m := range models {
		if m != nil && pyrepr.Equal(m["id"], activeModel) {
			modelMeta = m
			break
		}
	}
	modelModalities := map[string]bool{}
	for _, m := range stringItems(modelMeta["modalities"]) {
		modelModalities[m] = true
	}
	if mod, ok := modelMeta["modality"].(string); ok && mod != "" {
		modelModalities[mod] = true
	}
	if modelModalities["image"] && !modelModalities["text"] {
		parts = append(parts, "- this model is image-to-video only — image_url is REQUIRED; text-only calls will be rejected")
	} else if modelModalities["text"] && !modelModalities["image"] {
		parts = append(parts, "- this model is text-to-video only — image_url is not supported")
	}
	effective := modelModalities
	if len(effective) == 0 {
		effective = map[string]bool{}
		for _, m := range stringItems(caps["modalities"]) {
			effective[m] = true
		}
	}
	canI2V, t2v := effective["image"], effective["text"]
	if canI2V && !t2v {
		parts = append(parts, "- image-to-video only: image_url is REQUIRED")
	} else if !canI2V {
		parts = append(parts, "- text-to-video only (no image input)")
	}
	if p.Name() == "xai" {
		parts = append(parts, "- chaining: for edit/extend pass the public HTTPS MP4 in `video` "+
			"or `public_url` from the prior Imagine result (files-cdn). For "+
			"image-to-video / reference-to-video pass public image URLs the "+
			"same way")
		if notice := videoXAIHTTP("", "", func() string { return xaiStorageNoticeText(env, "video_gen") }); notice != "" {
			parts = append(parts, "- storage: "+notice)
		}
	}
	properties := map[string]any{"prompt": static["prompt"]}
	if canI2V {
		properties["image_url"] = map[string]any{"type": "string",
			"description": "Public HTTPS URL of a still image to animate (image-to-video). Omit for text-to-video."}
		maxRefs := 0
		if pyTruthy(caps["max_reference_images"]) {
			if n, err := pyint.Of(caps["max_reference_images"]); err == nil {
				maxRefs = int(n.Int64())
			}
		}
		if maxRefs > 0 {
			properties["reference_image_urls"] = map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "maxItems": maxRefs,
				"description": fmt.Sprintf("Up to %d public HTTPS reference image URLs (style or character refs).", maxRefs)}
		}
	}
	metaOr := func(key string) any {
		if v, ok := modelMeta[key]; ok {
			return v
		}
		return caps[key]
	}
	minDuration, maxDuration := metaOr("min_duration"), metaOr("max_duration")
	durationParam := copyParam(static["duration"].(map[string]any))
	if pyTruthy(minDuration) && pyTruthy(maxDuration) {
		lo, err1 := pyint.Of(minDuration)
		hi, err2 := pyint.Of(maxDuration)
		if err1 == nil && err2 == nil {
			durationParam["minimum"] = pyint.Native(lo)
			durationParam["maximum"] = pyint.Native(hi)
			durationParam["description"] = fmt.Sprintf("Video duration in seconds (%s-%s). Omit for the provider default.",
				pyrepr.Str(minDuration), pyrepr.Str(maxDuration))
		}
	}
	properties["duration"] = durationParam
	for _, kv := range [][2]string{{"aspect_ratio", "aspect_ratios"}, {"resolution", "resolutions"}} {
		param := copyParam(static[kv[0]].(map[string]any))
		if pyTruthy(caps[kv[1]]) {
			if list, ok := caps[kv[1]].([]any); ok {
				param["enum"] = append([]any(nil), list...)
			} else if list, ok := caps[kv[1]].([]string); ok {
				param["enum"] = stringsToAny(list)
			}
		}
		properties[kv[0]] = param
	}
	for _, cp := range videoCapabilityParams {
		if pyTruthy(caps[cp.flag]) {
			properties[cp.key] = cp.param
		}
	}
	if pyTruthy(caps["audio_always_on"]) && !pyTruthy(caps["supports_audio"]) {
		parts = append(parts, "- audio: native stereo audio is generated with every video "+
			"(always on; no toggle) — describe the desired sound in the prompt")
	}
	return videoSchema(strings.Join(parts, "\n"), properties)
}

// Whether video_gen.provider is exactly xai in the profile config
func configuredForXAIVideo(ctx context.Context) bool {
	return videoGenConfig(ctx)["provider"] == "xai"
}

// Edit and extend tools require the xai provider configured and xAI credentials
func checkXAIVideoRequirements(env *tools.Env) bool {
	return configuredForXAIVideo(envContext(context.Background(), env)) && hasXAIVideoCredentials(env)
}

// Stripped non empty string, empty for anything else
func cleanString(v any) string {
	s, _ := v.(string)
	return strings.TrimSpace(s)
}

// int of the value with bools rejected so duration true never becomes one
func xaiCoerceInt(value any) *int {
	if value == nil {
		return nil
	}
	if _, ok := value.(bool); ok {
		return nil
	}
	n, err := pyint.Of(value)
	if err != nil || !n.IsInt64() {
		return nil
	}
	i := int(n.Int64())
	return &i
}

var xaiVideoURLParam = map[string]any{"type": "string",
	"description": "Public HTTPS MP4 URL of the source video — the `video` or `public_url` from a prior xAI Imagine result."}

func xaiVideoSchema(name, verb, noun, promptVerb string, extra map[string]any) map[string]any {
	props := map[string]any{
		"prompt":    map[string]any{"type": "string", "description": fmt.Sprintf("Instruction for how xAI should %s the source video.", promptVerb)},
		"video_url": xaiVideoURLParam,
	}
	for k, v := range extra {
		props[k] = v
	}
	return map[string]any{
		"name": name,
		"description": fmt.Sprintf("%s an existing video with xAI Imagine. This is separate from "+
			"`video_generate` because video %s is provider-specific. "+
			"`video_url` must be the public HTTPS MP4 URL from a prior Imagine "+
			"result (`video` or `public_url` on files-cdn).", verb, noun),
		"parameters": map[string]any{"type": "object", "properties": props, "required": []any{"prompt", "video_url"}},
	}
}

func xaiVideoEditSchema() map[string]any {
	return xaiVideoSchema("xai_video_edit", "Edit", "editing", "modify", nil)
}

func xaiVideoExtendSchema() map[string]any {
	return xaiVideoSchema("xai_video_extend", "Extend", "extension", "continue", map[string]any{
		"duration": map[string]any{"type": "integer", "description": "Desired extension duration in seconds. xAI clamps this to its supported range."},
	})
}

// Validates prompt, public video URL and the xai provider selection before running the operation
func runXAIVideoTool(ctx context.Context, env *tools.Env, args map[string]any, op string, run func(prompt, videoURL string) map[string]any) string {
	prompt, videoURL := cleanString(args["prompt"]), cleanString(args["video_url"])
	if prompt == "" {
		return tools.ErrorResult(fmt.Errorf("prompt is required for xAI video %s", op))
	}
	if videoURL == "" || !hasLowerPrefix(videoURL, "http://", "https://") {
		return tools.ErrorResult(errors.New("video_url must be a public HTTPS MP4 URL (the `video`/`public_url` from a prior Imagine result)"))
	}
	if !configuredForXAIVideo(envContext(ctx, env)) {
		return dumpOrdered([]string{"success", "error", "error_type", "provider"}, map[string]any{"success": false,
			"error":      "xAI video edit/extend tools require `video_gen.provider` to be configured as `xai` via `icebath tools` -> Video Generation.",
			"error_type": "provider_not_configured", "provider": "xai"})
	}
	out, err := encodeVideoResult(run(prompt, videoURL))
	if err != nil {
		return videoHandlerFailure(env, "xai_video_"+op, err)
	}
	return out
}

func handleXAIVideoEdit(ctx context.Context, call *tools.Call) string {
	return runXAIVideoTool(ctx, call.Env, call.Args, "edit", func(prompt, videoURL string) map[string]any {
		return runXAIVideoEdit(ctx, call.Env, prompt, videoURL, "")
	})
}

func handleXAIVideoExtend(ctx context.Context, call *tools.Call) string {
	duration := xaiCoerceInt(call.Args["duration"])
	return runXAIVideoTool(ctx, call.Env, call.Args, "extend", func(prompt, videoURL string) map[string]any {
		return runXAIVideoExtend(ctx, call.Env, prompt, videoURL, duration, "")
	})
}

// Registers video_generate with its capability driven schema plus the xAI edit and extend tools
func registerVideoGenerateTools() {
	tools.Register(tools.Def{
		Name: "video_generate", Toolset: "video_gen", Schema: videoGenerateSchema(), Handler: handleVideoGenerate,
		Check: checkVideoGenerationRequirements, RequiresEnv: []string{}, Emoji: "🎬", DynamicSchema: buildDynamicVideoSchema,
	})
	for _, d := range []struct {
		name    string
		schema  map[string]any
		handler tools.Handler
	}{{"xai_video_edit", xaiVideoEditSchema(), handleXAIVideoEdit}, {"xai_video_extend", xaiVideoExtendSchema(), handleXAIVideoExtend}} {
		tools.Register(tools.Def{Name: d.name, Toolset: "video_gen", Schema: d.schema, Handler: d.handler,
			Check: checkXAIVideoRequirements, RequiresEnv: []string{}, Emoji: "video"})
	}
}
