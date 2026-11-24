package media

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/settings"
	"github.com/nickheyer/icebath/pkg/tools"
)

var (
	imageFalClient   *falClientModule
	imageFalClientMu sync.Mutex
	imageDebugMu     sync.Mutex
	imageDebug       *tools.DebugSession
)

// FAL client module loaded once, keeping a test installed module
func loadFalClient() *falClientModule {
	imageFalClientMu.Lock()
	defer imageFalClientMu.Unlock()
	if imageFalClient == nil {
		imageFalClient = importFalClient()
	}
	return imageFalClient
}

// Debug session of the image tools gated by IMAGE_TOOLS_DEBUG
func imageDebugSession(env *tools.Env) *tools.DebugSession {
	imageDebugMu.Lock()
	defer imageDebugMu.Unlock()
	if imageDebug == nil {
		imageDebug = tools.NewDebugSession(env, "image_tools", "IMAGE_TOOLS_DEBUG")
	}
	return imageDebug
}

// Python ValueError raised by the image pipeline
type imageValueError struct{ msg string }

func (e *imageValueError) Error() string      { return e.msg }
func (e *imageValueError) PyTypeName() string { return "ValueError" }

// Raised when the user interrupts while a FAL job is in flight
type imageGenerationInterrupted struct{ msg string }

func (e *imageGenerationInterrupted) Error() string      { return e.msg }
func (e *imageGenerationInterrupted) PyTypeName() string { return "ImageGenerationInterrupted" }

func (e *falClientHTTPError) PyTypeName() string      { return "FalClientHTTPError" }
func (falMissingCredentialsError) PyTypeName() string { return "MissingCredentialsError" }

// Python exception class name of an error for the error_type field
func pyErrorTypeName(err error) string {
	var named interface{ PyTypeName() string }
	if errors.As(err, &named) {
		return named.PyTypeName()
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		if ue.Timeout() {
			return "ReadTimeout"
		}
		return "ConnectError"
	}
	return "ValueError"
}

// Waits for the FAL result while polling the interrupt bit, abandoning the worker on interrupt
func waitFalResult(ctx context.Context, h *falRequestHandle, pollSeconds float64) (any, error) {
	if pollSeconds <= 0 {
		pollSeconds = 0.5
	}
	var result any
	var werr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		result, werr = waitFalQueueResult(ctx, h, falQueuePollSeconds)
	}()
	poll := time.Duration(pollSeconds * float64(time.Second))
	for {
		select {
		case <-done:
			return result, werr
		default:
		}
		if tools.IsInterrupted(ctx) {
			return nil, &imageGenerationInterrupted{"Image generation interrupted by user — abandoned the in-flight FAL job."}
		}
		select {
		case <-done:
			return result, werr
		case <-time.After(poll):
		}
	}
}

// Submits to the FAL queue with the profile's FAL key and a fresh idempotency key
func submitFalRequest(ctx context.Context, env *tools.Env, model string, arguments map[string]any) (*falRequestHandle, error) {
	return loadFalClient().Submit(ctx, env, model, arguments, map[string]string{"x-idempotency-key": uuid4()})
}

// Error naming why FAL cannot serve image generation, nil when FAL_KEY is configured
func falKeyMissingError(env *tools.Env) error {
	if tools.FalKeyIsConfigured(env) {
		return nil
	}
	if selected := tools.ReadSelection(env, "image_gen"); selected != "" {
		return &imageValueError{tools.SelectionError("image_gen", selected, "FAL_KEY is not set")}
	}
	return &imageValueError{buildNoBackendSetupMessage()}
}

// Image model the profile chose, empty when unset
func readConfiguredImageModel(env *tools.Env) string {
	return strings.TrimSpace(settings.ImageGenModel.Get(tools.WithEnvHome(context.Background(), env)))
}

// Image service the profile chose, empty when none was chosen
func readConfiguredImageProvider(env *tools.Env) string {
	return tools.StoredProvider(env, "image_gen")
}

// Configured provider that dispatches through the plugin registry, empty for unset and fal
func pluginProviderName(env *tools.Env) string {
	configured := readConfiguredImageProvider(env)
	if configured == "" || configured == "fal" {
		return ""
	}
	return configured
}

