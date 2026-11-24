package media

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/tools"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

var containerHomeEnvTypes = map[string]bool{"docker": true, "modal": true}

var cacheBaseByBackend = map[string]string{"docker": "/root/.icebath", "modal": "/root/.icebath", "ssh": "~/.icebath"}

// Environment that exposes its own agent visible cache root
type agentVisibleCacheBaser interface {
	AgentVisibleCacheBase() (string, error)
}

// Environment that knows the remote user home it connected to
type remoteHomeEnvironment interface {
	RemoteHome() (home string, detected bool)
}

// Absolute POSIX or drive letter path that is not a URL or data URL
func looksLikeAbsoluteFilePath(value string) bool {
	l := strings.ToLower(value)
	if value == "" || strings.HasPrefix(l, "http://") || strings.HasPrefix(l, "https://") || strings.HasPrefix(l, "data:") {
		return false
	}
	if filepath.IsAbs(value) || strings.HasPrefix(value, "/") {
		return true
	}
	return len(value) >= 3 && value[1] == ':' && (value[2] == '/' || value[2] == '\\')
}

// Active terminal environment of the task, nil when none is live or the lookup fails
func activeTerminalEnv(env *tools.Env, taskID string) tools.Environment {
	if taskID == "" {
		taskID = "default"
	}
	e, err := tools.GetActiveEnv(env, taskID)
	if err != nil {
		logOf(env).Debug(fmt.Sprintf("Could not inspect active terminal environment: %s", err))
		return nil
	}
	return e
}

// Cache root the agent sees inside the terminal backend, empty for the local backend
func agentCacheBaseForEnv(env *tools.Env, e tools.Environment) (string, error) {
	if e != nil {
		if hook, ok := e.(agentVisibleCacheBaser); ok {
			value, err := hook.AgentVisibleCacheBase()
			if err != nil {
				logOf(env).Debug(fmt.Sprintf("active env agent_visible_cache_base failed: %s", err))
			} else if value != "" {
				return strings.TrimRight(value, "/"), nil
			}
		}
		if rh, ok := e.(remoteHomeEnvironment); ok {
			if h, _ := rh.RemoteHome(); h != "" {
				return strings.TrimRight(h, "/") + "/.icebath", nil
			}
		}
		if containerHomeEnvTypes[e.EnvType()] {
			return "/root/.icebath", nil
		}
	}
	backend, err := tools.TerminalEnvRead(env, "TERMINAL_ENV", "")
	if err != nil {
		return "", err
	}
	if backend == "" {
		backend = "local"
	}
	return cacheBaseByBackend[strings.ToLower(strings.TrimSpace(backend))], nil
}

// Adds host_image and agent_visible_image to a local success result under a non local backend
func postprocessImageGenerateResult(env *tools.Env, raw, taskID string) (string, error) {
	decoded, err := jsonutil.Decode([]byte(raw))
	if err != nil {
		return raw, nil
	}
	payload, ok := decoded.(*jsonutil.Object)
	if !ok {
		return raw, nil
	}
	if success, _ := payload.Get("success"); !pyTruthy(success) {
		return raw, nil
	}
	imageValue, _ := payload.Get("image")
	image, ok := imageValue.(string)
	if !ok || !looksLikeAbsoluteFilePath(image) {
		return raw, nil
	}
	e := activeTerminalEnvHook(env, taskID)
	cacheBase, err := agentCacheBaseForEnv(env, e)
	if err != nil {
		return "", err
	}
	if cacheBase == "" {
		return raw, nil
	}
	agentPath := tools.MapCachePathToContainer(env, image, cacheBase)
	if agentPath == "" || agentPath == image {
		return raw, nil
	}
	if e != nil {
		if sm := tools.EnvironmentSyncManager(e); sm != nil {
			func() {
				defer func() {
					if rec := recover(); rec != nil {
						logOf(env).Warn(fmt.Sprintf("Could not force-sync generated image artifact: %v", rec))
					}
				}()
				sm.Sync(true)
			}()
		}
	}
	if _, has := payload.Get("host_image"); !has {
		payload.Set("host_image", image)
	}
	if _, has := payload.Get("agent_visible_image"); !has {
		payload.Set("agent_visible_image", agentPath)
	}
	out, err := jsonutil.Encode(payload)
	if err != nil {
		return raw, nil
	}
	return out, nil
}

