package media

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/settings"
	"github.com/nickheyer/icebath/pkg/tools"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	pathutil "github.com/nickheyer/icebath/pkg/utils/path"
)

const (
	xaiImageDefaultModel    = "grok-imagine-image"
	xaiEditFallbackModel    = "grok-imagine-image-quality"
	xaiLiveCacheTTL         = 300 * time.Second
	xaiLiveTimeout          = 10 * time.Second
	xaiMaxSourceImages      = 3
	xaiImageMissingKeyError = "No xAI credentials found. Configure xAI OAuth in `icebath model` or set XAI_API_KEY."
)

var xaiImageModels = imageModelCatalog{order: []string{"grok-imagine-image", "grok-imagine-image-2.0", "grok-imagine-image-quality"},
	meta: map[string]map[string]any{
		"grok-imagine-image": {"display": "Grok Imagine Image", "speed": "~5-10s", "strengths": "Fast, high-quality"},
		"grok-imagine-image-2.0": {"display": "Grok Imagine Image 2.0", "speed": "~10-20s",
			"strengths": "Typography/layout-aware; legible small text; strongest quality."},
		"grok-imagine-image-quality": {"display": "Grok Imagine Image (Quality)", "speed": "~10-20s",
			"strengths": "Higher fidelity / detail; slower than the standard model."},
	}}

var xaiAspectRatios = map[string]string{"landscape": "16:9", "square": "1:1", "portrait": "9:16",
	"4:3": "4:3", "3:4": "3:4", "3:2": "3:2", "2:3": "2:3"}

var xaiRemotePrefixes = []string{"http://", "https://", "data:"}

var xaiFileOutputExtraKeys = []string{"filename", "expires_at", "public_url_expires_at", "public_url_error", "storage_error"}

// Connect and read timeout of the images request
var xaiImageRequestTimeout = 120 * time.Second

// Bearer credentials of the direct xAI HTTP endpoints
type xaiImageCreds struct{ Provider, APIKey, BaseURL string }

func resolveXAIImageCreds(env *tools.Env) xaiImageCreds {
	p, k, b := tools.ResolveXAIHTTPCredentials(env, false)
	return xaiImageCreds{Provider: p, APIKey: k, BaseURL: b}
}

// Stripped base URL without its trailing slash, the public xAI origin when unset
func xaiImageBaseURL(c xaiImageCreds) string {
	b := c.BaseURL
	if b == "" {
		b = DefaultXAIBaseURL
	}
	return strings.TrimRight(strings.TrimSpace(b), "/")
}

// Live model ids in endpoint order with their input modalities and aliases
type xaiLiveCatalog struct {
	order  []string
	models map[string]map[string]any
}

// Live image model catalog of the endpoint, an error when no key resolves or the request fails
var fetchXAILiveImageModels = func(ctx context.Context, env *tools.Env, creds *xaiImageCreds) (xaiLiveCatalog, error) {
	if creds == nil {
		c := resolveXAIImageCreds(env)
		creds = &c
	}
	apiKey := strings.TrimSpace(creds.APIKey)
	if apiKey == "" {
		return xaiLiveCatalog{}, fmt.Errorf("no xAI credentials")
	}
	payload, _, err := getJSON(ctx, xaiImageBaseURL(*creds)+"/image-generation-models",
		map[string]string{"Authorization": "Bearer " + apiKey, "User-Agent": tools.IcebathXAIUserAgent()}, xaiLiveTimeout)
	if err != nil {
		return xaiLiveCatalog{}, err
	}
	entries, _ := payload["models"].([]any)
	if len(entries) == 0 {
		entries, _ = payload["data"].([]any)
	}
	out := xaiLiveCatalog{models: map[string]map[string]any{}}
	for _, e := range entries {
		entry, ok := e.(map[string]any)
		if !ok {
			continue
		}
		id := entry["id"]
		if !anyutil.IsTrue(id) {
			id = entry["name"]
		}
		s, ok := id.(string)
		if !ok || strings.TrimSpace(s) == "" {
			continue
		}
		s = strings.TrimSpace(s)
		mods, aliases := entry["input_modalities"], entry["aliases"]
		if !anyutil.IsTrue(mods) {
			mods = []any{}
		}
		if !anyutil.IsTrue(aliases) {
			aliases = []any{}
		}
		if _, seen := out.models[s]; !seen {
			out.order = append(out.order, s)
		}
		out.models[s] = map[string]any{"input_modalities": mods, "aliases": aliases}
	}
	return out, nil
}

