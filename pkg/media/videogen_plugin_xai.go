package media

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyhttpx"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pymimetypes"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	xaiVideoDefaultBaseURL        = "https://api.x.ai/v1"
	xaiVideoDefaultTextModel      = "grok-imagine-video"
	xaiVideoDefaultImageModel     = "grok-imagine-video-1.5"
	xaiVideoDefaultDuration       = 8
	xaiVideoDefaultExtendDuration = 6
	xaiVideoTimeoutSeconds        = 240
	xaiVideoPollIntervalSeconds   = 5
	xaiVideoMaxReferenceImages    = 7
	xaiVideoPublicURLHint         = "(e.g. the `image`/`public_url` from a prior Imagine result)"
	xaiVideoAuthRequiredMsg       = "No xAI credentials found. Sign in via `icebath auth add xai-oauth` " +
		"(SuperGrok / Premium+) or set XAI_API_KEY from https://console.x.ai/."
)

var xaiVideoAspectRatios = []string{"1:1", "16:9", "2:3", "3:2", "3:4", "4:3", "9:16"}

var xaiVideoResolutions = []string{"480p", "720p"}

var xaiVideoTerminalStatuses = map[string]bool{"done": true, "failed": true, "error": true, "expired": true, "cancelled": true}

var xaiImageToVideoCompatModelIDs = map[string]bool{"grok-imagine-video-1.5-preview": true, "grok-imagine-video-1.5-2026-05-30": true}

var xaiVideoModelOrder = []string{"grok-imagine-video", "grok-imagine-video-1.5"}

var xaiVideoModels = map[string]map[string]any{
	"grok-imagine-video": {"display": "Grok Imagine Video", "speed": "~60-240s", "strengths": "Text-to-video; legacy image-to-video fallback.",
		"price": "see https://docs.x.ai/developers/models/grok-imagine-video", "modalities": []any{"text", "image"}},
	"grok-imagine-video-1.5": {"display": "Grok Imagine Video 1.5", "speed": "~60-240s", "strengths": "Latest xAI image-to-video model.",
		"price": "see https://docs.x.ai/developers/pricing", "modalities": []any{"image"}},
}

// HTTP client the xAI video requests use, tests swap it for a recording transport
var xaiVideoHTTPClient = func() *http.Client { return pyhttpx.PlainClient(false) }

// Sleep between xAI job polls, tests replace it to skip the wait
var xaiVideoSleep = videoPollSleep

// Result of an xai_http helper, the fallback when it panics, logged when a format is given
func videoXAIHTTP[T any](fallback T, logFormat string, helper func() T) (out T) {
	defer func() {
		if r := recover(); r != nil {
			if logFormat != "" {
				videoGenLog.Debug(fmt.Sprintf(logFormat, r))
			}
			out = fallback
		}
	}()
	return helper()
}

// API key and base URL from the xAI resolver, an empty key meaning no credentials
func resolveXAIVideoCredentials(env *tools.Env) (string, string) {
	creds := videoXAIHTTP([2]string{}, "xAI credential resolver failed: %v", func() [2]string {
		_, key, base := tools.ResolveXAIHTTPCredentials(env, false)
		return [2]string{key, base}
	})
	apiKey, baseURL := creds[0], creds[1]
	if baseURL == "" {
		baseURL = xaiVideoDefaultBaseURL
	}
	return strings.TrimSpace(apiKey), strings.TrimRight(strings.TrimSpace(baseURL), "/")
}

func videoXAIHeaders(apiKey string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + apiKey, "Content-Type": "application/json", "User-Agent": videoXAIHTTP("icebath-agent/video_gen", "", tools.IcebathXAIUserAgent)}
}

func videoXAIError(errText, errorType, prompt, model, aspect string) map[string]any {
	return videoErrorResponse(errText, errorType, "xai", model, prompt, aspect)
}

// Whether the value starts with any prefix ignoring case
func hasLowerPrefix(value string, prefixes ...string) bool {
	lowered := strings.ToLower(value)
	for _, p := range prefixes {
		if strings.HasPrefix(lowered, p) {
			return true
		}
	}
	return false
}