// Configured FAL model, the default with a warning when unknown
func resolveFalModel(env *tools.Env) (string, map[string]any) {
	modelID := readConfiguredImageModel(env)
	if _, ok := falModels[modelID]; modelID != "" && !ok {
		logOf(env).Warn(fmt.Sprintf("Unknown FAL model '%s' in config; falling back to %s", modelID, falDefaultModel))
		modelID = ""
	}
	if modelID == "" {
		modelID = falDefaultModel
	}
	return modelID, falModels[modelID]
}

var falSizeKeyByStyle = map[string]string{"image_size_preset": "image_size", "gpt_literal": "image_size", "aspect_ratio": "aspect_ratio"}

// Defaults plus native size plus overrides filtered to the model whitelist, edit mode when imageURLs is set
func buildFalRequestPayload(modelID, prompt, aspectRatio string, seed any, overrides map[string]any, imageURLs []string, edit bool) (map[string]any, error) {
	meta := falModels[modelID]
	supports, _ := meta["supports"].(map[string]bool)
	if edit {
		supports, _ = meta["edit_supports"].(map[string]bool)
	}
	sizes, _ := meta["sizes"].(map[string]any)
	aspect := aspectRatio
	if aspect == "" {
		aspect = defaultImageAspectRatio
	}
	aspect = strings.TrimSpace(strings.ToLower(aspect))
	if _, ok := sizes[aspect]; !ok {
		aspect = defaultImageAspectRatio
	}
	payload := map[string]any{}
	if defaults, ok := meta["defaults"].(map[string]any); ok {
		for k, v := range defaults {
			payload[k] = v
		}
	}
	payload["prompt"] = strings.TrimSpace(prompt)
	required := map[string]bool{"prompt": true}
	if edit {
		param, _ := meta["edit_image_param"].(string)
		if param == "" {
			param = "image_urls"
		}
		if param != "image_urls" {
			first := ""
			if len(imageURLs) > 0 {
				first = imageURLs[0]
			}
			payload[param] = first
		} else {
			list := make([]any, len(imageURLs))
			for i, u := range imageURLs {
				list[i] = u
			}
			payload[param] = list
		}
		required[param] = true
	}
	style, _ := meta["size_style"].(string)
	sizeKey, known := falSizeKeyByStyle[style]
	if !known && !edit {
		return nil, &imageValueError{"Unknown size_style: " + pyjsonRepr(style)}
	}
	if known && (!edit || supports[sizeKey]) {
		payload[sizeKey] = sizes[aspect]
	}
	if pyIsInt(seed) {
		payload["seed"] = seed
	}
	for k, v := range overrides {
		if v != nil {
			payload[k] = v
		}
	}
	out := map[string]any{}
	for k, v := range payload {
		if supports[k] || required[k] {
			out[k] = v
		}
	}
	return out, nil
}

// Python isinstance int test, bool included
func pyIsInt(v any) bool {
	if _, ok := v.(bool); ok {
		return true
	}
	_, ok := pyrepr.Int(v)
	return ok
}

// Python repr of a string for error text
func pyjsonRepr(s string) string { return "'" + strings.ReplaceAll(s, "'", "\\'") + "'" }

// FAL text to image payload from the unified inputs
func buildFalPayload(modelID, prompt, aspectRatio string, seed any, overrides map[string]any) (map[string]any, error) {
	return buildFalRequestPayload(modelID, prompt, aspectRatio, seed, overrides, nil, false)
}

// FAL edit payload with the source images, filtered to edit_supports
func buildFalEditPayload(modelID, prompt string, imageURLs []string, aspectRatio string, seed any, overrides map[string]any) (map[string]any, error) {
	return buildFalRequestPayload(modelID, prompt, aspectRatio, seed, overrides, imageURLs, true)
}

