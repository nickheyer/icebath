package generate

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/media"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Providers that can ground generation on a reference image, in preference order
var refCapable = []string{"openai", "openai-codex", "openrouter", "krea"}

// GenerationError is any image generation failure, no provider, API error or IO
type GenerationError struct{ Msg string }

func (e *GenerationError) Error() string { return e.Msg }

// SpriteProvider is the resolved image provider and whether it takes reference images
type SpriteProvider struct {
	Name               string
	Provider           media.ImageGenProvider
	SupportsReferences bool
}

// Discovers image generation plugins, a failure only logged
func discoverImagePlugins(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			pkgLog().Debug(fmt.Sprintf("image-gen plugin discovery failed: %v", r))
		}
	}()
	plugins.GetPluginManager(ctx).DiscoverAndLoad(ctx, false)
}

// Registered provider of the name when it has credentials in the context's profile scope, nil otherwise
func availableProvider(ctx context.Context, name string) media.ImageGenProvider {
	p := media.GetImageGenProvider(ctx, name, "")
	if p != nil && p.AvailableIn(ctx) {
		return p
	}
	return nil
}

// Whether name is one of the reference capable providers
func isRefCapable(name string) bool {
	for _, n := range refCapable {
		if n == name {
			return true
		}
	}
	return false
}

// Picks the sprite image provider, the env override, prefer, the active one, then the first available
func ResolveProvider(ctx context.Context, env *tools.Env, requireReferences bool, prefer string) (*SpriteProvider, error) {
	discoverImagePlugins(ctx)
	forced := strings.ToLower(strings.TrimSpace(os.Getenv("ICEBATH_PET_IMAGE_PROVIDER")))
	for _, name := range []string{forced, prefer} {
		if isRefCapable(name) {
			if chosen := availableProvider(ctx, name); chosen != nil {
				return &SpriteProvider{Name: name, Provider: chosen, SupportsReferences: true}, nil
			}
		}
	}
	var active media.ImageGenProvider
	func() {
		defer func() { _ = recover() }()
		active = media.GetActiveImageGenProvider(ctx, env)
	}()
	activeName := ""
	if active != nil {
		activeName = active.Name()
	}
	if isRefCapable(activeName) && active.AvailableIn(ctx) {
		return &SpriteProvider{Name: activeName, Provider: active, SupportsReferences: true}, nil
	}
	for _, name := range refCapable {
		if p := availableProvider(ctx, name); p != nil {
			return &SpriteProvider{Name: name, Provider: p, SupportsReferences: true}, nil
		}
	}
	if !requireReferences && active != nil && active.AvailableIn(ctx) {
		name := activeName
		if name == "" {
			name = "unknown"
		}
		return &SpriteProvider{Name: name, Provider: active, SupportsReferences: false}, nil
	}
	return nil, &GenerationError{Msg: "Pet generation needs an image backend that supports reference images. " +
		"Pick OpenRouter or OpenAI in /settings → Images & media and add its key."}
}

// Local path of an image reference, downloading a URL first
func saveLocal(ctx context.Context, env *tools.Env, ref, prefix string) (string, error) {
	if strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://") {
		return media.SaveURLImage(ctx, env, ref, prefix)
	}
	return ref, nil
}

// Whether a provider error is about the transparent background flag
func rejectedBackground(err string) bool {
	lowered := strings.ToLower(err)
	return strings.Contains(lowered, "background") && (strings.Contains(lowered, "not supported") || strings.Contains(lowered, "transparent"))
}

// Generates n sprite images and returns their local paths, retrying without the transparent flag where rejected
func Generate(ctx context.Context, env *tools.Env, prompt string, n int, references []string, sprite *SpriteProvider, prefix, aspectRatio string) ([]string, error) {
	if sprite == nil {
		var err error
		if sprite, err = ResolveProvider(ctx, env, len(references) > 0, ""); err != nil {
			return nil, err
		}
	}
	if len(references) > 0 && !sprite.SupportsReferences {
		return nil, &GenerationError{Msg: fmt.Sprintf("image backend '%s' cannot use reference images; "+
			"configure OpenAI gpt-image-2 or Krea for pet generation", sprite.Name)}
	}
	run := func(transparent bool) (string, string) {
		kwargs := map[string]any{}
		if transparent {
			kwargs["background"] = "transparent"
		}
		req := media.ImageGenRequest{Prompt: prompt, AspectRatio: aspectRatio, Kwargs: kwargs}
		if len(references) > 0 {
			req.ReferenceImageURLs = append([]string{}, references...)
			refs := make([]any, len(references))
			for i, r := range references {
				refs[i] = r
			}
			kwargs["reference_images"] = refs
		}
		result, err := func() (result map[string]any, err error) {
			defer func() {
				if r := recover(); r != nil {
					err = fmt.Errorf("%v", r)
				}
			}()
			return sprite.Provider.Generate(ctx, env, req)
		}()
		if err != nil {
			pkgLog().Debug(fmt.Sprintf("provider.generate crashed: %s", pyrepr.ExceptionMessage(err)))
			return "", pyrepr.ExceptionMessage(err)
		}
		if result == nil {
			return "", "no result"
		}
		if !pyrepr.Truthy(result["success"]) {
			if v, ok := result["error"]; ok {
				return "", pyrepr.Str(v)
			}
			return "", "unknown error"
		}
		imageRef := result["image"]
		if !pyrepr.Truthy(imageRef) {
			return "", "provider returned no image"
		}
		path, serr := saveLocal(ctx, env, pyrepr.Str(imageRef), prefix)
		if serr != nil {
			return "", "could not save generated image: " + pyrepr.ExceptionMessage(serr)
		}
		return path, ""
	}
	var out []string
	lastError := ""
	allowTransparent := true
	for i := 0; i < max(1, n); i++ {
		path, errText := run(allowTransparent)
		if path == "" && allowTransparent && rejectedBackground(errText) {
			allowTransparent = false
			path, errText = run(false)
		}
		if path != "" {
			out = append(out, path)
		} else {
			lastError = errText
		}
	}
	if len(out) == 0 {
		if lastError == "" {
			lastError = "image generation produced no output"
		}
		return nil, &GenerationError{Msg: lastError}
	}
	return out, nil
}
