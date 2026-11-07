package media

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/nickheyer/icebath/pkg/internal/pybase64"
	"github.com/nickheyer/icebath/pkg/internal/pyhttpx"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	codexMaxErrorBodyChars  = 500
	codexMaxReferenceImages = 16
	codexMaxInputImageBytes = 25 * 1024 * 1024
	codexNoAuth             = "No Codex/ChatGPT OAuth credentials available. Run " +
		"`icebath auth add openai-codex` (or `icebath setup` → Codex) to sign in."
)

// Raster types the Codex edit endpoint accepts, the shared sniffer also knowing SVG, TIFF and ICO
var codexAcceptedInputMime = map[string]bool{"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true}

var codexImageLog = slog.Default().With("logger", "plugins.image_gen.openai-codex")

// httpx.Client construction seam of the images request, nil when no HTTP client is linked
var codexImagesHTTPClient = func() *http.Client { return pyhttpx.PlainClient(false) }

// httpx.Timeout(300, connect=30, read=300, write=60, pool=30) of the images request
var codexImagesTimeout = pyhttpx.Timeout{Connect: 30 * time.Second, Read: 300 * time.Second, Write: 60 * time.Second, Pool: 30 * time.Second}

// Error message of a parsed error object clipped to the cap, else the clipped raw body
func summarizeCodexErrorBody(body string) string {
	if payload, err := pyjson.Loads(body); err == nil {
		if obj, ok := pyjson.AsObject(payload); ok {
			errVal, _ := obj.Get("error")
			if eo, ok := pyjson.AsObject(errVal); ok {
				if msg, ok := eo.Vals["message"].(string); ok && pystr.Strip(msg) != "" {
					return clipRunes(pystr.Strip(msg), codexMaxErrorBodyChars)
				}
			}
		}
	}
	return clipRunes(body, codexMaxErrorBodyChars)
}

// Selected gpt-image-2 tier from OPENAI_IMAGE_MODEL or the openai-codex config section
func resolveCodexImageModel(env *tools.Env) (string, map[string]any) {
	return resolveStaticModel(env, gptImage2Tiers, gptImage2Default, staticModelOptions{EnvVar: "OPENAI_IMAGE_MODEL", ConfigKey: "openai-codex"})
}

// Codex token and the base URL its credential routes to, both empty when no usable token resolves
func readCodexImageCredential(ctx context.Context) (token, baseURL string) {
	defer func() {
		if r := recover(); r != nil {
			codexImageLog.Debug(fmt.Sprintf("Could not resolve Codex credential: %v", r))
			token, baseURL = "", ""
		}
	}()
	tok, base, err := provider.ResolveCodexCredentialAndBase(ctx)
	if err != nil {
		codexImageLog.Debug(fmt.Sprintf("Could not resolve Codex credential: %v", err))
		return "", ""
	}
	if t := pystr.Strip(tok); t != "" {
		return t, base
	}
	return "", ""
}

// Whether an HTTP client for the images request is linked
func codexHTTPAvailable() bool { return codexImagesHTTPClient != nil }

// Accepted raster MIME of the bytes, empty for anything else
func sniffCodexImageMime(raw []byte) string {
	if m := tools.SniffImageMime(raw); codexAcceptedInputMime[m] {
		return m
	}
	return ""
}

// Canonical data URL of size and MIME checked image bytes
func encodeCodexInputImage(raw []byte, tooBig, unsupported string) (string, error) {
	if len(raw) > codexMaxInputImageBytes {
		return "", &imageValueError{tooBig}
	}
	mime := sniffCodexImageMime(raw)
	if mime == "" {
		return "", &imageValueError{unsupported}
	}
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(raw), nil
}

// Re-encoded data URL of a base64 data:image URL after strict decoding
func codexDataURLToInputImageURL(value string) (string, error) {
	header, data, ok := strings.Cut(value, ",")
	if !ok {
		return "", &imageValueError{"Image data URL is missing a comma separator"}
	}
	headerLC := strings.ToLower(header)
	if !strings.HasPrefix(headerLC, "data:image/") || !strings.Contains(headerLC, ";base64") {
		return "", &imageValueError{"Only base64 data:image URLs are supported as Codex image inputs"}
	}
	raw, err := pybase64.B64DecodeValidate(data)
	if err != nil {
		return "", err
	}
	return encodeCodexInputImage(raw, "Image data URL exceeds 25MB cap", "Image data URL does not contain supported image bytes")
}

// Data URL of a remote image fetched under the SSRF guard, the edit endpoint taking inline images only
func codexRemoteImageToDataURL(ctx context.Context, env *tools.Env, value string) (string, error) {
	if !tools.IsSafeURL(env, value) {
		return "", &imageValueError{"Image URL failed the SSRF safety check: " + value}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, value, nil)
	if err != nil {
		return "", fmt.Errorf("%s", pyhttpx.ErrorText(err))
	}
	resp, err := pyhttpx.Do(tools.NewSSRFSafeClient(env, 60*time.Second, true), req, 60*time.Second)
	if err != nil {
		return "", fmt.Errorf("%s", pyhttpx.ErrorText(err))
	}
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("%s", pyhttpx.RaiseForStatusMessage(resp.StatusCode, resp.URL, resp.Header.Get("Location")))
	}
	return encodeCodexInputImage(resp.Content, "Image URL exceeds 25MB cap: "+value, "Image URL did not return a supported image: "+value)
}