// Whether the path names an existing regular file after symlinks
func isRegularFile(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.Mode().IsRegular()
}

// Remote URL or matching data URI as is, a readable local file of the kind inlined as base64 after the read guard
func mediaRefToXAIURL(env *tools.Env, value, kind, fallbackMime string) (string, error) {
	ref := strings.TrimSpace(value)
	path := pypath.Expanduser(ref)
	if ref == "" || hasLowerPrefix(ref, "http://", "https://", "data:"+kind+"/") || !isRegularFile(path) {
		return ref, nil
	}
	if blocked := tools.ReadBlockError(env, ref); blocked != "" {
		return "", &imageValueError{blocked}
	}
	mime, _, _ := pymimetypes.GuessType(filepath.Base(path), true)
	if mime == "" {
		mime = fallbackMime
	}
	if !strings.HasPrefix(mime, kind+"/") {
		return ref, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(raw), nil
}

// xAI image input of a URL, data URI or local image, nil when the reference is not usable
func imageRefToXAIInput(env *tools.Env, value string) (map[string]any, error) {
	ref, err := mediaRefToXAIURL(env, value, "image", "application/octet-stream")
	if err != nil {
		return nil, err
	}
	if ref != "" && hasLowerPrefix(ref, "http://", "https://", "data:image/") {
		return map[string]any{"url": ref}, nil
	}
	return nil, nil
}

// xAI video input carrying a public URL in the url field, a local file inlined, nil otherwise
func videoInputFromPublicURL(env *tools.Env, value string) (map[string]any, error) {
	ref := strings.TrimSpace(value)
	if ref != "" && isRegularFile(pypath.Expanduser(ref)) {
		u, err := mediaRefToXAIURL(env, ref, "video", "video/mp4")
		if err != nil {
			return nil, err
		}
		return map[string]any{"url": u}, nil
	}
	if hasLowerPrefix(ref, "http://", "https://") {
		return map[string]any{"url": ref}, nil
	}
	return nil, nil
}

// Duration clamped to one through max seconds, reference to video capped at ten
func xaiClampDuration(duration *int, hasReferenceImages bool, maxSeconds, def int) int {
	d := def
	if duration != nil {
		d = *duration
	}
	limit := maxSeconds
	if hasReferenceImages {
		limit = 10
	}
	return min(max(1, min(maxSeconds, d)), limit)
}

// Text or image model for the modality, an explicit tool model winning over configuration
func xaiResolveModelForModality(model, modality string, explicitModel bool) string {
	requested := strings.TrimSpace(model)
	if explicitModel && requested != "" {
		return requested
	}
	if modality == "image" {
		return xaiVideoDefaultImageModel
	}
	if requested == xaiVideoDefaultImageModel || xaiImageToVideoCompatModelIDs[requested] || requested == "" {
		return xaiVideoDefaultTextModel
	}
	return requested
}

// xAI Grok Imagine video backend
type xaiVideoGenProvider struct {
	provider.CatalogProviderDefaults
}

func newXAIVideoGenProvider() *xaiVideoGenProvider {
	p := &xaiVideoGenProvider{}
	p.Self = p
	return p
}

func (p *xaiVideoGenProvider) Name() string        { return "xai" }
func (p *xaiVideoGenProvider) DisplayName() string { return "xAI" }
func (p *xaiVideoGenProvider) IsAvailable() bool   { return hasXAIVideoCredentials(nil) }
func (p *xaiVideoGenProvider) DefaultModel() string {
	return xaiVideoDefaultTextModel
}

func (p *xaiVideoGenProvider) ListModels() []map[string]any {
	rows := make([]map[string]any, 0, len(xaiVideoModelOrder))
	for _, id := range xaiVideoModelOrder {
		row := map[string]any{"id": id}
		for k, v := range xaiVideoModels[id] {
			row[k] = v
		}
		rows = append(rows, row)
	}
	return rows
}

func (p *xaiVideoGenProvider) GetSetupSchema() map[string]any {
	tag := "grok-imagine-video for text/reference; grok-imagine-video-1.5 for image-to-video; edit/extend: pass the stored public " +
		"HTTPS MP4 (`video` / `public_url` from a prior Imagine result); uses xAI Grok OAuth or XAI_API_KEY"
	if notice := videoXAIHTTP("", "", func() string { return xaiStorageNoticeText(pluginEnv(), "video_gen") }); notice != "" {
		tag += ". " + notice
	}
	return map[string]any{"name": "xAI Grok Imagine", "badge": "paid", "tag": tag, "env_vars": []any{}, "post_setup": "xai_grok"}
}

func (p *xaiVideoGenProvider) Capabilities() map[string]any {
	return map[string]any{"modalities": []any{"text", "image"}, "aspect_ratios": stringsToAny(xaiVideoAspectRatios),
		"resolutions": stringsToAny(xaiVideoResolutions), "max_duration": 15, "min_duration": 1, "supports_audio": false,
		"supports_negative_prompt": false, "supports_seed": true, "supports_upscale": false, "max_reference_images": xaiVideoMaxReferenceImages}
}

// Text, image or reference to video with the configured model, never a per request override
func (p *xaiVideoGenProvider) Generate(ctx context.Context, env *tools.Env, req VideoGenRequest) (map[string]any, error) {
	if env == nil {
		env = pluginEnv()
	}
	aspect := req.aspect()
	return runXAIVideo(env, "generation", req.Prompt, req.Model, aspect, func(apiKey, baseURL string) (map[string]any, error) {
		return generateXAIVideo(ctx, env, apiKey, baseURL, req.Prompt, req.Model, false, req.ImageURL, req.ReferenceImageURLs,
			req.Duration, aspect, req.resolution())
	}), nil
}

// Whether the xAI resolver yields a key
func hasXAIVideoCredentials(env *tools.Env) bool {
	key, _ := resolveXAIVideoCredentials(env)
	return key != ""
}

// Edits a stored xAI video with the prompt
func runXAIVideoEdit(ctx context.Context, env *tools.Env, prompt, videoURL, model string) map[string]any {
	return runXAIVideo(env, "edit", prompt, model, defaultVideoAspectRatio, func(apiKey, baseURL string) (map[string]any, error) {
		return mutateXAIVideo(ctx, env, apiKey, baseURL, prompt, videoURL, model, "edits", "edit", xaiVideoDefaultDuration)
	})
}

// Extends a stored xAI video by the clamped duration
func runXAIVideoExtend(ctx context.Context, env *tools.Env, prompt, videoURL string, duration *int, model string) map[string]any {
	clamped := xaiClampDuration(duration, false, 10, xaiVideoDefaultExtendDuration)
	return runXAIVideo(env, "extend", prompt, model, defaultVideoAspectRatio, func(apiKey, baseURL string) (map[string]any, error) {
		return mutateXAIVideo(ctx, env, apiKey, baseURL, prompt, videoURL, model, "extensions", "extend", clamped)
	})
}

// Resolves credentials then drives the flow, an escaped error becoming an api_error envelope
func runXAIVideo(env *tools.Env, label, prompt, model, aspect string, flow func(apiKey, baseURL string) (map[string]any, error)) map[string]any {
	apiKey, baseURL := resolveXAIVideoCredentials(env)
	if apiKey == "" {
		return videoXAIError(xaiVideoAuthRequiredMsg, "auth_required", prompt, "", "")
	}
	res, err := flow(apiKey, baseURL)
	if err != nil {
		videoGenLog.Warn(fmt.Sprintf("xAI video %s unexpected failure: %v", label, err))
		if model == "" {
			model = xaiVideoDefaultTextModel
		}
		return videoXAIError(fmt.Sprintf("xAI video %s failed: %v", label, err), "api_error", prompt, model, aspect)
	}
	return res
}

// Validates inputs in contract order, soft clamps enums and submits one generation job
func generateXAIVideo(ctx context.Context, env *tools.Env, apiKey, baseURL, prompt, model string, explicitModel bool, imageURL string,
	referenceImageURLs []string, duration *int, aspect, resolution string) (map[string]any, error) {
	prompt, imageURL = strings.TrimSpace(prompt), strings.TrimSpace(imageURL)
	var imageInput map[string]any
	if imageURL != "" {
		var err error
		if imageInput, err = imageRefToXAIInput(env, imageURL); err != nil {
			return nil, err
		}
	}
	var refs []map[string]any
	allRefs := true
	for _, u := range referenceImageURLs {
		if strings.TrimSpace(u) == "" {
			continue
		}
		in, err := imageRefToXAIInput(env, strings.TrimSpace(u))
		if err != nil {
			return nil, err
		}
		allRefs = allRefs && in != nil
		refs = append(refs, in)
	}
	checks := []struct {
		bad              bool
		message, errType string
	}{
		{imageURL != "" && imageInput == nil, "image_url must be a public HTTPS URL or data URI " + xaiVideoPublicURLHint, "invalid_image_url"},
		{!allRefs, "reference_image_urls must be public HTTPS URLs or data URIs " + xaiVideoPublicURLHint, "invalid_reference_image_urls"},
		{prompt == "", "prompt is required for xAI video generation", "missing_prompt"},
		{len(refs) > xaiVideoMaxReferenceImages, fmt.Sprintf("reference_image_urls supports at most %d images on xAI", xaiVideoMaxReferenceImages), "too_many_references"},
		{imageInput != nil && len(refs) > 0, "image_url and reference_image_urls cannot be combined on xAI", "conflicting_inputs"},
	}
	for _, c := range checks {
		if c.bad {
			return videoXAIError(c.message, c.errType, prompt, "", ""), nil
		}
	}
	if aspect = strings.TrimSpace(aspect); aspect == "" || !containsString(xaiVideoAspectRatios, aspect) {
		aspect = defaultVideoAspectRatio
	}
	if resolution = strings.ToLower(strings.TrimSpace(resolution)); !containsString(xaiVideoResolutions, resolution) {
		resolution = defaultVideoResolution
	}
	modality := "text"
	if len(refs) > 0 {
		modality = "reference"
	} else if imageInput != nil {
		modality = "image"
	}
	resolved := xaiResolveModelForModality(model, modality, explicitModel)
	if len(refs) > 0 && resolved != xaiVideoDefaultTextModel {
		if explicitModel {
			return videoXAIError(fmt.Sprintf("xAI reference-to-video requires %s; got %s", xaiVideoDefaultTextModel, resolved),
				"unsupported_model", prompt, resolved, ""), nil
		}
		resolved = xaiVideoDefaultTextModel
	}
	clamped := xaiClampDuration(duration, len(refs) > 0, 15, xaiVideoDefaultDuration)
	payload := pyjson.ObjectOf("model", resolved, "prompt", prompt, "duration", clamped, "aspect_ratio", aspect, "resolution", resolution)
	if imageInput != nil {
		payload.Set("image", imageInput)
	}
	if len(refs) > 0 {
		list := make([]any, len(refs))
		for i, r := range refs {
			list[i] = r
		}
		payload.Set("reference_images", list)
	}
	return submitXAIVideoPayload(ctx, env, apiKey, baseURL, "generations", payload, modality, "generate", aspect, clamped, resolution)
}

// Edit or extend using a public HTTPS video URL sent as video.url
func mutateXAIVideo(ctx context.Context, env *tools.Env, apiKey, baseURL, prompt, videoURL, model, endpoint, operation string, duration int) (map[string]any, error) {
	prompt = strings.TrimSpace(prompt)
	videoInput, err := videoInputFromPublicURL(env, videoURL)
	if err != nil {
		return nil, err
	}
	if prompt == "" {
		return videoXAIError("prompt is required for xAI video edit/extend", "missing_prompt", prompt, "", ""), nil
	}
	if videoInput == nil {
		return videoXAIError("video_url must be a public HTTPS MP4 URL (the `video`/`public_url` from a prior Imagine result)", "missing_video", prompt, "", ""), nil
	}
	payload := pyjson.ObjectOf("model", xaiResolveModelForModality(model, "text", model != ""), "prompt", prompt, "video", videoInput)
	if endpoint == "extensions" {
		payload.Set("duration", duration)
	}
	return submitXAIVideoPayload(ctx, env, apiKey, baseURL, endpoint, payload, operation, operation, defaultVideoAspectRatio, duration, "")
}

// One xAI request with httpx timeouts, the decoded JSON object and a raise_for_status style error
func xaiVideoRequest(ctx context.Context, client *http.Client, method, rawURL string, headers map[string]string, body []byte, timeout time.Duration) (*pyhttpx.Response, error) {
	var reader *bytes.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	var req *http.Request
	var err error
	if reader != nil {
		req, err = http.NewRequestWithContext(ctx, method, rawURL, reader)
	} else {
		req, err = http.NewRequestWithContext(ctx, method, rawURL, nil)
	}
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return pyhttpx.Do(client, req, timeout)
}

// HTTPStatusError of a non 2xx httpx response
type xaiHTTPStatusError struct{ msg string }

func (e *xaiHTTPStatusError) Error() string { return e.msg }

// raise_for_status of an httpx response
func xaiRaiseForStatus(resp *pyhttpx.Response) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	return &xaiHTTPStatusError{pyhttpx.RaiseForStatusMessage(resp.StatusCode, resp.URL, resp.Header.Get("Location"))}
}