// Clarity upscaler result row, nil on any failure except an interrupt
func upscaleImage(ctx context.Context, env *tools.Env, imageURL, originalPrompt string) (map[string]any, error) {
	log := logOf(env)
	log.Info("Upscaling image with Clarity Upscaler...")
	h, err := submitFalRequest(ctx, env, upscalerModel, map[string]any{
		"image_url": imageURL, "prompt": upscalerDefaultPrompt + ", " + originalPrompt,
		"upscale_factor": upscalerFactor, "negative_prompt": upscalerNegativePrompt,
		"creativity": upscalerCreativity, "resemblance": upscalerResemblance,
		"guidance_scale": upscalerGuidanceScale, "num_inference_steps": upscalerNumInferenceSteps,
		"enable_safety_checker": upscalerSafetyChecker})
	var result any
	if err == nil {
		result, err = waitFalResult(ctx, h, 0.5)
	}
	var intr *imageGenerationInterrupted
	if errors.As(err, &intr) {
		return nil, err
	}
	if err != nil {
		log.Error(fmt.Sprintf("Error upscaling image: %v", err))
		return nil, nil
	}
	res, _ := result.(map[string]any)
	up, ok := res["image"]
	if !ok {
		log.Error("Upscaler returned invalid response")
		return nil, nil
	}
	upm, _ := up.(map[string]any)
	u, ok := upm["url"]
	if !ok {
		log.Error("Error upscaling image: the upscale reply has no url")
		return nil, nil
	}
	width, height := any(0), any(0)
	if w, ok := upm["width"]; ok {
		width = w
	}
	if hh, ok := upm["height"]; ok {
		height = hh
	}
	log.Info(fmt.Sprintf("Image upscaled successfully to %vx%v", orUnknown(upm, "width"), orUnknown(upm, "height")))
	return map[string]any{"url": u, "width": width, "height": height, "upscaled": true, "upscale_factor": upscalerFactor}, nil
}

// Value of a key or the word unknown for log text
func orUnknown(m map[string]any, k string) any {
	if v, ok := m[k]; ok {
		return v
	}
	return "unknown"
}

// URL rows of the generated images, upscaled when asked and the native image when the upscale fails
func formatImages(ctx context.Context, env *tools.Env, images []any, shouldUpscale bool, prompt string) ([]map[string]any, error) {
	var formatted []map[string]any
	for _, raw := range images {
		img, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		u, ok := img["url"]
		if !ok {
			continue
		}
		if shouldUpscale {
			us, _ := u.(string)
			up, err := upscaleImageHook(ctx, env, us, strings.TrimSpace(prompt))
			if err != nil {
				return nil, err
			}
			if up != nil {
				formatted = append(formatted, up)
				continue
			}
			logOf(env).Warn("Using original image as fallback (upscale failed)")
		}
		width, height := any(0), any(0)
		if w, ok := img["width"]; ok {
			width = w
		}
		if h, ok := img["height"]; ok {
			height = h
		}
		formatted = append(formatted, map[string]any{"url": u, "width": width, "height": height, "upscaled": false})
	}
	return formatted, nil
}

const falInlineBudget = 3_500_000

// Inlines local sources as data URLs through the guarded resolver and shrinks them to the inline budget
func inlineFalSources(ctx context.Context, env *tools.Env, sources []string) ([]string, error) {
	urls := make([]string, len(sources))
	for i, s := range sources {
		u, err := tools.ResolveLocalSourceToDataURL(ctx, env, s, "")
		if err != nil {
			var ire *tools.ImageResolutionError
			if errors.As(err, &ire) {
				return nil, &imageValueError{"Could not read source image: " + err.Error()}
			}
			return nil, err
		}
		urls[i] = u
	}
	total, inline := 0, 0
	for _, u := range urls {
		if strings.HasPrefix(u, "data:") {
			total += len(u)
			inline++
		}
	}
	if total <= falInlineBudget {
		return urls, nil
	}
	share := falInlineBudget / inline
	for i, u := range urls {
		if strings.HasPrefix(u, "data:") && len(u) > share {
			urls[i] = shrinkDataURL(env, u, share)
		}
	}
	return urls, nil
}