type xaiLiveCacheEntry struct {
	live xaiLiveCatalog
	at   time.Time
}

// Process wide and per credential live catalog caches with the five minute TTL
var xaiLiveCache struct {
	sync.Mutex
	process      *xaiLiveCacheEntry
	byCredential map[[2]string]xaiLiveCacheEntry
}

var xaiLiveNow = time.Now

// Live catalog or an empty one when the endpoint is unreachable, the failure only logged
func fetchXAILiveImageModelsOrEmpty(ctx context.Context, env *tools.Env, creds *xaiImageCreds) xaiLiveCatalog {
	live, err := fetchXAILiveImageModels(ctx, env, creds)
	if err != nil {
		pkgLog().Debug(fmt.Sprintf("xAI live image model catalog unavailable: %v", err))
		return xaiLiveCatalog{}
	}
	return live
}

// Cached live catalog, keyed by base URL and key fingerprint when a profile home override is active
func xaiLiveImageModels(ctx context.Context, env *tools.Env) xaiLiveCatalog {
	if home.Override(ctx) == "" {
		xaiLiveCache.Lock()
		cached := xaiLiveCache.process
		xaiLiveCache.Unlock()
		if cached != nil && xaiLiveNow().Sub(cached.at) < xaiLiveCacheTTL {
			return cached.live
		}
		live := fetchXAILiveImageModelsOrEmpty(ctx, env, nil)
		xaiLiveCache.Lock()
		xaiLiveCache.process = &xaiLiveCacheEntry{live, xaiLiveNow()}
		xaiLiveCache.Unlock()
		return live
	}
	creds := resolveXAIImageCreds(env)
	key := [2]string{xaiImageBaseURL(creds), provider.FingerprintSecretValue(creds.APIKey)}
	xaiLiveCache.Lock()
	cached, ok := xaiLiveCache.byCredential[key]
	xaiLiveCache.Unlock()
	if ok && xaiLiveNow().Sub(cached.at) < xaiLiveCacheTTL {
		return cached.live
	}
	live := fetchXAILiveImageModelsOrEmpty(ctx, env, &creds)
	xaiLiveCache.Lock()
	if xaiLiveCache.byCredential == nil {
		xaiLiveCache.byCredential = map[[2]string]xaiLiveCacheEntry{}
	}
	xaiLiveCache.byCredential[key] = xaiLiveCacheEntry{live, xaiLiveNow()}
	xaiLiveCache.Unlock()
	return live
}

// Live ids with curated metadata plus the curated entries the live list omits, the static table when offline
func xaiImageCatalog(ctx context.Context, env *tools.Env) imageModelCatalog {
	live := xaiLiveImageModels(ctx, env)
	if len(live.order) == 0 {
		return imageModelCatalog{order: append([]string(nil), xaiImageModels.order...), meta: xaiImageModels.meta}
	}
	merged := imageModelCatalog{meta: map[string]map[string]any{}}
	for _, id := range live.order {
		meta := xaiImageModels.meta[id]
		if meta == nil {
			meta = map[string]any{"display": id, "speed": "", "strengths": "New xAI Imagine model (from live xAI catalog)"}
		}
		row := map[string]any{}
		for k, v := range meta {
			row[k] = v
		}
		mods := live.models[id]["input_modalities"]
		if !anyutil.IsTrue(mods) {
			mods = []any{}
		}
		row["input_modalities"] = mods
		merged.order = append(merged.order, id)
		merged.meta[id] = row
	}
	for _, id := range xaiImageModels.order {
		if _, ok := merged.meta[id]; !ok {
			merged.order = append(merged.order, id)
			merged.meta[id] = xaiImageModels.meta[id]
		}
	}
	return merged
}

// image_gen.xai.model with its XAI_IMAGE_MODEL override, empty when unset
func xaiConfiguredModel(env *tools.Env) string {
	return strings.TrimSpace(settings.XAIImageModel.Get(tools.WithEnvHome(context.Background(), env)))
}

// Caller model of a kind that cannot name a model, a TypeError
type xaiUnhashableError struct{ kind string }

func (e *xaiUnhashableError) Error() string {
	return "xAI image model is " + e.kind + " where text was expected"
}
func (e *xaiUnhashableError) ErrorKind() string { return "TypeError" }

