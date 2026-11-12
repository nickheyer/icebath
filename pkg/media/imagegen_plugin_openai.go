package media

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pybase64"
	"github.com/nickheyer/icebath/pkg/internal/pyhttpx"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// GPT Image 2 tiers on the gpt-image-2 API model, then every GPT Image 2.5 Flare and Sunburst quality
var openAIImageModels = func() imageModelCatalog {
	c := imageModelCatalog{meta: map[string]map[string]any{}}
	for _, id := range gptImage2Tiers.order {
		meta := map[string]any{}
		for k, v := range gptImage2Tiers.meta[id] {
			meta[k] = v
		}
		meta["api_model"] = gptImage2APIModel
		c.order = append(c.order, id)
		c.meta[id] = meta
	}
	for _, m := range [][4]string{{"gpt-image-2.5-flare", "Flare", "Fast", "Everyday image generation and editing"},
		{"gpt-image-2.5-sunburst", "Sunburst", "Slower", "Precision generation and editing"}} {
		for _, quality := range []string{"auto", "low", "medium", "high", "xhigh", "max"} {
			id := m[0] + "-" + quality
			if quality == "auto" {
				id = m[0]
			}
			c.order = append(c.order, id)
			c.meta[id] = map[string]any{"display": fmt.Sprintf("GPT Image 2.5 %s (%s)", m[1], strings.ToUpper(quality[:1])+quality[1:]),
				"speed": m[2], "strengths": m[3], "api_model": m[0], "quality": quality}
		}
	}
	return c
}()

// Credential read through the secret scope, panicking like the scope does on an unscoped multiplexed read
func mustSecret(ctx context.Context, name string) string {
	v, _, err := provider.GetSecret(ctx, name)
	if err != nil {
		panic(err)
	}
	return v
}

// Selected OpenAI tier, custom ids passing through from the env var or the scoped config
func resolveOpenAIImageModel(env *tools.Env) (string, map[string]any) {
	return resolveStaticModel(env, openAIImageModels, gptImage2Default, staticModelOptions{EnvVar: "OPENAI_IMAGE_MODEL", ConfigKey: "openai", Passthrough: true})
}

// Base URL and key of a declared custom endpoint, empty with a warning when no entry matches
func openAINamedEndpoint(ctx context.Context, env *tools.Env, name string) (string, string, error) {
	entry, err := provider.NamedCustomProvider(ctx, homeOf(env), name)
	if err != nil {
		return "", "", err
	}
	if len(entry) == 0 {
		logOf(env).Warn(fmt.Sprintf("image_gen.openai.provider '%s' matches no custom endpoint in providers:", name))
		return "", "", nil
	}
	keyEnv := strings.TrimSpace(pyStrOf(orNone(entry["key_env"])))
	apiKey := strings.TrimSpace(pyStrOf(orNone(entry["api_key"])))
	if apiKey == "" && keyEnv != "" {
		apiKey = mustSecret(ctx, keyEnv)
	}
	return strings.TrimRight(strings.TrimSpace(pyStrOf(orNone(entry["base_url"]))), "/"), apiKey, nil
}

// Empty string for a falsy value, the str() of the value otherwise
func orNone(v any) any {
	if !pyTruthy(v) {
		return ""
	}
	return v
}

// Base URL then key from config, the named endpoint, and the OpenAI env vars, shared by availability and generate
func resolveOpenAIEndpoint(ctx context.Context, env *tools.Env) (string, string, error) {
	cfg := loadImageGenConfig(env, "openai")
	named := strings.TrimSpace(pyStrOf(orNone(cfg["provider"])))
	var namedBase, namedKey string
	if named != "" {
		var err error
		if namedBase, namedKey, err = openAINamedEndpoint(ctx, env, named); err != nil {
			return "", "", err
		}
	}
	baseURL := strings.TrimRight(strings.TrimSpace(pyStrOf(orNone(cfg["base_url"]))), "/")
	if baseURL == "" {
		baseURL = namedBase
	}
	if baseURL == "" {
		baseURL = strings.TrimSpace(provider.GetSecretStr(ctx, "OPENAI_BASE_URL", ""))
	}
	keyEnv := strings.TrimSpace(pyStrOf(orNone(cfg["key_env"])))
	apiKey := ""
	if keyEnv != "" {
		apiKey = mustSecret(ctx, keyEnv)
	}
	if apiKey == "" {
		apiKey = namedKey
	}
	if apiKey == "" {
		apiKey = mustSecret(ctx, "OPENAI_API_KEY")
	}
	return baseURL, apiKey, nil
}