// Validated FAL endpoint and payload for a text to image or edit request
func prepareFalRequest(ctx context.Context, env *tools.Env, modelID string, meta map[string]any, prompt, aspectRatio string, seed any, overrides map[string]any, sources []string) (string, map[string]any, error) {
	if strings.TrimSpace(prompt) == "" {
		return "", nil, &imageValueError{"Prompt is required and must be a non-empty string"}
	}
	if err := falKeyMissingError(env); err != nil {
		return "", nil, err
	}
	editEndpoint, _ := meta["edit_endpoint"].(string)
	display, ok := meta["display"].(string)
	if !ok {
		display = modelID
	}
	if len(sources) > 0 && editEndpoint == "" {
		return "", nil, &imageValueError{fmt.Sprintf("Model '%s' (%s) is not capable of image-to-image / editing. "+
			"Provide a text-only prompt (omit image_url), or switch to an edit-capable model "+
			"in /settings → Images & media.", display, modelID)}
	}
	aspect := aspectRatio
	if aspect == "" {
		aspect = defaultImageAspectRatio
	}
	aspect = strings.TrimSpace(strings.ToLower(aspect))
	if !containsString(imageAspectRatios, aspect) {
		logOf(env).Warn(fmt.Sprintf("Invalid aspect_ratio '%s', defaulting to '%s'", aspectRatio, defaultImageAspectRatio))
		aspect = defaultImageAspectRatio
	}
	if len(sources) > 0 {
		maxRefs, _ := meta["max_reference_images"].(int)
		if maxRefs == 0 {
			maxRefs = 1
		}
		picked := sources
		if maxRefs > 0 && len(sources) > maxRefs {
			picked = sources[:maxRefs]
		}
		clamped, err := inlineFalSources(ctx, env, picked)
		if err != nil {
			return "", nil, err
		}
		args, err := buildFalEditPayload(modelID, prompt, clamped, aspect, seed, overrides)
		if err != nil {
			return "", nil, err
		}
		logOf(env).Info(fmt.Sprintf("Editing image with %s (%s) — %d source image(s), prompt: %s", display, editEndpoint, len(clamped), clipRunes(prompt, 80)))
		return editEndpoint, args, nil
	}
	args, err := buildFalPayload(modelID, prompt, aspect, seed, overrides)
	if err != nil {
		return "", nil, err
	}
	logOf(env).Info(fmt.Sprintf("Generating image with %s (%s) — prompt: %s", display, modelID, clipRunes(prompt, 80)))
	return modelID, args, nil
}

// Reports whether the list holds the string
func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// First n runes of a string like a Python slice
func clipRunes(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}

// Keyword arguments of the in tree FAL image_generate pipeline
type imageToolArgs struct {
	Prompt             string
	AspectRatio        string
	NumInferenceSteps  any
	GuidanceScale      any
	NumImages          any
	OutputFormat       any
	Seed               any
	ImageURL           any
	ReferenceImageURLs any
	Upscale            any
	FalModel           string
}