// String form, truthiness and hashability of a caller model kwarg
func xaiCallerModel(v any) (string, bool, error) {
	switch v.(type) {
	case []any, []string, map[string]any, *jsonutil.Object:
		return "", true, &xaiUnhashableError{errchain.Kind(v)}
	}
	s, _ := v.(string)
	return s, anyutil.IsTrue(v), nil
}

// Caller kwarg, then XAI_IMAGE_MODEL, then config, then the default, each validated against the merged catalog
func resolveXAIImageModel(ctx context.Context, env *tools.Env, caller any) (string, map[string]any, error) {
	cat := xaiImageCatalog(ctx, env)
	c, _, err := xaiCallerModel(caller)
	if err != nil {
		return "", nil, err
	}
	for _, candidate := range []string{c, xaiConfiguredModel(env)} {
		if meta, ok := cat.meta[candidate]; candidate != "" && ok {
			return candidate, meta, nil
		}
	}
	if meta, ok := cat.meta[xaiImageDefaultModel]; ok {
		return xaiImageDefaultModel, meta, nil
	}
	return xaiImageDefaultModel, xaiImageModels.meta[xaiImageDefaultModel], nil
}

// Explicit selection that accepts image input, else the documented quality baseline
func resolveXAIEditModel(ctx context.Context, env *tools.Env, caller any) (string, error) {
	cat := xaiImageCatalog(ctx, env)
	c, truthy, err := xaiCallerModel(caller)
	if err != nil {
		return "", err
	}
	explicit := c
	if !truthy {
		explicit = xaiConfiguredModel(env)
	}
	if meta, ok := cat.meta[explicit]; explicit != "" && ok {
		if mods, _ := meta["input_modalities"].([]any); listContains(mods, "image") {
			return explicit, nil
		}
		if mods, _ := meta["input_modalities"].([]string); listContains(stringsAsAny(mods), "image") {
			return explicit, nil
		}
	}
	return xaiEditFallbackModel, nil
}

func stringsAsAny(s []string) []any {
	out := make([]any, len(s))
	for i, v := range s {
		out[i] = v
	}
	return out
}

func listContains(items []any, want string) bool {
	for _, v := range items {
		if s, ok := v.(string); ok && s == want {
			return true
		}
	}
	return false
}

// Configured 1k or 2k resolution
func resolveXAIResolution(env *tools.Env) string {
	return settings.XAIImageResolution.Get(tools.WithEnvHome(context.Background(), env))
}

func hasXAIRemotePrefix(s string) bool {
	lower := strings.ToLower(s)
	for _, p := range xaiRemotePrefixes {
		if strings.HasPrefix(lower, p) {
			return true
		}
	}
	return false
}

// Edit image field, URLs and data URIs passing through and local paths inlined as data URIs after the read guard
func xaiImageField(env *tools.Env, source string) (map[string]any, error) {
	source = strings.TrimSpace(source)
	if hasXAIRemotePrefix(source) {
		return map[string]any{"url": source, "type": "image_url"}, nil
	}
	if blocked := tools.ReadBlockError(env, source); blocked != "" {
		return nil, &imageValueError{blocked}
	}
	raw, err := os.ReadFile(pathutil.ExpandUser(source))
	if err != nil {
		return nil, fmt.Errorf("%s", errchain.OSText(err))
	}
	_, ext := pathutil.SplitExt(source)
	ext = strings.ToLower(strings.TrimLeft(ext, "."))
	if ext == "" {
		ext = "png"
	}
	if ext == "jpg" {
		ext = "jpeg"
	}
	return map[string]any{"url": "data:image/" + ext + ";base64," + base64.StdEncoding.EncodeToString(raw), "type": "image_url"}, nil
}

// At most three sources, each a remote URL, a data URI or an existing local file
func checkXAISourceImages(sources []string, imageURL string, fail errorFn) map[string]any {
	if len(sources) > xaiMaxSourceImages {
		return fail(fmt.Sprintf("xAI image editing supports at most %d source images", xaiMaxSourceImages), "too_many_references")
	}
	for index, source := range sources {
		if hasXAIRemotePrefix(source) {
			continue
		}
		if p, err := pathutil.ExpandUserPath(source); err == nil {
			if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() {
				continue
			}
		}
		field := "reference_image_urls"
		if index == 0 && imageURL != "" && strings.TrimSpace(imageURL) == source {
			field = "image_url"
		}
		return fail(field+" must be a public HTTPS URL or data URI "+
			"(e.g. the `image`/`public_url` from a prior Imagine result)", "invalid_image_url")
	}
	return nil
}