// Data URL of a local image after the read guard, existence and emptiness checks
func codexLocalImageToDataURL(env *tools.Env, value string) (string, error) {
	if blocked := tools.ReadBlockError(env, value); blocked != "" {
		return "", &imageValueError{blocked}
	}
	path, err := pypath.Resolve(pypath.Expanduser(value))
	if err != nil {
		return "", fmt.Errorf("%s", pypath.OSErrorText(err))
	}
	st, err := os.Stat(path)
	if err != nil || !st.Mode().IsRegular() {
		return "", &imageValueError{"Image input path does not exist or is not a file: " + value}
	}
	if st.Size() <= 0 {
		return "", &imageValueError{"Image input path is empty: " + value}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("%s", pypath.OSErrorText(err))
	}
	return encodeCodexInputImage(raw, "Image input path exceeds 25MB cap: "+value, "Image input path is not a supported image: "+value)
}

// images entry of a URL, data URL or local path for the edits endpoint
func codexToInputImage(ctx context.Context, env *tools.Env, value string) (*pyjson.Object, error) {
	candidate := pystr.Strip(value)
	if candidate == "" {
		return nil, &imageValueError{"Blank image input"}
	}
	lowered := strings.ToLower(candidate)
	var imageURL string
	var err error
	switch {
	case strings.HasPrefix(lowered, "http://") || strings.HasPrefix(lowered, "https://"):
		imageURL, err = codexRemoteImageToDataURL(ctx, env, candidate)
	case strings.HasPrefix(lowered, "data:"):
		imageURL, err = codexDataURLToInputImageURL(candidate)
	default:
		imageURL, err = codexLocalImageToDataURL(env, candidate)
	}
	if err != nil {
		return nil, err
	}
	return pyjson.ObjectOf("image_url", imageURL), nil
}

// images entries of the primary and reference sources up to the sixteen the endpoint accepts
func normalizeCodexInputImages(ctx context.Context, env *tools.Env, imageURL string, refs []string) ([]any, error) {
	out := []any{}
	for _, v := range collectSourceImages(imageURL, refs, codexMaxReferenceImages) {
		entry, err := codexToInputImage(ctx, env, v)
		if err != nil {
			return nil, err
		}
		out = append(out, entry)
	}
	return out, nil
}