// Generates or edits an image through FAL and returns the indented JSON envelope
func imageGenerateTool(ctx context.Context, env *tools.Env, a imageToolArgs) string {
	modelID, meta := a.FalModel, falModels[a.FalModel]
	if meta == nil {
		modelID, meta = resolveFalModel(env)
	}
	var sources []string
	candidates := []any{a.ImageURL}
	switch refs := a.ReferenceImageURLs.(type) {
	case []any:
		candidates = append(candidates, refs...)
	case []string:
		for _, r := range refs {
			candidates = append(candidates, r)
		}
	}
	for _, c := range candidates {
		if s, ok := c.(string); ok && strings.TrimSpace(s) != "" {
			sources = append(sources, strings.TrimSpace(s))
		}
	}
	editEndpoint, _ := meta["edit_endpoint"].(string)
	useEdit := len(sources) > 0 && editEndpoint != ""
	modality := "text"
	if useEdit {
		modality = "image"
	}
	overrides := map[string]any{"num_inference_steps": a.NumInferenceSteps, "guidance_scale": a.GuidanceScale,
		"num_images": a.NumImages, "output_format": a.OutputFormat}
	params := map[string]any{"prompt": a.Prompt, "aspect_ratio": a.AspectRatio,
		"num_inference_steps": a.NumInferenceSteps, "guidance_scale": a.GuidanceScale, "num_images": a.NumImages,
		"output_format": a.OutputFormat, "seed": a.Seed, "modality": modality, "source_images": len(sources)}
	debugData := map[string]any{"model": modelID, "parameters": params, "error": nil, "success": false,
		"images_generated": 0, "generation_time": 0}
	start := time.Now()
	finish := func(keys []string, response map[string]any) string {
		debugData["generation_time"] = time.Since(start).Seconds()
		dbg := imageDebugSession(env)
		dbg.LogCall("image_generate_tool", debugData)
		dbg.Save(env)
		return dumpOrdered(keys, response, pyjson.Indent(2), pyjson.EnsureASCII(false))
	}
	fail := func(err error) string {
		msg := "Error generating image: " + err.Error()
		logOf(env).Error(msg)
		debugData["error"] = msg
		return finish([]string{"success", "image", "error", "error_type"}, map[string]any{"success": false, "image": nil, "error": err.Error(), "error_type": pyErrorTypeName(err)})
	}
	endpoint, arguments, err := prepareFalRequest(ctx, env, modelID, meta, a.Prompt, a.AspectRatio, a.Seed, overrides, sources)
	if err != nil {
		return fail(err)
	}
	h, err := submitFalRequestHook(ctx, env, endpoint, arguments)
	if err != nil {
		return fail(err)
	}
	raw, err := waitFalResult(ctx, h, 0.5)
	if err != nil {
		return fail(err)
	}
	result, _ := raw.(map[string]any)
	imagesRaw, ok := result["images"]
	if !ok {
		return fail(&imageValueError{"Invalid response from FAL.ai API — no images returned"})
	}
	images, _ := imagesRaw.([]any)
	if len(images) == 0 {
		return fail(&imageValueError{"No images were generated"})
	}
	shouldUpscale := false
	if a.Upscale != nil {
		shouldUpscale = pyrepr.Truthy(a.Upscale)
	} else {
		up, _ := meta["upscale"].(bool)
		shouldUpscale = up && !useEdit
	}
	formatted, err := formatImages(ctx, env, images, shouldUpscale, a.Prompt)
	if err != nil {
		return fail(err)
	}
	if len(formatted) == 0 {
		return fail(&imageValueError{"No valid image URLs returned from API"})
	}
	upscaled := 0
	for _, img := range formatted {
		if img["upscaled"] == true {
			upscaled++
		}
	}
	logOf(env).Info(fmt.Sprintf("Generated %d image(s) in %.1fs (%d upscaled) via %s [%s]", len(formatted), time.Since(start).Seconds(), upscaled, endpoint, modality))
	debugData["success"] = true
	debugData["images_generated"] = len(formatted)
	return finish([]string{"success", "image", "modality", "upscaled"}, map[string]any{"success": true, "image": formatted[0]["url"], "modality": modality, "upscaled": formatted[0]["upscaled"] == true})
}

// Reports whether FAL_KEY is configured for the profile
func checkFalAPIKey(env *tools.Env) bool { return tools.FalKeyIsConfigured(env) }

// Setup guidance when FAL_KEY is not set and no other image backend is configured
func buildNoBackendSetupMessage() string {
	return strings.Join([]string{"Image generation is unavailable in this environment.", "", "Missing requirements:",
		"  - FAL_KEY environment variable is not set", "", "To enable image generation, do one of:",
		"  1. Get a free API key at https://fal.ai and set FAL_KEY=<your-key> (then restart the session)",
		"  2. Pick another image service in /settings → Images & media " +
			"(run `icebath plugins list` to see installed backends)"}, "\n")
}

// JSON text of the map with keys in the given insertion order
func dumpOrdered(keys []string, m map[string]any, opts ...pyjson.Option) string {
	o := pyjson.NewObject()
	for _, k := range keys {
		o.Set(k, m[k])
	}
	s, _ := pyjson.Dumps(o, opts...)
	return s
}

var (
	submitFalRequestHook = submitFalRequest
	upscaleImageHook     = upscaleImage
)
