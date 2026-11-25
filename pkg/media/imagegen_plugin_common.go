package media

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/settings"
	"github.com/nickheyer/icebath/pkg/tools"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
)

var openAISizes = map[string]string{"landscape": "1536x1024", "square": "1024x1024", "portrait": "1024x1536"}

const (
	gptImage2APIModel = "gpt-image-2"
	gptImage2Default  = "gpt-image-2-medium"
	promptRequired    = "Prompt is required and must be a non-empty string"
)

// Fixed model catalog keeping the upstream dict order
type imageModelCatalog struct {
	order []string
	meta  map[string]map[string]any
}

func (c imageModelCatalog) get(id string) (map[string]any, bool) {
	m, ok := c.meta[id]
	return m, ok
}

var gptImage2Tiers = imageModelCatalog{
	order: []string{"gpt-image-2-low", "gpt-image-2-medium", "gpt-image-2-high"},
	meta: map[string]map[string]any{
		"gpt-image-2-low":    {"display": "GPT Image 2 (Low)", "speed": "~15s", "strengths": "Fast iteration, lowest cost", "quality": "low"},
		"gpt-image-2-medium": {"display": "GPT Image 2 (Medium)", "speed": "~40s", "strengths": "Balanced — default", "quality": "medium"},
		"gpt-image-2-high":   {"display": "GPT Image 2 (High)", "speed": "~2min", "strengths": "Highest fidelity, strongest prompt adherence", "quality": "high"},
	},
}

// OpenAI size string of an aspect, square when the aspect is unknown
func sizeFor(aspect string) string {
	if s, ok := openAISizes[aspect]; ok {
		return s
	}
	return openAISizes["square"]
}

// Home and .env of the profile the context binds, for a provider method that gets no call environment
func pluginEnv(ctx context.Context) *tools.Env {
	h := home.Dir(ctx)
	return &tools.Env{Home: h, Secrets: config.NewSecrets(h)}
}

// Options of resolveStaticModel: Scoped is the provider's own model setting, ExcludeTopLevel skips image_gen.model
type staticModelOptions struct {
	Scoped                       *settings.String
	Explicit                     string
	ExcludeTopLevel, Passthrough bool
}

// First known model id among explicit, the provider's own setting and image_gen.model, else the default, an unknown id of the provider's own setting passing through when allowed
func resolveStaticModel(env *tools.Env, models imageModelCatalog, def string, o staticModelOptions) (string, map[string]any) {
	if e := strings.TrimSpace(o.Explicit); e != "" {
		if m, ok := models.get(e); ok {
			return e, m
		}
	}
	ctx := tools.WithEnvHome(context.Background(), env)
	scoped := ""
	if o.Scoped != nil {
		scoped = strings.TrimSpace(o.Scoped.Get(ctx))
	}
	candidates := []string{scoped}
	if !o.ExcludeTopLevel {
		candidates = append(candidates, strings.TrimSpace(settings.ImageGenModel.Get(ctx)))
	}
	for _, c := range candidates {
		if m, ok := models.get(c); c != "" && ok {
			return c, m
		}
	}
	if o.Passthrough && scoped != "" {
		return scoped, map[string]any{"display": scoped, "api_model": scoped, "quality": nil}
	}
	m, _ := models.get(def)
	return def, m
}

// Primary image first then the normalized references, clamped to limit when limit is not negative
func collectSourceImages(imageURL string, referenceImageURLs any, limit int) []string {
	var sources []string
	if s := strings.TrimSpace(imageURL); s != "" {
		sources = append(sources, s)
	}
	sources = append(sources, normalizeReferenceImages(referenceImageURLs)...)
	if limit >= 0 && len(sources) > limit {
		sources = sources[:limit]
	}
	return sources
}

var defaultCatalogFields = []string{"display", "speed", "strengths", "price"}

// Picker rows of id plus the fields, display defaulting to the id and others to empty, price overriding when set
func catalogRows(models imageModelCatalog, fields []string, price *string) []map[string]any {
	if fields == nil {
		fields = defaultCatalogFields
	}
	rows := make([]map[string]any, 0, len(models.order))
	for _, id := range models.order {
		meta := models.meta[id]
		row := map[string]any{"id": id}
		for _, f := range fields {
			v, ok := meta[f]
			if !ok {
				v = ""
				if f == "display" {
					v = id
				}
			}
			row[f] = v
		}
		if price != nil {
			row["price"] = *price
		}
		rows = append(rows, row)
	}
	return rows
}

