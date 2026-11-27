package generate

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/generation"
	xaiconfig "github.com/nickheyer/icebath/internal/plugins/defaults/generators/xai/config"
	"github.com/nickheyer/icebath/internal/web"
	"github.com/nickheyer/icebath/pkg/strs"
)

const (
	textVideoModel     = "grok-imagine-video"
	imageVideoModel    = "grok-imagine-video-1.5"
	defaultLength      = 8
	defaultExtension   = 6
	pollFor            = 240 * time.Second
	pollEvery          = 5 * time.Second
	maxVideoReferences = 7
	publicHint         = "(e.g. the `image`/`public_url` from a prior Imagine result)"
	noVideoKey         = "No xAI credentials found. Sign in via `icebath auth add xai-oauth` (SuperGrok / Premium+) or set XAI_API_KEY from https://console.x.ai/."
)

var (
	videoAspects     = []string{"1:1", "16:9", "2:3", "3:2", "3:4", "4:3", "9:16"}
	videoResolutions = []string{"480p", "720p"}
	ended            = strs.Set("done", "failed", "error", "expired", "cancelled")
	// Names of the image to video model the text model stands in for
	imageVideoAliases = strs.Set("grok-imagine-video-1.5-preview", "grok-imagine-video-1.5-2026-05-30")
)

var videoModels = []generation.Model{
	{ID: textVideoModel, Title: "Grok Imagine Video", Speed: "1 to 4 minutes", Strengths: "Video from text, and from an image the older way",
		Price: "See https://docs.x.ai/developers/models/grok-imagine-video", Modalities: []string{"text", "image"}},
	{ID: imageVideoModel, Title: "Grok Imagine Video 1.5", Speed: "1 to 4 minutes", Strengths: "The newest xAI model that turns an image into video",
		Price: "See https://docs.x.ai/developers/pricing", Modalities: []string{"image"}},
}

// Model for what the video is made from: one named explicitly as it is, else the image model for a still and the text model otherwise
func modelFor(model, modality string, explicit bool) string {
	requested := strings.TrimSpace(model)
	switch {
	case explicit && requested != "":
		return requested
	case modality == "image":
		return imageVideoModel
	case requested == "" || requested == imageVideoModel || imageVideoAliases[requested]:
		return textVideoModel
	}
	return requested
}

// Seconds asked for within one and max, ten at most when references guide the video
func length(asked *int, references bool, most, fallback int) int {
	d := fallback
	if asked != nil {
		d = *asked
	}
	limit := most
	if references {
		limit = 10
	}
	return min(max(1, min(most, d)), limit)
}

// Address xAI takes for a media source of the kind: a URL or matching data URL as it is, a file of the kind of this machine inlined; empty
// when it can take none
func (s *Services) media(job *generation.Job, source, kind, fallback string) (string, error) {
	ref := strings.TrimSpace(source)
	lower := strings.ToLower(ref)
	if ref == "" || strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") || strings.HasPrefix(lower, "data:"+kind+"/") {
		return ref, nil
	}
	if !fileExists(ref) {
		return "", nil
	}
	mime := generation.TypeOf(filepath.Base(ref))
	if mime == "" {
		mime = fallback
	}
	if !strings.HasPrefix(mime, kind+"/") {
		return "", nil
	}
	data, err := job.Local(ref)
	if err != nil {
		return "", err
	}
	return generation.DataURL(mime, data), nil
}

// Makes a video from text, a still or reference pictures with the model video_gen.model names, else the one for what it is made from
func (s *Services) Generate(ctx context.Context, job *generation.Job, request generation.VideoRequest) generation.Result {
	return s.run(ctx, job.Config, "generation", request.Prompt, request.Model, request.Aspect, func(bearer web.XAI) (generation.Result, error) {
		return s.generate(ctx, job, bearer, request)
	})
}