// Python truth value of a decoded JSON value
func pyTruthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case float64:
		return x != 0
	case int:
		return x != 0
	case int64:
		return x != 0
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	case *jsonutil.Object:
		return x.Len() > 0
	}
	return true
}

var ensurePluginsDiscoveredHook = func(ctx context.Context, force bool) {
	plugins.GetPluginManager(ctx).DiscoverAndLoad(ctx, force)
}

// Discovers plugins of the environment home then returns the named image provider, nil when absent
func getPluginProvider(ctx context.Context, env *tools.Env, name string, force bool) ImageGenProvider {
	ctx = envContext(ctx, env)
	ensurePluginsDiscoveredHook(ctx, force)
	return GetImageGenProvider(ctx, name, "")
}

var checkFalAPIKeyHook = checkFalAPIKey

// True when FAL or the explicitly configured image backend is available in the environment's profile home
func checkImageGenerationRequirements(env *tools.Env) bool {
	if checkFalAPIKeyHook(env) {
		loadFalClient()
		return true
	}
	configured := pluginProviderName(env)
	if configured == "" {
		return false
	}
	ctx := envContext(context.Background(), env)
	p := getPluginProvider(ctx, env, configured, false)
	return p != nil && p.AvailableIn(ctx)
}

// Static image_generate schema the dynamic overrides rebuild at definition time
func imageGenerateSchema() *jsonutil.Object {
	return jsonutil.ObjectOf(
		"name", "image_generate",
		"description", "Generate or edit images with the configured model. Edit/reference capabilities are rendered at serving time.",
		"parameters", jsonutil.ObjectOf(
			"type", "object",
			"properties", jsonutil.ObjectOf(
				"prompt", imagePromptParam(),
				"aspect_ratio", jsonutil.ObjectOf(
					"type", "string", "enum", []any{"landscape", "square", "portrait"},
					"description", "The aspect ratio of the generated image. 'landscape' is 16:9 wide, 'portrait' is 16:9 tall, 'square' is 1:1.",
					"default", defaultImageAspectRatio,
				),
			),
			"required", []any{"prompt"},
		),
	)
}

func imagePromptParam() *jsonutil.Object {
	return jsonutil.ObjectOf(
		"type", "string",
		"description", "The text prompt describing the desired image (text-to-image) or the edit to apply (image-to-image). Be detailed and descriptive.",
	)
}

// JSON error envelope shared by every provider dispatch failure path
func providerError(errText, errorType string) string {
	return dumpOrdered([]string{"success", "image", "error", "error_type"},
		map[string]any{"success": false, "image": nil, "error": errText, "error_type": errorType})
}

// JSON of a provider result map, a contract error for a nil result
func providerResult(result map[string]any, contractError string) string {
	if result == nil {
		return providerError(contractError, "provider_contract")
	}
	out, err := jsonutil.Encode(result)
	if err != nil {
		return providerError(contractError, "provider_contract")
	}
	return out
}

// Image request carrying the optional generate kwargs, edit sources only when supplied
func addProviderKwargs(req *ImageGenRequest, imageURL, referenceImageURLs any, upscale *bool, model string, controls map[string]any) {
	if req.Kwargs == nil {
		req.Kwargs = map[string]any{}
	}
	if model != "" {
		req.Kwargs["model"] = model
	}
	for k, v := range controls {
		req.Kwargs[k] = v
	}
	if s, ok := imageURL.(string); ok && strings.TrimSpace(s) != "" {
		req.ImageURL = strings.TrimSpace(s)
	}
	if referenceImageURLs != nil {
		if refs := normalizeReferenceImages(anyList(referenceImageURLs)); len(refs) > 0 {
			req.ReferenceImageURLs = refs
		}
	}
	if upscale != nil {
		req.Kwargs["upscale"] = *upscale
	}
}

// String lists as the []any shape normalizeReferenceImages reads
func anyList(v any) any {
	if ss, ok := v.([]string); ok {
		out := make([]any, len(ss))
		for i, s := range ss {
			out[i] = s
		}
		return out
	}
	return v
}

