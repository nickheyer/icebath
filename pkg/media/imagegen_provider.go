package media

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

var imageAspectRatios = []string{"landscape", "square", "portrait"}

const defaultImageAspectRatio = "landscape"

// One image_generate call as the provider sees it, Kwargs carrying upscale, model and creative controls
type ImageGenRequest struct {
	Prompt             string
	AspectRatio        string
	ImageURL           string
	ReferenceImageURLs []string
	Kwargs             map[string]any
}

// Image generation backend selected by image_gen.provider
type ImageGenProvider interface {
	provider.CatalogProviderBase
	Capabilities() map[string]any
	Generate(ctx context.Context, env *tools.Env, req ImageGenRequest) (map[string]any, error)
}

// Text only capability declaration every provider gets unless it overrides it
func defaultImageCapabilities() map[string]any {
	return map[string]any{"modalities": []any{"text"}, "max_reference_images": 0}
}

// Lowercased known aspect ratio, landscape for anything else
func resolveAspectRatio(value any) string {
	s, _ := value.(string)
	v := strings.ToLower(strings.TrimSpace(s))
	for _, r := range imageAspectRatios {
		if v == r {
			return v
		}
	}
	return defaultImageAspectRatio
}

// Trimmed non empty reference strings from a string or list, nil when nothing usable remains
func normalizeReferenceImages(value any) []string {
	var items []any
	switch v := value.(type) {
	case string:
		items = []any{v}
	case []any:
		items = v
	case []string:
		for _, s := range v {
			items = append(items, s)
		}
	default:
		return nil
	}
	var out []string
	for _, item := range items {
		if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
			out = append(out, strings.TrimSpace(s))
		}
	}
	return out
}

var generatedImageKind = generatedSubdir + "/images"

// Decodes base64 image data into the generated images cache
func saveB64Image(env *tools.Env, b64 string, prefix, extension string) (string, error) {
	return saveB64(env, generatedImageKind, b64, prefix, extension)
}

var urlImageContentTypes = map[string]string{
	"image/png": "png", "image/jpeg": "jpg", "image/jpg": "jpg", "image/webp": "webp", "image/gif": "gif",
}

// Downloads a provider image URL into the generated images cache under the SSRF guard and size cap
func saveURLImage(ctx context.Context, env *tools.Env, rawURL, prefix string, timeout time.Duration, maxBytes int64) (string, error) {
	if prefix == "" {
		prefix = "image"
	}
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	if maxBytes <= 0 {
		maxBytes = 25 * 1024 * 1024
	}
	return saveURL(ctx, env, generatedImageKind, rawURL, saveURLOptions{
		Prefix: prefix, Timeout: timeout, MaxBytes: maxBytes, ChunkSize: 64 * 1024,
		ContentTypes: urlImageContentTypes, URLExtensions: []string{"png", "jpg", "jpeg", "webp", "gif"},
		DefaultExtension: "png", Label: "Image", EmptyError: "Image at {url} returned 0 bytes; refusing to cache.",
	})
}

// Success envelope, extra keys added only where the base keys leave them unset
func imageSuccessResponse(image, model, prompt, aspectRatio, provider, modality string, extra map[string]any) map[string]any {
	if modality == "" {
		modality = "text"
	}
	payload := map[string]any{
		"success": true, "image": image, "model": model, "prompt": prompt,
		"aspect_ratio": aspectRatio, "modality": modality, "provider": provider,
	}
	for k, v := range extra {
		if _, ok := payload[k]; !ok {
			payload[k] = v
		}
	}
	return payload
}

// Failure envelope with a null image and the provider_error type by default
func imageErrorResponse(errText, errorType, provider, model, prompt, aspectRatio string) map[string]any {
	if errorType == "" {
		errorType = "provider_error"
	}
	if aspectRatio == "" {
		aspectRatio = defaultImageAspectRatio
	}
	return map[string]any{
		"success": false, "image": nil, "error": errText, "error_type": errorType,
		"model": model, "prompt": prompt, "aspect_ratio": aspectRatio, "provider": provider,
	}
}

var imageGenLog = slog.Default().With("logger", "agent.image_gen_registry")

var imageGenRegistry = provider.NewProviderRegistry[ImageGenProvider](provider.ProviderRegistryOptions{
	Label: "Image gen", Logger: imageGenLog,
})

// ResetImageGenRegistry empties the image generation registry, the registry's _reset_for_tests
func ResetImageGenRegistry() { imageGenRegistry.ResetForTests() }

// Registers an image generation provider, a same name registration replacing the earlier one
func RegisterImageGenProvider(p ImageGenProvider, scope string) error {
	return imageGenRegistry.Register(p, scope)
}

// Registered image generation providers sorted by name
func ListImageGenProviders(ctx context.Context, scope string) []ImageGenProvider {
	return imageGenRegistry.ListProviders(ctx, scope)
}

// Provider registered under the name, scoped registration first, nil when absent
func GetImageGenProvider(ctx context.Context, name, scope string) ImageGenProvider {
	return imageGenRegistry.GetProvider(ctx, name, scope)
}

// Plugin facing registry that register_image_gen_provider writes into, adapting hosted Python providers
func ImageGenScopedRegistry() plugins.ScopedRegistry {
	return hostedImageScopedRegistry{imageGenRegistry.Scoped()}
}

// Scoped registry view that wraps plugin host objects as ImageGenProvider values before delegating
type hostedImageScopedRegistry struct{ inner plugins.ScopedRegistry }