// OpenAI client on the env only proxy keepalive client with the project header blanked
func buildOpenAIImageClient(env *tools.Env, newClient func(*tools.Env, openAIClientOptions) (*openAIClient, error), baseURL, apiKey string) (*openAIClient, error) {
	o := openAIClientOptions{APIKey: &apiKey, DefaultHeaders: map[string]string{"OpenAI-Project": ""}}
	if baseURL != "" {
		o.BaseURL = &baseURL
	}
	if hc := provider.BuildKeepaliveHTTPClient(baseURL, false, provider.TLSVerify{}); hc != nil {
		o.HTTPClient = hc
	}
	return newClient(env, o)
}

// Bytes and filename of a URL, data URI or local path, URLs vetted for SSRF and local reads for credential stores
func loadOpenAIImageBytes(ctx context.Context, env *tools.Env, ref string) ([]byte, string, error) {
	ref = strings.TrimSpace(ref)
	lower := strings.ToLower(ref)
	switch {
	case strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://"):
		if !tools.IsSafeURL(env, ref) {
			return nil, "", &imageValueError{"Image reference URL failed the SSRF safety check: " + ref}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, ref, nil)
		if err != nil {
			return nil, "", err
		}
		resp, err := tools.NewSSRFSafeClient(env, 60*time.Second, true).Do(req)
		if err != nil {
			return nil, "", fmt.Errorf("%s", pyhttpx.ErrorText(err))
		}
		defer resp.Body.Close()
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, "", fmt.Errorf("%s", pyhttpx.ErrorText(err))
		}
		if resp.StatusCode >= 400 {
			return nil, "", fmt.Errorf("%s", pyhttpx.RaiseForStatusMessage(resp.StatusCode, resp.Request.URL.String(), resp.Header.Get("Location")))
		}
		name := strings.SplitN(ref, "?", 2)[0]
		name = name[strings.LastIndex(name, "/")+1:]
		if name == "" {
			name = "image.png"
		}
		return data, name, nil
	case strings.HasPrefix(lower, "data:"):
		header, b64, _ := strings.Cut(ref, ",")
		ext := ""
		if _, after, ok := strings.Cut(header, "image/"); ok {
			ext, _, _ = strings.Cut(after, ";")
		}
		if ext == "" {
			ext = "png"
		}
		data, err := pybase64.B64Decode(b64)
		if err != nil {
			return nil, "", err
		}
		return data, "image." + ext, nil
	}
	if msg := tools.ReadBlockError(env, ref); msg != "" {
		return nil, "", &imageValueError{msg}
	}
	data, err := os.ReadFile(ref)
	if err != nil {
		return nil, "", fmt.Errorf("%s", pypath.OSErrorText(err))
	}
	name := filepath.Base(ref)
	if name == "." || name == string(filepath.Separator) || strings.HasSuffix(ref, string(filepath.Separator)) {
		name = "image.png"
	}
	return data, name, nil
}

// Named upload of one source reference for the multipart edit
func openAINamedFile(ctx context.Context, env *tools.Env, ref string) (openAIFile, error) {
	data, name, err := loadOpenAIImageBytes(ctx, env, ref)
	return openAIFile{Name: name, Data: data}, err
}

// OpenAI images.generate and images.edit backend with the selectable API models
type openAIImageGenProvider struct {
	staticImageGenProvider
	env func() *tools.Env
}

func newOpenAIImageGenProvider() *openAIImageGenProvider {
	price := "varies"
	return &openAIImageGenProvider{env: pluginEnv, staticImageGenProvider: staticImageGenProvider{providerID: "openai", label: "OpenAI",
		models: openAIImageModels, defaultModelID: gptImage2Default, price: &price,
		setup: apiKeySetup{Name: "OpenAI", Badge: "paid", Tag: "GPT Image 2 / 2.5 Flare / 2.5 Sunburst — text-to-image & image editing",
			Key: "OPENAI_API_KEY", Prompt: "OpenAI API key", URL: "https://platform.openai.com/api-keys"}}}
}

