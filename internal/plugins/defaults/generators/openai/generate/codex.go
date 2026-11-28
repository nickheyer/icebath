package generate

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/generation"
	openaiconfig "github.com/nickheyer/icebath/internal/plugins/defaults/generators/openai/config"
	openaicodexconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/openaicodex/config"
	codexmodels "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/openaicodex/models"
	"github.com/nickheyer/icebath/internal/provider"
	"github.com/nickheyer/icebath/pkg/strs"
)

const (
	codexReferences = 16
	// Largest picture the edits endpoint takes, and the characters of an error body kept
	codexMaxImage   = 25 << 20
	codexMaxError   = 500
	codexNoAuth     = "No Codex/ChatGPT OAuth credentials available. Sign in to OpenAI via Codex in /providers (or run `icebath auth add openai-codex`)."
	codexImageModel = "gpt-image-2"
)

// Types of picture the edits endpoint takes
var codexTypes = strs.Set("image/png", "image/jpeg", "image/gif", "image/webp")

// Route of the Codex sign-in, false when there is none with a token
func (s services) codex(cfg *config.Config) (provider.Route, bool) {
	route, ok := s.creds.Kind(s.providers, cfg, openaicodexconfig.Name)
	if !ok {
		return provider.Route{}, false
	}
	if route.BaseURL == "" {
		route.BaseURL = openaicodexconfig.BaseURL
	}
	return route, true
}

// Draws or edits with gpt-image-2 through the native images endpoints of the Codex backend, keeping the PNG in the cache
func (s services) drawCodex(ctx context.Context, job *generation.Job, request generation.ImageRequest) generation.Result {
	cfg := job.Config
	prompt, aspect := strings.TrimSpace(request.Prompt), generation.Aspect(request.Aspect)
	if prompt == "" {
		return generation.Call{Provider: openaiconfig.Codex, Aspect: aspect}.Fail("Prompt is required and must be a non-empty string", "invalid_argument")
	}
	route, ok := s.codex(cfg)
	if !ok {
		return generation.Call{Provider: openaiconfig.Codex, Aspect: aspect}.Fail(codexNoAuth, "auth_required")
	}
	t := pick(tiers, request.Model, false)
	call := generation.Call{Provider: openaiconfig.Codex, Model: t.id, Prompt: prompt, Aspect: aspect}
	size := generation.Size(aspect)
	var images []any
	for _, source := range request.Sources(codexReferences) {
		data, err := codexInput(ctx, job, source)
		if err != nil {
			return call.Fail("Invalid image input for Codex image editing: "+err.Error(), "invalid_image_input")
		}
		images = append(images, map[string]any{"image_url": data})
	}
	body := map[string]any{"prompt": prompt, "model": codexImageModel, "n": 1, "quality": t.quality, "size": size, "background": "opaque"}
	reply, id, status, err := postCodex(ctx, route, body, images)
	if status == 401 && route.Keeper != nil && route.Keeper.Recover != nil {
		// A sign-in the backend refused is renewed, or the next account taken, and the call made once more
		if renewed, ok := route.Keeper.Recover(ctx, &provider.APIError{Provider: route.Provider, Model: codexImageModel, StatusCode: status, Message: err.Error()}); ok {
			reply, id, _, err = postCodex(ctx, renewed, body, images)
		}
	}
	if err != nil {
		slog.Debug("Codex image generation failed", "error", err)
		return call.Fail("OpenAI image generation via Codex auth failed: "+err.Error(), "api_error")
	}
	b64 := ""
	if data := generation.List(reply, "data"); len(data) > 0 {
		b64 = generation.Text(data[0], "b64_json")
	}
	if b64 == "" {
		return call.Fail("Codex images API response contained no image data", "empty_response")
	}
	raw, err := generation.Base64(b64)
	if err != nil {
		return call.Fail("Could not save image to cache: "+err.Error(), "io_error")
	}
	path, err := job.Save(generation.Image, raw, "openai_codex_"+t.id, "png")
	if err != nil {
		return call.Fail("Could not save image to cache: "+err.Error(), "io_error")
	}
	modality := "text"
	if len(images) > 0 {
		modality = "image"
	}
	return call.Made(path, modality, map[string]any{"size": size, "quality": t.quality, "input_image_count": len(images), "requested_size": size,
		"pixel_size": pixels(raw), "reported_quality": reply["quality"], "reported_size": reply["size"], "imagegen_request_id": strs.Nullable(id)})
}