func (s hostedImageScopedRegistry) Accepts(p any) bool {
	return s.inner.Accepts(adaptHostedImageGenProvider(p))
}
func (s hostedImageScopedRegistry) ProviderName(p any) string {
	return s.inner.ProviderName(adaptHostedImageGenProvider(p))
}
func (s hostedImageScopedRegistry) Identity(p any) error {
	return s.inner.Identity(adaptHostedImageGenProvider(p))
}
func (s hostedImageScopedRegistry) Snapshot(name, scope string) any {
	return s.inner.Snapshot(name, scope)
}
func (s hostedImageScopedRegistry) Register(p any, scope string) (bool, error) {
	return s.inner.Register(adaptHostedImageGenProvider(p), scope)
}
func (s hostedImageScopedRegistry) Restore(name string, current, replacement any, scope string) bool {
	return s.inner.Restore(name, adaptHostedImageGenProvider(current), adaptHostedImageGenProvider(replacement), scope)
}

// Context whose home resolution answers the environment home
func envContext(ctx context.Context, env *tools.Env) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return home.WithOverride(ctx, homeOf(env))
}

// Configured provider even when unavailable, else the single available one, else an available fal
func GetActiveImageGenProvider(ctx context.Context, env *tools.Env) ImageGenProvider {
	ctx = envContext(ctx, env)
	configured := provider.ConfiguredProviderName(ctx, "image_gen", imageGenLog)
	snapshot := imageGenRegistry.Merged(ctx, "")
	if configured != "" {
		if p, ok := snapshot.Get(configured); ok && p != nil {
			return p
		}
		imageGenLog.Debug(fmt.Sprintf("image_gen.provider='%s' configured but not registered; falling back", configured))
	}
	available := func(p ImageGenProvider) bool {
		return provider.IsAvailableSafe(p, imageGenLog, "image_gen provider %s.is_available() raised %s", slog.LevelDebug, false)
	}
	var avail []ImageGenProvider
	for _, p := range snapshot.Values() {
		if available(p) {
			avail = append(avail, p)
		}
	}
	if len(avail) == 1 {
		return avail[0]
	}
	if fal, ok := snapshot.Get("fal"); ok && fal != nil && available(fal) {
		return fal
	}
	return nil
}

// Python plugin provider object proxied from the plugin host
type hostedImageGenProvider struct{ obj *plugins.HostedObject }

// Plugin host object that subclasses ImageGenProvider as a provider, any other value unchanged
func adaptHostedImageGenProvider(p any) any {
	obj, ok := p.(*plugins.HostedObject)
	if !ok || obj == nil || !obj.HasMethod("generate") {
		return p
	}
	return hostedImageGenProvider{obj: obj}
}

func (h hostedImageGenProvider) get(name string) any {
	v, err := h.obj.Get(context.Background(), name)
	if err != nil {
		return nil
	}
	return v
}

func (h hostedImageGenProvider) call(ctx context.Context, method string, args []any, kwargs map[string]any) any {
	v, err := h.obj.Call(ctx, method, args, kwargs)
	if err != nil {
		return nil
	}
	return v
}

func (h hostedImageGenProvider) Name() string {
	s, _ := h.get("name").(string)
	return s
}

func (h hostedImageGenProvider) DisplayName() string {
	if s, ok := h.get("display_name").(string); ok {
		return s
	}
	return pystr.Title(h.Name())
}

func (h hostedImageGenProvider) GetSetupSchema() map[string]any {
	if m, ok := h.call(context.Background(), "get_setup_schema", nil, nil).(map[string]any); ok {
		return m
	}
	return map[string]any{"name": h.DisplayName(), "badge": "", "tag": "", "env_vars": []any{}}
}

func (h hostedImageGenProvider) IsAvailable() bool {
	v, err := h.obj.Call(context.Background(), "is_available", nil, nil)
	if err != nil {
		panic(err)
	}
	b, _ := v.(bool)
	return b
}

func (h hostedImageGenProvider) ListModels() []map[string]any {
	rows, _ := h.call(context.Background(), "list_models", nil, nil).([]any)
	var out []map[string]any
	for _, r := range rows {
		if m, ok := r.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func (h hostedImageGenProvider) DefaultModel() string {
	s, _ := h.call(context.Background(), "default_model", nil, nil).(string)
	return s
}

func (h hostedImageGenProvider) Capabilities() map[string]any {
	if m, ok := h.call(context.Background(), "capabilities", nil, nil).(map[string]any); ok {
		return m
	}
	return defaultImageCapabilities()
}

func (h hostedImageGenProvider) Generate(ctx context.Context, _ *tools.Env, req ImageGenRequest) (map[string]any, error) {
	kwargs := map[string]any{}
	for k, v := range req.Kwargs {
		kwargs[k] = v
	}
	if req.ImageURL != "" {
		kwargs["image_url"] = req.ImageURL
	}
	if len(req.ReferenceImageURLs) > 0 {
		refs := make([]any, len(req.ReferenceImageURLs))
		for i, r := range req.ReferenceImageURLs {
			refs[i] = r
		}
		kwargs["reference_image_urls"] = refs
	}
	v, err := h.obj.Call(ctx, "generate", []any{req.Prompt, req.AspectRatio}, kwargs)
	if err != nil {
		return nil, err
	}
	m, _ := v.(map[string]any)
	return m, nil
}