// Available when the resolved endpoint carries a key
func (p *openAIImageGenProvider) IsAvailable() bool {
	_, key, err := resolveOpenAIEndpoint(context.Background(), p.env())
	if err != nil {
		panic(err)
	}
	return key != ""
}

// Text and image modalities with the sixteen source images images.edit accepts
func (p *openAIImageGenProvider) Capabilities() map[string]any {
	return map[string]any{"modalities": []any{"text", "image"}, "max_reference_images": 16}
}

// Generates or edits through the OpenAI Images API and caches the result
func (p *openAIImageGenProvider) Generate(ctx context.Context, env *tools.Env, req ImageGenRequest) (map[string]any, error) {
	if env == nil {
		env = p.env()
	}
	prompt := strings.TrimSpace(req.Prompt)
	aspect := resolveAspectRatio(req.AspectRatio)
	if prompt == "" {
		return promptRequiredError("openai", aspect), nil
	}
	baseURL, apiKey, err := resolveOpenAIEndpoint(ctx, env)
	if err != nil {
		return nil, err
	}
	if apiKey == "" {
		return errorFactory("openai", aspect, "", "")("OPENAI_API_KEY not set (or the variable named by image_gen.openai.key_env is empty). "+
			"Run `icebath tools` → Image Generation → OpenAI to configure, or `icebath setup` to add the key.", "auth_required"), nil
	}
	newClient := newOpenAIClientHook
	tierID, meta := resolveOpenAIImageModel(env)
	size := sizeFor(aspect)
	sources := collectSourceImages(req.ImageURL, req.ReferenceImageURLs, 16)
	isEdit := len(sources) > 0
	fail := errorFactory("openai", aspect, tierID, prompt)
	client, err := buildOpenAIImageClient(env, newClient, baseURL, apiKey)
	if err != nil {
		return nil, err
	}
	request := map[string]any{"model": meta["api_model"], "prompt": prompt, "size": size, "n": 1}
	if meta["quality"] != nil {
		request["quality"] = meta["quality"]
	}
	if isEdit {
		files := make([]openAIFile, 0, len(sources))
		for _, ref := range sources {
			f, err := openAINamedFile(ctx, env, ref)
			if err != nil {
				return fail(fmt.Sprintf("Could not load source image for editing: %v", err), "io_error"), nil
			}
			files = append(files, f)
		}
		if len(files) > 1 {
			request["image"] = files
		} else {
			request["image"] = files[0]
		}
	}
	call, verb, gerund := client.ImagesGenerate, "generation", "generation"
	if isEdit {
		call, verb, gerund = client.ImagesEdit, "edit", "editing"
	}
	response, err := call(ctx, request)
	if err != nil {
		logOf(env).Debug(fmt.Sprintf("OpenAI image %s failed", verb), "err", err)
		return fail(fmt.Sprintf("OpenAI image %s failed: %v", gerund, err), "api_error"), nil
	}
	apiModel := pyStrOf(meta["api_model"])
	recordTokenUsage(ctx, response.Usage, apiModel, "openai", "")
	if len(response.Data) == 0 {
		return fail("OpenAI returned no image data", "empty_response"), nil
	}
	first := response.Data[0]
	imageRef, failure := materializeImage(ctx, env, openAIItemString(first, "b64_json"), openAIItemString(first, "url"),
		materializeOptions{Prefix: "openai_" + tierID, Label: "OpenAI", Provider: "openai", Model: tierID, Prompt: prompt, Aspect: aspect, Log: logOf(env)})
	if failure != nil {
		return failure, nil
	}
	extra := map[string]any{"size": size, "quality": meta["quality"]}
	if revised := openAIItemField(first, "revised_prompt"); pyTruthy(revised) {
		extra["revised_prompt"] = revised
	}
	modality := "text"
	if isEdit {
		modality = "image"
	}
	return imageSuccessResponse(imageRef, tierID, prompt, aspect, "openai", modality, extra), nil
}

func init() {
	plugins.RegisterBundled(imageGenBackendPlugin{name: "openai", requiresEnv: []string{"OPENAI_API_KEY"},
		description: "OpenAI image generation backend (GPT Image 2 and GPT Image 2.5 Flare/Sunburst). Saves generated images to $ICEBATH_HOME/cache/generated/images/.",
		provider:    func() ImageGenProvider { return newOpenAIImageGenProvider() }})
}