// Data URL of a source the edits endpoint takes: a URL fetched under the private address rules, a data URL decoded and checked, a file of
// this machine read; at most 25 MB of a picture type it takes
func codexInput(ctx context.Context, job *generation.Job, source string) (string, error) {
	source = strings.TrimSpace(source)
	lower := strings.ToLower(source)
	var data []byte
	var tooBig, unsupported string
	switch {
	case source == "":
		return "", errors.New("Blank image input")
	case strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://"):
		fetched, err := job.Fetch(ctx, source, 60*time.Second, codexMaxImage)
		if errors.Is(err, generation.ErrUnsafe) {
			return "", errors.New("Image URL failed the SSRF safety check: " + source)
		}
		if err != nil {
			return "", err
		}
		data, tooBig, unsupported = fetched, "Image URL exceeds 25MB cap: "+source, "Image URL did not return a supported image: "+source
	case strings.HasPrefix(lower, "data:"):
		header, _, _ := strings.Cut(lower, ",")
		if !strings.HasPrefix(header, "data:image/") || !strings.Contains(header, ";base64") {
			return "", errors.New("Only base64 data:image URLs are supported as Codex image inputs")
		}
		decoded, _, err := generation.Inline(source)
		if err != nil {
			return "", err
		}
		data, tooBig, unsupported = decoded, "Image data URL exceeds 25MB cap", "Image data URL does not contain supported image bytes"
	default:
		read, err := job.Local(source)
		if err != nil {
			return "", err
		}
		if len(read) == 0 {
			return "", errors.New("Image input path is empty: " + source)
		}
		data, tooBig, unsupported = read, "Image input path exceeds 25MB cap: "+source, "Image input path is not a supported image: "+source
	}
	if len(data) > codexMaxImage {
		return "", errors.New(tooBig)
	}
	kind := generation.Sniff(data)
	if !codexTypes[kind] {
		return "", errors.New(unsupported)
	}
	return generation.DataURL(kind, data), nil
}

// Posts to images/edits when there are pictures, images/generations otherwise; the reply, the backend's id of the request and the status
// of a failed one
func postCodex(ctx context.Context, route provider.Route, body map[string]any, images []any) (map[string]any, string, int, error) {
	path := "images/generations"
	if len(images) > 0 {
		body["images"], path = images, "images/edits"
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, "", 0, err
	}
	base := strings.TrimRight(strings.TrimSpace(route.BaseURL), "/")
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/"+path, bytes.NewReader(encoded))
	if err != nil {
		return nil, "", 0, err
	}
	for name, value := range codexmodels.Headers(route.APIKey, base) {
		request.Header.Set(name, value)
	}
	for name, value := range route.Headers {
		request.Header.Set(name, value)
	}
	request.Header.Set("Authorization", "Bearer "+route.APIKey)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("x-codex-image-turn-id", uuid.NewString())
	client := &http.Client{Timeout: 300 * time.Second, Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, DialContext: (&net.Dialer{Timeout: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout: 30 * time.Second, ResponseHeaderTimeout: 300 * time.Second, ForceAttemptHTTP2: true}}
	response, err := client.Do(request)
	if err != nil {
		return nil, "", 0, err
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, "", 0, err
	}
	if response.StatusCode >= 400 {
		return nil, "", response.StatusCode, fmt.Errorf("Codex images API returned HTTP %d: %s", response.StatusCode, summary(raw))
	}
	var reply map[string]any
	if err := json.Unmarshal(raw, &reply); err != nil {
		return nil, "", 0, err
	}
	if reply == nil {
		return nil, "", 0, errors.New("Codex images API returned a non-object body")
	}
	if route.Keeper != nil && route.Keeper.Sent != nil {
		route.Keeper.Sent()
	}
	return reply, strings.Join(response.Header.Values("x-codex-imagegen-request-id"), ", "), 0, nil
}

// Message of an error body clipped, else the body clipped
func summary(raw []byte) string {
	var reply struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &reply) == nil && strings.TrimSpace(reply.Error.Message) != "" {
		return strs.Head(strings.TrimSpace(reply.Error.Message), codexMaxError)
	}
	return strs.Head(string(raw), codexMaxError)
}

// Width x height of a PNG, null for anything else
func pixels(raw []byte) any {
	if len(raw) < 24 || string(raw[:8]) != "\x89PNG\r\n\x1a\n" || string(raw[12:16]) != "IHDR" {
		return nil
	}
	return fmt.Sprintf("%dx%d", binary.BigEndian.Uint32(raw[16:20]), binary.BigEndian.Uint32(raw[20:24]))
}