// xAI grok-imagine-image backend
type xaiImageGenProvider struct {
	staticImageGenProvider
	env func(context.Context) *tools.Env
}

func newXAIImageGenProvider() *xaiImageGenProvider {
	return &xaiImageGenProvider{env: pluginEnv, staticImageGenProvider: staticImageGenProvider{providerID: "xai", label: "xAI (Grok)"}}
}

// Available when xAI OAuth or XAI_API_KEY resolves a key for the process profile
func (p *xaiImageGenProvider) IsAvailable() bool { return p.AvailableIn(context.Background()) }

// Available when xAI OAuth or XAI_API_KEY resolves a key in the profile home the context binds
func (p *xaiImageGenProvider) AvailableIn(ctx context.Context) bool {
	return resolveXAIImageCreds(p.env(ctx)).APIKey != ""
}

// Picker rows of the merged live and curated catalog
func (p *xaiImageGenProvider) ListModels() []map[string]any {
	ctx := context.Background()
	return catalogRows(xaiImageCatalog(ctx, p.env(ctx)), []string{"display", "speed", "strengths"}, nil)
}

// First model of the merged catalog
func (p *xaiImageGenProvider) DefaultModel() string {
	ctx := context.Background()
	if cat := xaiImageCatalog(ctx, p.env(ctx)); len(cat.order) > 0 {
		return cat.order[0]
	}
	return ""
}

// OAuth driven setup with the storage notice appended to the tag when one applies
func (p *xaiImageGenProvider) GetSetupSchema() map[string]any {
	tag := "grok-imagine-image - text-to-image & image editing; uses xAI Grok OAuth or XAI_API_KEY"
	if notice := xaiStorageNoticeText(p.env(context.Background()), "image_gen"); notice != "" {
		tag += ". " + notice
	}
	return map[string]any{"name": "xAI Grok Imagine (image)", "badge": "paid", "tag": tag, "env_vars": []any{}, "post_setup": "xai_grok"}
}

// Text and image modalities, two references and three source images
func (p *xaiImageGenProvider) Capabilities() map[string]any {
	return map[string]any{"modalities": []any{"text", "image"}, "max_reference_images": 2, "max_source_images": xaiMaxSourceImages}
}