// Setup schema of a provider authenticated by one env var
type apiKeySetup struct{ Name, Badge, Tag, Key, Prompt, URL string }

func apiKeySetupSchema(s apiKeySetup) map[string]any {
	return map[string]any{"name": s.Name, "badge": s.Badge, "tag": s.Tag,
		"env_vars": []any{map[string]any{"key": s.Key, "prompt": s.Prompt, "url": s.URL}}}
}

// Identity, picker rows and single key setup of a provider with a fixed catalog
type staticImageGenProvider struct {
	providerID     string
	label          string
	models         imageModelCatalog
	defaultModelID string
	price          *string
	catalogFields  []string
	setup          apiKeySetup
}

func (p *staticImageGenProvider) Name() string                   { return p.providerID }
func (p *staticImageGenProvider) DisplayName() string            { return p.label }
func (p *staticImageGenProvider) DefaultModel() string           { return p.defaultModelID }
func (p *staticImageGenProvider) GetSetupSchema() map[string]any { return apiKeySetupSchema(p.setup) }
func (p *staticImageGenProvider) ListModels() []map[string]any {
	return catalogRows(p.models, p.catalogFields, p.price)
}

// Overrides of the error envelope context, an unset field keeping the bound value
type errorOverride struct{ Provider, Model, Prompt, AspectRatio *string }

type errorFn func(errText, errorType string, o ...errorOverride) map[string]any

// Error envelope builder bound to one call's provider, aspect, model and prompt
func errorFactory(providerName, aspect, model, prompt string) errorFn {
	return func(errText, errorType string, o ...errorOverride) map[string]any {
		p, m, pr, a := providerName, model, prompt, aspect
		for _, ov := range o {
			if ov.Provider != nil {
				p = *ov.Provider
			}
			if ov.Model != nil {
				m = *ov.Model
			}
			if ov.Prompt != nil {
				pr = *ov.Prompt
			}
			if ov.AspectRatio != nil {
				a = *ov.AspectRatio
			}
		}
		return imageErrorResponse(errText, errorType, p, m, pr, a)
	}
}

// Invalid argument envelope for an empty prompt
func promptRequiredError(providerName, aspect string) map[string]any {
	return errorFactory(providerName, aspect, "", "")(promptRequired, "invalid_argument")
}

// Options of materializeImage and cacheURLBestEffort, ProviderBase naming the API endpoint that returned the result
type materializeOptions struct {
	Prefix, Label, Provider, Model, Prompt, Aspect string
	ProviderBase                                   string
	Log                                            *slog.Logger
	OnURLFail                                      func(error)
}

// Cached image path of a base64 or URL result, the bare URL when caching the URL fails, an envelope on error
func materializeImage(ctx context.Context, env *tools.Env, b64, rawURL string, o materializeOptions) (string, map[string]any) {
	fail := errorFactory(o.Provider, o.Aspect, o.Model, o.Prompt)
	if b64 != "" {
		path, err := saveB64Image(env, b64, o.Prefix, "png")
		if err != nil {
			return "", fail(fmt.Sprintf("Could not save image to cache: %v", err), "io_error")
		}
		return path, nil
	}
	if rawURL != "" {
		return cacheURLBestEffort(ctx, env, rawURL, o), nil
	}
	return "", fail(o.Label+" response contained neither b64_json nor URL", "empty_response")
}

// Local cache path of an image URL, the bare URL with a warning or the callback when the download fails
func cacheURLBestEffort(ctx context.Context, env *tools.Env, rawURL string, o materializeOptions) string {
	path, err := saveURLImage(ctx, env, rawURL, o.Prefix, 60*time.Second, 25*1024*1024, o.ProviderBase)
	if err == nil {
		return path
	}
	if o.OnURLFail != nil {
		o.OnURLFail(err)
		return rawURL
	}
	log := o.Log
	if log == nil {
		log = logOf(env)
	}
	log.Warn(fmt.Sprintf("%s image URL %s could not be cached (%v); falling back to bare URL.", o.Label, rawURL, err))
	return rawURL
}

// Records a token billed image call against the ambient session as task image_generation
func recordTokenUsage(ctx context.Context, usage any, model, providerName, baseURL string) {
	if !anyutil.IsTrue(usage) {
		return
	}
	provider.RecordAuxUsage(ctx, map[string]any{"model": model, "usage": usage}, "image_generation", providerName, baseURL)
}