// Endpoint path and JSON body, images/edits when sources are present else images/generations
func buildCodexImageRequest(prompt, size string, quality any, inputImages []any) (string, *pyjson.Object) {
	body := pyjson.ObjectOf("prompt", prompt, "model", gptImage2APIModel, "n", 1, "quality", quality, "size", size, "background", "opaque")
	if len(inputImages) > 0 {
		body.Set("images", inputImages)
		return "images/edits", body
	}
	return "images/generations", body
}

// Posts to the native Codex images endpoint and returns the decoded body with the backend request id
func postCodexImageRequest(ctx context.Context, token, prompt, size string, quality any, inputImages []any, baseURL string) (*pyjson.Object, error) {
	baseURL = strings.TrimRight(pystr.Strip(baseURL), "/")
	headers := provider.CodexCloudflareHeaders(token, baseURL)
	headers["Authorization"] = "Bearer " + token
	headers["Content-Type"] = "application/json"
	headers["x-codex-image-turn-id"] = uuid.NewString()
	path, body := buildCodexImageRequest(prompt, size, quality, inputImages)
	encoded, err := pyjson.Dumps(body, pyjson.EnsureASCII(false), pyjson.Separators(",", ":"), pyjson.AllowNaN(false))
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/"+path, bytes.NewReader([]byte(encoded)))
	if err != nil {
		return nil, fmt.Errorf("%s", pyhttpx.ErrorText(err))
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := pyhttpx.DoTimeout(codexImagesHTTPClient(), req, codexImagesTimeout)
	if err != nil {
		return nil, fmt.Errorf("%s", pyhttpx.ErrorText(err))
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("Codex images API returned HTTP %d: %s", resp.StatusCode, summarizeCodexErrorBody(resp.Text()))
	}
	decoded, err := resp.JSON()
	if err != nil {
		return nil, err
	}
	payload, ok := pyjson.AsObject(decoded)
	if !ok {
		return nil, fmt.Errorf("Codex images API returned a non-object body")
	}
	var requestID any
	if vals := resp.Header.Values("x-codex-imagegen-request-id"); len(vals) > 0 {
		requestID = strings.Join(vals, ", ")
	}
	payload.Set("imagegen_request_id", requestID)
	return payload, nil
}

// Width x height of a PNG IHDR, false for anything that is not a PNG
func pngPixelSize(raw []byte) (string, bool) {
	if len(raw) < 24 || string(raw[:8]) != "\x89PNG\r\n\x1a\n" || string(raw[12:16]) != "IHDR" {
		return "", false
	}
	return fmt.Sprintf("%dx%d", binary.BigEndian.Uint32(raw[16:20]), binary.BigEndian.Uint32(raw[20:24])), true
}

// gpt-image-2 routed through ChatGPT or Codex OAuth instead of an API key
type openAICodexImageGenProvider struct {
	staticImageGenProvider
	env func() *tools.Env
}

func newOpenAICodexImageGenProvider() *openAICodexImageGenProvider {
	price := "varies"
	return &openAICodexImageGenProvider{env: pluginEnv, staticImageGenProvider: staticImageGenProvider{providerID: "openai-codex",
		label: "OpenAI (Codex auth)", models: gptImage2Tiers, defaultModelID: gptImage2Default, price: &price}}
}

// Available when a Codex token resolves and an HTTP client is linked
func (p *openAICodexImageGenProvider) IsAvailable() bool {
	return codexImageAvailable(context.Background())
}

// Codex token presence through the secret scope the context carries, plus the linked HTTP client
func codexImageAvailable(ctx context.Context) bool {
	token, _ := readCodexImageCredential(ctx)
	return token != "" && codexHTTPAvailable()
}

// Picker entry with no credential prompt, the Codex OAuth bootstrap hook starting the sign in
func (p *openAICodexImageGenProvider) GetSetupSchema() map[string]any {
	return map[string]any{
		"name":       "OpenAI (Codex auth)",
		"badge":      "free",
		"tag":        "gpt-image-2 via ChatGPT/Codex OAuth — no API key required; supports text and image inputs",
		"env_vars":   []any{},
		"post_setup": "openai_codex",
		"post_setup_hint": "Sign in with `icebath auth add openai-codex` (or `icebath setup` → Codex) " +
			"if you haven't already. No API key needed.",
	}
}

// Text and image modalities with sixteen source images
func (p *openAICodexImageGenProvider) Capabilities() map[string]any {
	return map[string]any{"modalities": []any{"text", "image"}, "max_reference_images": codexMaxReferenceImages}
}

// Generates or edits through the native Codex images endpoints and caches the PNG
func (p *openAICodexImageGenProvider) Generate(ctx context.Context, env *tools.Env, req ImageGenRequest) (map[string]any, error) {
	if env == nil {
		env = p.env()
	}
	prompt := pystr.Strip(req.Prompt)
	aspect := resolveAspectRatio(req.AspectRatio)
	if prompt == "" {
		return promptRequiredError("openai-codex", aspect), nil
	}
	token, baseURL := readCodexImageCredential(ctx)
	if token == "" {
		return errorFactory("openai-codex", aspect, "", "")(codexNoAuth, "auth_required"), nil
	}
	if !codexHTTPAvailable() {
		return errorFactory("openai-codex", aspect, "", "")("httpx Python package not installed (pip install httpx)", "missing_dependency"), nil
	}
	tierID, meta := resolveCodexImageModel(env)
	size := sizeFor(aspect)
	fail := errorFactory("openai-codex", aspect, tierID, prompt)
	inputImages, err := normalizeCodexInputImages(ctx, env, req.ImageURL, req.ReferenceImageURLs)
	if err != nil {
		return fail("Invalid image input for Codex image editing: "+err.Error(), "invalid_image_input"), nil
	}
	payload, err := postCodexImageRequest(ctx, token, prompt, size, meta["quality"], inputImages, baseURL)
	if err != nil {
		codexImageLog.Debug("Codex image generation failed", "error", err)
		return fail("OpenAI image generation via Codex auth failed: "+err.Error(), "api_error"), nil
	}
	data, _ := payload.Get("data")
	var b64 string
	if items, ok := data.([]any); ok && len(items) > 0 {
		if first, ok := pyjson.AsObject(items[0]); ok {
			b64, _ = first.Vals["b64_json"].(string)
		}
	}
	if b64 == "" {
		return fail("Codex images API response contained no image data", "empty_response"), nil
	}
	raw, err := pybase64.B64Decode(b64)
	if err != nil {
		return fail("Could not save image to cache: "+err.Error(), "io_error"), nil
	}
	var pixelSize any
	if s, ok := pngPixelSize(raw); ok {
		pixelSize = s
	}
	savedPath, err := saveB64Image(env, b64, "openai_codex_"+tierID, "png")
	if err != nil {
		return fail("Could not save image to cache: "+err.Error(), "io_error"), nil
	}
	modality := "text"
	if len(inputImages) > 0 {
		modality = "image"
	}
	reportedQuality, _ := payload.Get("quality")
	reportedSize, _ := payload.Get("size")
	requestID, _ := payload.Get("imagegen_request_id")
	return imageSuccessResponse(savedPath, tierID, prompt, aspect, "openai-codex", modality, map[string]any{
		"size": size, "quality": meta["quality"], "input_image_count": len(inputImages), "requested_size": size,
		"pixel_size": pixelSize, "reported_quality": reportedQuality, "reported_size": reportedSize, "imagegen_request_id": requestID,
	}), nil
}

func init() {
	plugins.RegisterBundled(imageGenBackendPlugin{name: "openai-codex",
		description: "OpenAI image generation backed by ChatGPT/Codex OAuth (gpt-image-2 via the native Codex images/generations and images/edits endpoints). Saves generated images to $ICEBATH_HOME/cache/generated/images/.",
		provider:    func() ImageGenProvider { return newOpenAICodexImageGenProvider() }})
}