// Edits a video xAI stored
func (s *Services) Edit(ctx context.Context, job *generation.Job, prompt, video string) generation.Result {
	return s.run(ctx, job.Config, "edit", prompt, "", generation.DefaultVideoAspect, func(bearer web.XAI) (generation.Result, error) {
		return s.mutate(ctx, job, bearer, prompt, video, "edits", "edit", defaultLength)
	})
}

// Extends a video xAI stored by the seconds asked for, one to ten, six when none are
func (s *Services) Extend(ctx context.Context, job *generation.Job, prompt, video string, seconds *int) generation.Result {
	extension := length(seconds, false, 10, defaultExtension)
	return s.run(ctx, job.Config, "extend", prompt, "", generation.DefaultVideoAspect, func(bearer web.XAI) (generation.Result, error) {
		return s.mutate(ctx, job, bearer, prompt, video, "extensions", "extend", extension)
	})
}

// Runs a flow with xAI's bearer, an error it left becoming the failure of the call
func (s *Services) run(ctx context.Context, cfg *config.Config, label, prompt, model, aspect string, flow func(bearer web.XAI) (generation.Result, error)) generation.Result {
	bearer, ok := s.bearer(cfg)
	if !ok {
		return generation.Call{Provider: xaiconfig.Name, Prompt: prompt}.Fail(noVideoKey, "auth_required")
	}
	result, err := flow(bearer)
	if err != nil {
		slog.Warn(fmt.Sprintf("xAI video %s unexpected failure: %v", label, err))
		return generation.Call{Provider: xaiconfig.Name, Model: cmp.Or(model, textVideoModel), Prompt: prompt, Aspect: aspect}.
			Fail(fmt.Sprintf("xAI video %s failed: %v", label, err), "api_error")
	}
	return result
}

// Checks the inputs, takes the aspect and resolution xAI has nearest, and submits one generation job
func (s *Services) generate(ctx context.Context, job *generation.Job, bearer web.XAI, request generation.VideoRequest) (generation.Result, error) {
	prompt, image := strings.TrimSpace(request.Prompt), strings.TrimSpace(request.Image)
	fail := func(text, kind string) (generation.Result, error) {
		return generation.Call{Provider: xaiconfig.Name, Prompt: prompt}.Fail(text, kind), nil
	}
	var still map[string]any
	if image != "" {
		u, err := s.media(job, image, "image", "application/octet-stream")
		if err != nil {
			return generation.Result{}, err
		}
		if usable(u) {
			still = map[string]any{"url": u}
		}
	}
	var refs []any
	every := true
	for _, ref := range request.References {
		if ref = strings.TrimSpace(ref); ref == "" {
			continue
		}
		u, err := s.media(job, ref, "image", "application/octet-stream")
		if err != nil {
			return generation.Result{}, err
		}
		every = every && usable(u)
		refs = append(refs, map[string]any{"url": u})
	}
	switch {
	case image != "" && still == nil:
		return fail("image_url must be a public HTTPS URL or data URI "+publicHint, "invalid_image_url")
	case !every:
		return fail("reference_image_urls must be public HTTPS URLs or data URIs "+publicHint, "invalid_reference_image_urls")
	case prompt == "":
		return fail("prompt is required for xAI video generation", "missing_prompt")
	case len(refs) > maxVideoReferences:
		return fail(fmt.Sprintf("reference_image_urls supports at most %d images on xAI", maxVideoReferences), "too_many_references")
	case still != nil && len(refs) > 0:
		return fail("image_url and reference_image_urls cannot be combined on xAI", "conflicting_inputs")
	}
	aspect := strings.TrimSpace(request.Aspect)
	if !slices.Contains(videoAspects, aspect) {
		aspect = generation.DefaultVideoAspect
	}
	resolution := strings.ToLower(strings.TrimSpace(request.Resolution))
	if !slices.Contains(videoResolutions, resolution) {
		resolution = generation.DefaultVideoResolution
	}
	modality := "text"
	switch {
	case len(refs) > 0:
		modality = "reference"
	case still != nil:
		modality = "image"
	}
	model := modelFor(request.Model, modality, false)
	if len(refs) > 0 {
		model = textVideoModel
	}
	seconds := length(request.Duration, len(refs) > 0, 15, defaultLength)
	payload := map[string]any{"model": model, "prompt": prompt, "duration": seconds, "aspect_ratio": aspect, "resolution": resolution}
	if still != nil {
		payload["image"] = still
	}
	if len(refs) > 0 {
		payload["reference_images"] = refs
	}
	return s.submit(ctx, job.Config, bearer, "generations", payload, modality, "generate", aspect, seconds, resolution)
}