// Text to image through images/generations or editing through the JSON images/edits endpoint
func (p *xaiImageGenProvider) Generate(ctx context.Context, env *tools.Env, req ImageGenRequest) (map[string]any, error) {
	if env == nil {
		env = p.env(ctx)
	}
	creds := resolveXAIImageCreds(env)
	apiKey := strings.TrimSpace(creds.APIKey)
	providerName := strings.TrimSpace(creds.Provider)
	if providerName == "" {
		providerName = "xai"
	}
	rawAspect := req.AspectRatio
	if rawAspect == "" {
		rawAspect = defaultImageAspectRatio
	}
	if apiKey == "" {
		return errorFactory(providerName, rawAspect, "", "")(xaiImageMissingKeyError, "missing_api_key"), nil
	}
	modelID, _, err := resolveXAIImageModel(ctx, env, req.Kwargs["model"])
	if err != nil {
		return nil, err
	}
	aspect := resolveAspectRatio(req.AspectRatio)
	xaiRes := resolveXAIResolution(env)
	sources := collectSourceImages(req.ImageURL, req.ReferenceImageURLs, -1)
	editFail := errorFactory(providerName, aspect, xaiEditFallbackModel, req.Prompt)
	if failure := checkXAISourceImages(sources, req.ImageURL, editFail); failure != nil {
		return failure, nil
	}
	isEdit := len(sources) > 0
	headers := map[string]string{"Authorization": "Bearer " + apiKey, "Content-Type": "application/json", "User-Agent": tools.IcebathXAIUserAgent()}
	baseURL := xaiImageBaseURL(creds)
	storageOptions := buildXAIStorageOptions(env, "image_gen", "icebath-xai-image", "png")
	storageNotice := maybeMarkXAIStorageNoticeSeen(env, "image_gen")
	storageCfg := readXAIStorageConfig(env, "image_gen")
	var payload *jsonutil.Object
	var endpointURL string
	if isEdit {
		if modelID, err = resolveXAIEditModel(ctx, env, req.Kwargs["model"]); err != nil {
			return nil, err
		}
		fields := []any{}
		for _, s := range sources {
			field, err := xaiImageField(env, s)
			if err != nil {
				return editFail("Could not load source image for editing: "+err.Error(), "io_error", errorOverride{Model: &modelID}), nil
			}
			fields = append(fields, field)
		}
		payload = jsonutil.ObjectOf("model", modelID, "prompt", req.Prompt)
		if len(fields) == 1 {
			payload.Set("image", fields[0])
		} else {
			payload.Set("images", fields)
		}
		endpointURL = baseURL + "/images/edits"
	} else {
		ratio, ok := xaiAspectRatios[aspect]
		if !ok {
			ratio = "1:1"
		}
		payload = jsonutil.ObjectOf("model", modelID, "prompt", req.Prompt, "aspect_ratio", ratio, "resolution", xaiRes)
		endpointURL = baseURL + "/images/generations"
	}
	if storageOptions != nil {
		payload.Set("storage_options", storageOptions)
	}
	fail := errorFactory(providerName, aspect, modelID, req.Prompt)
	result, failure, err := postJSON(ctx, endpointURL, headers, payload, requestsTimeout{Connect: xaiImageRequestTimeout, Read: xaiImageRequestTimeout}, "xAI", defaultErrorMessage, false)
	if err != nil {
		return nil, err
	}
	if failure != nil {
		if failure.Kind == "http" {
			pkgLog().Error(fmt.Sprintf("xAI image gen failed (%d): %s", failure.Status, failure.Message))
		}
		return fail(failure.Error, failure.ErrorType), nil
	}
	obj, ok := jsonutil.AsObject(result)
	if !ok {
		return nil, fmt.Errorf("xAI image reply is %s where a mapping was expected", errchain.Kind(result))
	}
	data, has := obj.Get("data")
	if !has {
		data = []any{}
	}
	if !anyutil.IsTrue(data) {
		return fail("xAI returned no image data", "empty_response"), nil
	}
	items, ok := data.([]any)
	if !ok {
		return nil, fmt.Errorf("xAI image data is %s where a list was expected", errchain.Kind(data))
	}
	first, isObj := jsonutil.AsObject(items[0])
	fileOutput := jsonutil.NewObject()
	if isObj {
		if fo, ok := jsonutil.AsObject(first.Value("file_output")); ok {
			fileOutput = fo
		}
	}
	publicURL, _ := fileOutput.Value("public_url").(string)
	imageRef := publicURL
	if publicURL == "" {
		if !isObj {
			return nil, fmt.Errorf("xAI image item is %s where a mapping was expected", errchain.Kind(items[0]))
		}
		b64, _ := first.Value("b64_json").(string)
		rawURL, _ := first.Value("url").(string)
		var envelope map[string]any
		imageRef, envelope = materializeImage(ctx, env, b64, rawURL, materializeOptions{Prefix: "xai_" + modelID, Label: "xAI", Provider: "xai",
			Model: modelID, Prompt: req.Prompt, Aspect: aspect, ProviderBase: baseURL, Log: pkgLog()})
		if envelope != nil {
			return envelope, nil
		}
	}
	extra := map[string]any{"storage_enabled": storageCfg.Enabled}
	if !isEdit {
		extra["resolution"] = xaiRes
	}
	if storageNotice != "" {
		extra["storage_notice"] = storageNotice
	}
	if publicURL != "" {
		extra["public_url"] = publicURL
	}
	for _, k := range xaiFileOutputExtraKeys {
		if v, ok := fileOutput.Get(k); ok {
			extra[k] = v
		}
	}
	if usage, _ := obj.Get("usage"); anyutil.IsTrue(usage) {
		extra["usage"] = usage
	}
	modality := "text"
	if isEdit {
		modality = "image"
	}
	return imageSuccessResponse(imageRef, modelID, req.Prompt, aspect, "xai", modality, extra), nil
}

func init() {
	plugins.RegisterBundled(imageGenBackendPlugin{name: "xai", author: "Julien Talbot", requiresEnv: []string{"XAI_API_KEY"},
		description: "xAI image generation backend (grok-imagine-image). Text-to-image.",
		provider:    func() ImageGenProvider { return newXAIImageGenProvider() }})
}