// JSON object of a response body, the AttributeError text get raises for any other JSON value
func xaiJSONObject(resp *pyhttpx.Response) (*pyjson.Object, error) {
	parsed, err := resp.JSON()
	if err != nil {
		return nil, err
	}
	obj, ok := pyjson.AsObject(parsed)
	if !ok {
		return nil, fmt.Errorf("'%s' object has no attribute 'get'", pyrepr.TypeName(parsed))
	}
	return obj, nil
}

// Posts the payload, polls the request to a terminal status and shapes the success or failure envelope
func submitXAIVideoPayload(ctx context.Context, env *tools.Env, apiKey, baseURL, endpoint string, payload *pyjson.Object,
	modality, operation, aspect string, duration int, resolution string) (map[string]any, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	prompt, _ := mustGet(payload, "prompt").(string)
	resolvedModel, _ := mustGet(payload, "model").(string)
	storageOptions, storageNotice, storageEnabled := func() (opts map[string]any, notice string, enabled bool) {
		defer func() {
			if recover() != nil {
				opts, notice, enabled = nil, "", false
			}
		}()
		return buildXAIStorageOptions(env, "video_gen", "icebath-xai-video", "mp4"), maybeMarkXAIStorageNoticeSeen(env, "video_gen"),
			readXAIStorageConfig(env, "video_gen").Enabled
	}()
	if storageOptions != nil {
		payload.Set("storage_options", storageOptions)
	}
	headers := videoXAIHeaders(apiKey)
	client := xaiVideoHTTPClient()
	defer client.CloseIdleConnections()
	body, err := pyjson.Dumps(payload)
	if err != nil {
		return nil, err
	}
	submitHeaders := map[string]string{"x-idempotency-key": uuid4()}
	for k, v := range headers {
		submitHeaders[k] = v
	}
	resp, err := xaiVideoRequest(ctx, client, http.MethodPost, baseURL+"/videos/"+endpoint, submitHeaders, []byte(body), 60*time.Second)
	if err != nil {
		return nil, err
	}
	if serr := xaiRaiseForStatus(resp); serr != nil {
		detail := resp.Text()
		if r := []rune(detail); len(r) > 500 {
			detail = string(r[:500])
		}
		if detail == "" {
			detail = serr.Error()
		}
		return videoXAIError(fmt.Sprintf("xAI submit failed (%d): %s", resp.StatusCode, detail), "api_error", prompt, resolvedModel, ""), nil
	}
	submitted, err := xaiJSONObject(resp)
	if err != nil {
		return nil, err
	}
	requestID := mustGet(submitted, "request_id")
	if !pyTruthy(requestID) {
		return nil, fmt.Errorf("xAI video response did not include request_id")
	}
	reqID := pyrepr.Str(requestID)
	status, job := "queued", pyjson.NewObject()
	finished := false
	for elapsed := 0; elapsed < xaiVideoTimeoutSeconds; elapsed += xaiVideoPollIntervalSeconds {
		resp, err := xaiVideoRequest(ctx, client, http.MethodGet, baseURL+"/videos/"+reqID, headers, nil, 30*time.Second)
		if err != nil {
			return nil, err
		}
		if err := xaiRaiseForStatus(resp); err != nil {
			return nil, err
		}
		if job, err = xaiJSONObject(resp); err != nil {
			return nil, err
		}
		status = ""
		if v := mustGet(job, "status"); pyTruthy(v) {
			s, ok := v.(string)
			if !ok {
				return nil, fmt.Errorf("'%s' object has no attribute 'lower'", pyrepr.TypeName(v))
			}
			status = strings.ToLower(s)
		}
		if xaiVideoTerminalStatuses[status] {
			finished = true
			break
		}
		if err := xaiVideoSleep(ctx, xaiVideoPollIntervalSeconds*time.Second); err != nil {
			return nil, err
		}
	}
	if !finished {
		return videoXAIError(fmt.Sprintf("Timed out waiting for xAI video request after %ds", xaiVideoTimeoutSeconds), "timeout", prompt, resolvedModel, ""), nil
	}
	plain, _ := pyjson.Plain(job).(map[string]any)
	modelOut := resolvedModel
	if pyTruthy(plain["model"]) {
		modelOut = pyrepr.Str(plain["model"])
	}
	if status != "done" {
		var message any
		errField := plain["error"]
		if !pyTruthy(errField) {
			errField = map[string]any{}
		}
		em, ok := errField.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("'%s' object has no attribute 'get'", pyrepr.TypeName(errField))
		}
		message = em["message"]
		if !pyTruthy(message) {
			message = plain["message"]
		}
		text := fmt.Sprintf("xAI video request ended with status '%s'", status)
		if pyTruthy(message) {
			text = pyrepr.Str(message)
		}
		return videoXAIError(text, "xai_"+status, prompt, resolvedModel, ""), nil
	}
	video, _ := plain["video"].(map[string]any)
	if video == nil {
		video = map[string]any{}
	}
	fileOutput, _ := video["file_output"].(map[string]any)
	if fileOutput == nil {
		fileOutput = map[string]any{}
	}
	stripped := func(v any) string {
		s, _ := v.(string)
		return strings.TrimSpace(s)
	}
	storedPublic, temporary := stripped(fileOutput["public_url"]), stripped(video["url"])
	publicVideoURL := storedPublic
	if publicVideoURL == "" {
		publicVideoURL = temporary
	}
	if publicVideoURL == "" {
		return videoXAIError("xAI video request completed without a video URL", "empty_response", prompt, modelOut, ""), nil
	}
	extra := map[string]any{"request_id": requestID, "operation": operation, "storage_enabled": storageEnabled}
	if resolution != "" {
		extra["resolution"] = resolution
	}
	if storageNotice != "" {
		extra["storage_notice"] = storageNotice
	}
	if storedPublic != "" {
		extra["public_url"] = storedPublic
		if temporary != "" && temporary != storedPublic {
			extra["temporary_url"] = temporary
		}
	}
	for _, k := range []string{"filename", "expires_at", "public_url_expires_at", "public_url_error", "storage_error"} {
		if v, ok := fileOutput[k]; ok {
			extra[k] = v
		}
	}
	if pyTruthy(plain["usage"]) {
		extra["usage"] = plain["usage"]
	}
	durationOut := duration
	if v := video["duration"]; pyTruthy(v) {
		n, err := pyint.Of(v)
		if err != nil {
			return nil, err
		}
		durationOut = int(n.Int64())
	}
	return videoSuccessResponse(publicVideoURL, modelOut, prompt, modality, aspect, durationOut, "xai", extra), nil
}

func init() {
	plugins.RegisterBundled(videoGenBackendPlugin{name: "xai", author: "NousResearch", requiresEnv: []string{"XAI_API_KEY"},
		description: "xAI Grok Imagine video generation backend. Supports text-to-video, image-to-video, reference-to-video, video editing, video extension, and stored public URLs via the xAI async videos API.",
		provider:    func() VideoGenProvider { return newXAIVideoGenProvider() }})
}