// Controls the provider declares in creative_controls, nil when none survive
func declaredControls(p ImageGenProvider, controls map[string]any) (out map[string]any) {
	if len(controls) == 0 {
		return nil
	}
	defer func() {
		if recover() != nil {
			out = nil
		}
	}()
	declared := map[string]bool{}
	for _, name := range stringList(p.Capabilities()["creative_controls"]) {
		declared[name] = true
	}
	out = map[string]any{}
	for name, value := range controls {
		if declared[name] {
			out[name] = value
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// Python str of a provider exception, the remote message for a plugin host raise
func excStr(err error) string {
	var hb *plugins.HostBuiltinError
	if errors.As(err, &hb) {
		return hb.Message
	}
	var he *plugins.PluginHostError
	if errors.As(err, &he) {
		return he.RemoteMessage
	}
	return err.Error()
}

// Whether a provider exception is a Python TypeError
func isPyTypeError(err error) bool {
	var hb *plugins.HostBuiltinError
	if errors.As(err, &hb) {
		return hb.Type == "TypeError"
	}
	var he *plugins.PluginHostError
	if errors.As(err, &he) {
		return he.TypeName == "TypeError"
	}
	return pyErrorTypeName(err) == "TypeError"
}

// Runs generate turning a panic into a returned error the way a raising provider surfaces
func safeGenerate(ctx context.Context, env *tools.Env, p ImageGenProvider, req ImageGenRequest) (result map[string]any, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			if e, ok := rec.(error); ok {
				err = e
			} else {
				err = fmt.Errorf("%v", rec)
			}
		}
	}()
	return p.Generate(ctx, env, req)
}

// Display name of a provider, a question mark when it has no name
func providerLabel(p ImageGenProvider) string {
	if n := p.Name(); n != "" {
		return n
	}
	return "?"
}

// Result of the selected plugin provider, handled false to fall through to in tree FAL
func dispatchToPluginProvider(ctx context.Context, env *tools.Env, prompt, aspectRatio string, src imageSources, controls map[string]any) (string, bool) {
	configured := pluginProviderName(env)
	if configured == "" {
		return "", false
	}
	p := getPluginProvider(ctx, env, configured, false)
	if p == nil {
		p = getPluginProvider(ctx, env, configured, true)
	}
	if p == nil {
		return providerError(fmt.Sprintf("image_gen.provider='%s' is set but no plugin registered that name. "+
			"Run `icebath plugins list` to see available image gen backends.", configured), "provider_not_registered"), true
	}
	pname := providerLabel(p)
	req := ImageGenRequest{Prompt: prompt, AspectRatio: aspectRatio}
	addProviderKwargs(&req, src.ImageURL, src.ReferenceImageURLs, src.Upscale, readConfiguredImageModel(env), declaredControls(p, controls))
	result, err := safeGenerate(ctx, env, p, req)
	if err != nil {
		typeErr := isPyTypeError(err)
		if typeErr && (req.ImageURL != "" || len(req.ReferenceImageURLs) > 0) {
			logOf(env).Warn(fmt.Sprintf("image_gen provider '%s' rejected image-to-image kwargs (signature too narrow): %s", pname, excStr(err)))
			return providerError(fmt.Sprintf("Provider '%s' does not support image-to-image / editing (its generate() "+
				"signature is out of date with the image_generate schema). Omit image_url for "+
				"text-to-image, or pick a service that edits images in /settings → "+
				"Images & media.", pname), "modality_unsupported"), true
		}
		suffix := ""
		if typeErr {
			suffix = " TypeError"
		}
		logOf(env).Warn(fmt.Sprintf("Image gen provider '%s' raised%s: %s", pname, suffix, excStr(err)))
		return providerError(fmt.Sprintf("Provider '%s' error: %s", pname, excStr(err)), "provider_exception"), true
	}
	return providerResult(result, "Provider returned a non-dict result"), true
}

// Python class name of an image source refusal
func imageResolutionTypeName(e *tools.ImageResolutionError) string {
	switch e.Kind {
	case tools.ImageUnsupportedScheme:
		return "UnsupportedScheme"
	case tools.ImageSourceUnsafe:
		return "SourceUnsafe"
	case tools.ImageSourceTooLarge:
		return "SourceTooLarge"
	case tools.ImageSourceNotFound:
		return "SourceNotFound"
	case tools.ImageNotAnImage:
		return "NotAnImage"
	}
	return "ImageResolutionError"
}

// Source arguments every dispatch route receives
type imageSources struct {
	ImageURL           any
	ReferenceImageURLs any
	Upscale            *bool
}

// Path like sources resolved to data URLs under a non local terminal backend, an error envelope on refusal
func confineSourceImages(ctx context.Context, env *tools.Env, imageURL, referenceImageURLs any, taskID string, permitted ...string) (any, any, string, error) {
	if len(permitted) == 0 {
		permitted = []string{"image"}
	}
	backend, err := tools.TerminalEnvRead(env, "TERMINAL_ENV", "")
	if err != nil {
		return imageURL, referenceImageURLs, "", err
	}
	if backend == "" {
		backend = "local"
	}
	if b := strings.ToLower(strings.TrimSpace(backend)); b == "" || b == "local" {
		return imageURL, referenceImageURLs, "", nil
	}
	resolve := func(ref string) (string, error) {
		return tools.ResolveLocalSourceToDataURL(ctx, env, ref, taskID, permitted...)
	}
	refusal := func(err error) (string, error) {
		var ire *tools.ImageResolutionError
		if errors.As(err, &ire) {
			return providerError("Could not read source image: "+ire.Error(), imageResolutionTypeName(ire)), nil
		}
		return "", err
	}
	if s, ok := imageURL.(string); ok && strings.TrimSpace(s) != "" {
		resolved, err := resolve(s)
		if err != nil {
			msg, ferr := refusal(err)
			return imageURL, referenceImageURLs, msg, ferr
		}
		imageURL = resolved
	}
	var refs []any
	switch r := referenceImageURLs.(type) {
	case []any:
		refs = r
	case []string:
		refs = anyList(r).([]any)
	default:
		return imageURL, referenceImageURLs, "", nil
	}
	out := make([]any, len(refs))
	for i, r := range refs {
		s, ok := r.(string)
		if !ok {
			out[i] = r
			continue
		}
		resolved, err := resolve(s)
		if err != nil {
			msg, ferr := refusal(err)
			return imageURL, referenceImageURLs, msg, ferr
		}
		out[i] = resolved
	}
	return imageURL, out, "", nil
}

var (
	activeTerminalEnvHook        = activeTerminalEnv
	dispatchToPluginProviderHook = dispatchToPluginProvider
	imageGenerateToolHook        = imageGenerateTool
)

// Tool error JSON of an exception the registry dispatch would have caught
func imageHandlerFailure(env *tools.Env, err error) string {
	logOf(env).Error(fmt.Sprintf("Tool image_generate dispatch error: %s", err))
	return tools.ErrorResult(errors.New(tools.SanitizeToolError(fmt.Sprintf("Tool execution failed: %s: %s", pyErrorTypeName(err), err))))
}

// image_generate handler confining sources, then plugin and FAL dispatch in that order
func handleImageGenerate(ctx context.Context, call *tools.Call) string {
	env, args := call.Env, call.Args
	prompt, _ := args["prompt"].(string)
	if pv, ok := args["prompt"]; ok && pv != nil && prompt == "" {
		prompt = fmt.Sprint(pv)
	}
	if prompt == "" {
		return tools.ErrorResult(errors.New("prompt is required for image generation"))
	}
	aspectRatio := defaultImageAspectRatio
	if v, ok := args["aspect_ratio"]; ok {
		aspectRatio, _ = v.(string)
	}
	imageURL, refs, confineErr, err := confineSourceImages(ctx, env, args["image_url"], args["reference_image_urls"], call.TaskID)
	if err != nil {
		return imageHandlerFailure(env, err)
	}
	if confineErr != "" {
		return confineErr
	}
	src := imageSources{ImageURL: imageURL, ReferenceImageURLs: refs}
	if b, ok := args["upscale"].(bool); ok {
		src.Upscale = &b
	}
	var controls map[string]any
	for _, name := range creativeControlOrder {
		if v, ok := args[name]; ok {
			if controls == nil {
				controls = map[string]any{}
			}
			controls[name] = v
		}
	}
	raw, handled := dispatchToPluginProviderHook(ctx, env, prompt, aspectRatio, src, controls)
	if !handled {
		raw = imageGenerateToolHook(ctx, env, imageToolArgs{Prompt: prompt, AspectRatio: aspectRatio,
			ImageURL: src.ImageURL, ReferenceImageURLs: src.ReferenceImageURLs, Upscale: optionalBool(src.Upscale)})
	}
	out, err := postprocessImageGenerateResult(env, raw, call.TaskID)
	if err != nil {
		return imageHandlerFailure(env, err)
	}
	return out
}

// Python str of a decoded JSON scalar
func pyStrOf(v any) string {
	if v == nil {
		return "None"
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

// Capability record advertised when the active backend declares nothing
func noImageCapabilities() map[string]any {
	return map[string]any{"modalities": []any{"text"}, "max_reference_images": 0, "supports_upscale": false}
}

// Capabilities of the active backend and model following runtime dispatch, undeclared ones absent
func activeImageCapabilities(env *tools.Env) map[string]any {
	info := noImageCapabilities()
	configured := readConfiguredImageProvider(env)
	if configured != "" && configured != "fal" && pluginCapabilities(env, configured, info) {
		return info
	}
	modelID, meta := resolveFalModel(env)
	_, canEdit := meta["edit_endpoint"].(string)
	canEdit = canEdit && meta["edit_endpoint"] != ""
	info["provider"] = "FAL.ai"
	if display, ok := meta["display"]; ok {
		info["model"] = display
	} else {
		info["model"] = modelID
	}
	if canEdit {
		info["modalities"] = []any{"text", "image"}
		maxRefs := positiveInt(meta["max_reference_images"])
		if maxRefs == 0 {
			maxRefs = 1
		}
		info["max_reference_images"] = maxRefs
	} else {
		info["modalities"] = []any{"text"}
		info["max_reference_images"] = 0
	}
	info["supports_upscale"] = true
	return info
}

// Fills info from the named plugin's declared capabilities, false when the plugin is absent or fails
func pluginCapabilities(env *tools.Env, pluginName string, info map[string]any) (ok bool) {
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	p := getPluginProvider(context.Background(), env, pluginName, false)
	if p == nil {
		return false
	}
	caps := func() (c map[string]any) {
		defer func() {
			if recover() != nil {
				c = map[string]any{}
			}
		}()
		return p.Capabilities()
	}()
	info["provider"] = p.DisplayName()
	model := readConfiguredImageModel(env)
	if model == "" {
		model = p.DefaultModel()
	}
	info["model"] = model
	if mods := stringList(caps["modalities"]); len(mods) > 0 {
		info["modalities"] = anyList(mods)
	}
	if n := capInt(caps["max_reference_images"]); n != 0 {
		info["max_reference_images"] = n
	}
	info["supports_upscale"] = pyTruthy(caps["supports_upscale"])
	if controls := stringList(caps["creative_controls"]); len(controls) > 0 {
		info["creative_controls"] = anyList(controls)
	}
	if role, _ := caps["source_image_role"].(string); role == "style" {
		info["source_image_role"] = "style"
	}
	return true
}

// Python int of a numeric capability value, zero for absent or non numeric
func capInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	case bool:
		if n {
			return 1
		}
	}
	return 0
}

func imageURLParam() *jsonutil.Object {
	return jsonutil.ObjectOf(
		"type", "string",
		"description", "Source image to edit/transform (image-to-image). A public URL or "+
			"an absolute local file path from the conversation. Omit for "+
			"text-to-image.",
	)
}

var creativeControlOrder = []string{"creativity", "thinking", "resolution", "intensity", "complexity", "movement"}

// Creative control vocabulary a provider opts into through creative_controls
func creativeControlParams() map[string]*jsonutil.Object {
	axis := func(desc string) *jsonutil.Object {
		return jsonutil.ObjectOf("type", "integer", "minimum", -100, "maximum", 100, "description", desc)
	}
	return map[string]*jsonutil.Object{
		"creativity": jsonutil.ObjectOf("type", "string", "enum", []any{"raw", "low", "medium", "high"},
			"description", "Prompt expansion: raw (none), low, medium or high."),
		"thinking": jsonutil.ObjectOf("type", "string", "enum", []any{"fast", "high"},
			"description", "How much the model plans before generating: fast (default) or high for complex multi-step edits."),
		"resolution": jsonutil.ObjectOf("type", "string", "enum", []any{"1K", "2K"},
			"description", "Output size: 1K (default) or 2K when small text or fine detail matters."),
		"intensity":  axis("Style intensity, -100 muted to 100 highly stylized. 0 neutral."),
		"complexity": axis("Composition density, -100 minimal to 100 dense. 0 neutral."),
		"movement":   axis("Motion in the scene, -100 static to 100 dynamic. 0 neutral."),
	}
}

func stylePromptParam() *jsonutil.Object {
	return jsonutil.ObjectOf(
		"type", "string",
		"description", "The subject and scene to generate. Be detailed and descriptive; "+
			"reference images supply only the look.",
	)
}

func styleImageURLParam() *jsonutil.Object {
	return jsonutil.ObjectOf(
		"type", "string",
		"description", "Style reference: its look is copied, the image itself is not edited. "+
			"A public URL or an absolute local file path. Describe the subject in "+
			"the prompt. Omit for text-to-image.",
	)
}

func upscaleParam() *jsonutil.Object {
	return jsonutil.ObjectOf(
		"type", "boolean",
		"description", "Post-generation high-resolution pass (~2x, extra cost/latency), "+
			"off by default. A creative enhancer that can alter fine detail "+
			"(rendered text, faces) — use only when resolution matters more "+
			"than fidelity.",
	)
}

// Description and parameters rendered from the active model capabilities, unsupported args omitted
func buildDynamicImageSchema(env *tools.Env) *jsonutil.Object {
	info := activeImageCapabilities(env)
	maxRefs := capInt(info["max_reference_images"])
	modalities := stringList(info["modalities"])
	if _, present := info["modalities"]; !present || len(modalities) == 0 {
		modalities = []string{"text"}
	}
	canEdit := containsString(modalities, "image")
	static := imageGenerateSchema().Value("parameters").(*jsonutil.Object).Value("properties").(*jsonutil.Object)
	properties := jsonutil.ObjectOf("prompt", static.Value("prompt"), "aspect_ratio", static.Value("aspect_ratio"))
	styleRefs := info["source_image_role"] == "style"
	var lead, editClause string
	if canEdit {
		var refsDesc string
		if styleRefs {
			lead = "Generate images, optionally styled on reference images, via configured model."
			editClause = "Pass style references in image_url. "
			properties.Set("prompt", stylePromptParam())
			properties.Set("image_url", styleImageURLParam())
			refsDesc = fmt.Sprintf("Up to %d more style references. URLs or absolute local paths.", maxRefs)
		} else {
			lead = "Generate or edit images using the configured image model."
			if maxRefs > 1 {
				lead = "Generate or edit images (with reference images) using the configured model."
			}
			editClause = "Pass image_url to edit / transform an existing image. "
			properties.Set("image_url", imageURLParam())
			refsDesc = fmt.Sprintf("Up to %d additional reference images (style, "+
				"character, or composition) guiding an edit. URLs or "+
				"absolute local paths.", maxRefs)
		}
		if maxRefs > 1 {
			properties.Set("reference_image_urls", jsonutil.ObjectOf(
				"type", "array", "items", jsonutil.ObjectOf("type", "string"), "maxItems", maxRefs, "description", refsDesc,
			))
		}
	} else {
		lead = "Generate images from text with the configured model (it cannot edit images)."
	}
	if pyTruthy(info["supports_upscale"]) {
		properties.Set("upscale", upscaleParam())
	}
	params := creativeControlParams()
	for _, name := range stringList(info["creative_controls"]) {
		if p, ok := params[name]; ok {
			properties.Set(name, p)
		}
	}
	desc := lead + " " + editClause + "Use this rather than calling image APIs or scripts directly. " +
		"Returns the result in the `image` field — a URL or an absolute " +
		"file path; reference it in your response using the current " +
		"platform's file-delivery convention."
	return jsonutil.ObjectOf(
		"description", desc,
		"parameters", jsonutil.ObjectOf("type", "object", "properties", properties, "required", []any{"prompt"}),
	)
}

// Registers image_generate in the image_gen toolset with its capability driven schema
func registerImageGenerateTool() {
	tools.Register(tools.Def{
		Name: "image_generate", Toolset: "image_gen", Schema: imageGenerateSchema(),
		Handler: handleImageGenerate, Check: checkImageGenerationRequirements, RequiresEnv: []string{},
		Emoji: "🎨", DynamicSchema: buildDynamicImageSchema,
	})
}

// Python None for a nil pointer, the bool otherwise
func optionalBool(p *bool) any {
	if p == nil {
		return nil
	}
	return *p
}