// Whether xAI can fetch a still from the address: an http(s) URL or an image data URL
func usable(address string) bool {
	lower := strings.ToLower(address)
	return address != "" && (strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") || strings.HasPrefix(lower, "data:image/"))
}

// Edit or extension of a video at a public URL, or a video file of this machine inlined
func (s *Services) mutate(ctx context.Context, job *generation.Job, bearer web.XAI, prompt, video, endpoint, operation string, seconds int) (generation.Result, error) {
	prompt = strings.TrimSpace(prompt)
	ref := strings.TrimSpace(video)
	var input map[string]any
	switch {
	case fileExists(ref):
		u, err := s.media(job, ref, "video", "video/mp4")
		if err != nil {
			return generation.Result{}, err
		}
		if u != "" {
			input = map[string]any{"url": u}
		}
	case strings.HasPrefix(strings.ToLower(ref), "http://") || strings.HasPrefix(strings.ToLower(ref), "https://"):
		input = map[string]any{"url": ref}
	}
	call := generation.Call{Provider: xaiconfig.Name, Prompt: prompt}
	if prompt == "" {
		return call.Fail("prompt is required for xAI video edit/extend", "missing_prompt"), nil
	}
	if input == nil {
		return call.Fail("video_url must be a public HTTPS MP4 URL (the `video`/`public_url` from a prior Imagine result)", "missing_video"), nil
	}
	payload := map[string]any{"model": modelFor("", "text", false), "prompt": prompt, "video": input}
	if endpoint == "extensions" {
		payload["duration"] = seconds
	}
	return s.submit(ctx, job.Config, bearer, endpoint, payload, operation, operation, generation.DefaultVideoAspect, seconds, "")
}

// Submits the job, waits for it to finish and words what came of it
func (s *Services) submit(ctx context.Context, cfg *config.Config, bearer web.XAI, endpoint string, payload map[string]any, modality, operation, aspect string,
	seconds int, resolution string) (generation.Result, error) {
	prompt, _ := payload["prompt"].(string)
	model, _ := payload["model"].(string)
	call := generation.Call{Provider: xaiconfig.Name, Model: model, Prompt: prompt}
	st := storageOf(cfg, xaiconfig.VideoKey)
	if options := st.options("icebath-xai-video", "mp4"); options != nil {
		payload["storage_options"] = options
	}
	notice := st.once(cfg, "video_gen")
	body, err := json.Marshal(payload)
	if err != nil {
		return generation.Result{}, err
	}
	status, raw, err := send(ctx, http.MethodPost, bearer.BaseURL+"/videos/"+endpoint, bearer.Key, body, 60*time.Second)
	if err == nil && status == 401 && bearer.Source == web.XAIOAuth && web.RenewXAI(ctx, s.providers, s.creds, cfg) == nil {
		if renewed, ok := s.bearer(cfg); ok {
			bearer = renewed
			status, raw, err = send(ctx, http.MethodPost, bearer.BaseURL+"/videos/"+endpoint, bearer.Key, body, 60*time.Second)
		}
	}
	if err != nil {
		return generation.Result{}, err
	}
	if status < 200 || status > 299 {
		return call.Fail(fmt.Sprintf("xAI submit failed (%d): %s", status, strs.Filled(strs.Head(string(raw), 500), generation.Status(status, bearer.BaseURL+"/videos/"+endpoint).Error())),
			"api_error"), nil
	}
	var submitted map[string]any
	if err := json.Unmarshal(raw, &submitted); err != nil {
		return generation.Result{}, err
	}
	id := strings.TrimSpace(fmt.Sprint(submitted["request_id"]))
	if submitted["request_id"] == nil || id == "" {
		return generation.Result{}, fmt.Errorf("xAI video response did not include request_id")
	}
	var reply map[string]any
	state, finished := "queued", false
	for waited := time.Duration(0); waited < pollFor; waited += pollEvery {
		status, raw, err := send(ctx, http.MethodGet, bearer.BaseURL+"/videos/"+id, bearer.Key, nil, 30*time.Second)
		if err != nil {
			return generation.Result{}, err
		}
		if status < 200 || status > 299 {
			return generation.Result{}, generation.Status(status, bearer.BaseURL+"/videos/"+id)
		}
		if err := json.Unmarshal(raw, &reply); err != nil {
			return generation.Result{}, err
		}
		state = strings.ToLower(generation.Text(reply, "status"))
		if ended[state] {
			finished = true
			break
		}
		select {
		case <-ctx.Done():
			return generation.Result{}, ctx.Err()
		case <-time.After(pollEvery):
		}
	}
	if !finished {
		return call.Fail(fmt.Sprintf("Timed out waiting for xAI video request after %ds", int(pollFor.Seconds())), "timeout"), nil
	}
	if made := generation.Text(reply, "model"); made != "" {
		call.Model = made
	}
	if state != "done" {
		message := generation.Text(generation.Object(reply)["error"], "message")
		if message == "" {
			message = generation.Text(reply, "message")
		}
		failed := generation.Call{Provider: xaiconfig.Name, Model: model, Prompt: prompt}
		return failed.Fail(strs.Filled(message, fmt.Sprintf("xAI video request ended with status '%s'", state)), "xai_"+state), nil
	}
	video := generation.Object(reply["video"])
	output := generation.Object(video["file_output"])
	stored, temporary := generation.Text(output, "public_url"), generation.Text(video, "url")
	address := strs.Filled(stored, temporary)
	if address == "" {
		return call.Fail("xAI video request completed without a video URL", "empty_response"), nil
	}
	extra := map[string]any{"request_id": submitted["request_id"], "operation": operation, "storage_enabled": st.enabled}
	if resolution != "" {
		extra["resolution"] = resolution
	}
	if notice != "" {
		extra["storage_notice"] = notice
	}
	if stored != "" {
		extra["public_url"] = stored
		if temporary != "" && temporary != stored {
			extra["temporary_url"] = temporary
		}
	}
	for _, key := range fileOutput {
		if value, ok := output[key]; ok {
			extra[key] = value
		}
	}
	if usage := reply["usage"]; usage != nil {
		extra["usage"] = usage
	}
	call.Aspect = aspect
	made := call.Made(address, modality, extra)
	made.Duration = seconds
	if d, ok := video["duration"].(float64); ok && d != 0 {
		made.Duration = int(d)
	}
	return made, nil
}

// One request to xAI's videos API with a limit on each wait, the status and the body
func send(ctx context.Context, method, address, key string, body []byte, limit time.Duration) (int, []byte, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, method, address, reader)
	if err != nil {
		return 0, nil, err
	}
	for name, value := range headers(key) {
		request.Header.Set(name, value)
	}
	if method == http.MethodPost {
		request.Header.Set("x-idempotency-key", uuid.NewString())
	}
	response, err := (&http.Client{Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, ResponseHeaderTimeout: limit, TLSHandshakeTimeout: limit}}).Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	raw, err := generation.Idle(response.Body, limit, cancel)
	return response.StatusCode, raw, err
}
