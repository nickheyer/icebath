package generate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/generation"
	kreaconfig "github.com/nickheyer/icebath/internal/plugins/defaults/generators/krea/config"
	"github.com/nickheyer/icebath/internal/provider"
	"github.com/nickheyer/icebath/pkg/strs"
)

const (
	defaultModel  = "krea-2-medium"
	resolution    = "1K"
	styleStrength = 0.6
	maxStyleRefs  = 10
	maxEditRefs   = 4
	pollFirst     = 2.0
	pollMost      = 5.0
	pollGrowth    = 1.3
	pollFor       = 180 * time.Second
	requestLimit  = 30 * time.Second
	enhancePath   = "/generate/enhance/krea/enhance"
	enhanceFactor = 2
	// Most bytes of local references inlined in all
	maxLocal = 3 << 20
)

var agent = provider.UserAgent("Icebath-Agent") + " (krea-image-gen)"

// One model of Krea, the endpoint it draws at and whether it edits the pictures it is handed
type model struct {
	id, title, speed, strengths, price, path string
	edit                                     bool
}

var models = []model{
	{id: "krea-3", title: "Krea 3", speed: "About 5 seconds at 1K, 40 seconds at 2K with high thinking",
		strengths: "Makes images and edits existing ones from instructions, keeping products, characters, logos and text consistent",
		price:     "$0.03 at 1K or $0.06 at 2K, $0.02 or $0.07 more for each reference image, $0.02 more with high thinking", path: "/generate/image/krea/krea-3", edit: true},
	{id: "krea-2-medium", title: "Krea 2 Medium", speed: "15 to 25 seconds", strengths: "Illustration, anime, painting and expressive styles, faster and cheaper",
		price: "$0.030 from text, $0.035 with style references, $0.040 with moodboards", path: "/generate/image/krea/krea-2/medium"},
	{id: "krea-2-large", title: "Krea 2 Large", speed: "25 to 60 seconds", strengths: "Photorealism, raw textured looks such as motion blur and grain, expressive styles",
		price: "$0.060 from text, $0.065 with style references, $0.070 with moodboards", path: "/generate/image/krea/krea-2/large"},
	{id: "krea-2-medium-turbo", title: "Krea 2 Medium Turbo", speed: "8 to 15 seconds", strengths: "The fastest Krea 2, medium quality for less wait and cost",
		price: "$0.015 from text, $0.0175 with style references", path: "/generate/image/krea/krea-2/medium-turbo"},
}

var (
	aspects     = map[string]string{"landscape": "16:9", "square": "1:1", "portrait": "9:16"}
	creativity  = []string{"raw", "low", "medium", "high"}
	sliders     = []string{"intensity", "complexity", "movement"}
	retried     = map[int]bool{408: true, 409: true, 425: true, 429: true, 500: true, 502: true, 503: true, 504: true}
	ended       = strs.Set("completed", "failed", "cancelled")
	resolutions = strs.Set("1K", "2K")
)

type services struct{}

func Register(registry *generation.Registry) {
	registry.Register(services{})
}

func key(cfg *config.Config) string {
	return strings.TrimSpace(cfg.GetString(kreaconfig.APIKey))
}

func (services) Services() []generation.Service {
	return []generation.Service{{
		Name:  kreaconfig.Name,
		Title: "Krea",
		Ready: func(cfg *config.Config) bool { return key(cfg) != "" },
		Setup: generation.Setup{Badge: "paid", Key: kreaconfig.APIKey, Prompt: "Krea API key", Link: kreaconfig.KeyLink},
		Images: &generation.Images{
			About:   "Stylized images and edits, needs a Krea API key",
			Section: kreaconfig.Key,
			Model:   kreaconfig.Model,
			Models: func(context.Context, *config.Config) []generation.Model {
				out := make([]generation.Model, len(models))
				for i, m := range models {
					out[i] = generation.Model{ID: m.id, Title: m.title, Speed: m.speed, Strengths: m.strengths, Price: m.price}
				}
				return out
			},
			Default:  func(context.Context, *config.Config) string { return defaultModel },
			Takes:    takes,
			Generate: draw,
		},
	}}
}

// Model image_gen.krea.model names when Krea has it, else the default
func chosen(name string) model {
	if i := slices.IndexFunc(models, func(m model) bool { return m.id == strings.TrimSpace(name) }); i >= 0 {
		return models[i]
	}
	return models[slices.IndexFunc(models, func(m model) bool { return m.id == defaultModel })]
}

// Krea 3 edits with thinking and resolution, Krea 2 restyles with its creative controls
func takes(cfg *config.Config) generation.ImageTakes {
	if chosen(cfg.GetString(kreaconfig.Model)).edit {
		return generation.ImageTakes{Edits: true, References: maxEditRefs, Upscale: true, Controls: []string{"thinking", "resolution"}}
	}
	return generation.ImageTakes{Edits: true, References: maxStyleRefs, Upscale: true, Controls: []string{"creativity", "intensity", "complexity", "movement"}, Style: true}
}

// Sources once each in order, at most n
func unique(sources []string, n int) []string {
	out := strs.Unique(sources)
	return out[:min(len(out), n)]
}

// Sources as Krea takes them: URLs as they are, files of this machine inlined, all of those under the byte budget
func inline(job *generation.Job, sources []string) ([]string, string, string) {
	out := make([]string, 0, len(sources))
	total := 0
	for _, source := range sources {
		if generation.Remote(source) {
			out = append(out, source)
			continue
		}
		if refusal := generation.Unreadable(job.Config, source); refusal != "" {
			return nil, refusal, "invalid_image_url"
		}
		data, err := job.Local(source)
		var refusal *generation.Refusal
		switch {
		case errors.As(err, &refusal):
			return nil, "Reference image not found: " + source, "invalid_image_url"
		case err != nil:
			return nil, fmt.Sprintf("Reference image unreadable: %s (%v)", source, err), "invalid_image_url"
		}
		if total += len(data); total > maxLocal {
			return nil, fmt.Sprintf("Local reference images total over %d MB; resize them or pass public URLs", maxLocal>>20), "source_too_large"
		}
		out = append(out, generation.DataURL(strs.Filled(generation.TypeOf(source), "image/png"), data))
	}
	return out, "", ""
}

// Request of the model for the call, or why it can't be made
func payload(job *generation.Job, m model, request generation.ImageRequest, prompt, ratio string) (map[string]any, string, string) {
	controls := request.Controls
	if m.edit {
		images, why, kind := inline(job, unique(request.Sources(-1), maxEditRefs))
		if why != "" {
			return nil, why, kind
		}
		out := map[string]any{"prompt": prompt, "resolution": resolution, "thinking_mode": "fast"}
		if r := strings.ToUpper(fmt.Sprint(controls["resolution"])); resolutions[r] {
			out["resolution"] = r
		}
		if controls["thinking"] == "high" {
			out["thinking_mode"] = "high"
		}
		if len(images) > 0 {
			out["image_urls"] = images
		} else {
			out["aspect_ratio"] = ratio
		}
		return out, "", ""
	}
	out := map[string]any{"prompt": prompt, "aspect_ratio": ratio, "resolution": resolution, "creativity": job.Config.GetString(kreaconfig.Creativity)}
	if c, ok := controls["creativity"].(string); ok && slices.Contains(creativity, strings.ToLower(strings.TrimSpace(c))) {
		out["creativity"] = strings.ToLower(strings.TrimSpace(c))
	}
	for _, slider := range sliders {
		if n := generation.Whole(controls[slider]); n != nil && *n >= -100 && *n <= 100 {
			out[slider] = *n
		}
	}
	refs, why, kind := inline(job, unique(request.Sources(-1), maxStyleRefs))
	if why != "" {
		return nil, why, kind
	}
	if len(refs) > 0 {
		styled := make([]any, len(refs))
		for i, ref := range refs {
			styled[i] = map[string]any{"url": ref, "strength": styleStrength}
		}
		out["image_style_references"] = styled
	}
	return out, "", ""
}

func headers(token string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + token, "User-Agent": agent}
}

// Message of a failed submit: its error's message, else its message or detail, else the head of the body
func submitMessage(_ int, body []byte) string {
	var reply map[string]any
	if json.Unmarshal(body, &reply) != nil {
		return strs.Head(string(body), 300)
	}
	if message := generation.Text(reply["error"], "message"); message != "" {
		return message
	}
	return strs.Filled(generation.Text(reply, "message"), generation.Text(reply, "detail"), strs.Head(string(body), 300))
}

// Submits, waits for, optionally enhances and keeps one Krea picture
func draw(ctx context.Context, job *generation.Job, request generation.ImageRequest) generation.Result {
	cfg := job.Config
	prompt, aspect := strings.TrimSpace(request.Prompt), generation.Aspect(request.Aspect)
	ratio, ok := aspects[aspect]
	if !ok {
		ratio = "1:1"
	}
	if prompt == "" {
		return generation.Call{Provider: kreaconfig.Name, Aspect: aspect}.Fail("Prompt is required and must be a non-empty string", "invalid_argument")
	}
	token := key(cfg)
	if token == "" {
		return generation.Call{Provider: kreaconfig.Name, Aspect: aspect}.Fail("KREA_API_KEY not set. The user gets a key at "+kreaconfig.KeyLink+
			" and sets it with /images.", "auth_required")
	}
	m := chosen(request.Model)
	call := generation.Call{Provider: kreaconfig.Name, Model: m.id, Prompt: prompt, Aspect: aspect}
	body, why, kind := payload(job, m, request, prompt, ratio)
	if why != "" {
		return call.Fail(why, kind)
	}
	reply, failure := generation.Post(ctx, kreaconfig.BaseURL+m.path, headers(token), body, requestLimit, requestLimit, "Krea", submitMessage)
	if failure != nil {
		switch failure.Kind {
		case "http":
			slog.Error(fmt.Sprintf("Krea submit failed (%d): %s", failure.Status, failure.Message))
			return call.Fail(failure.Error, "api_error")
		case "timeout":
			return call.Fail("Krea submit timed out (30s)", "timeout")
		case "invalid_json":
			return call.Fail("Krea returned invalid JSON on submit: "+failure.Message, "invalid_response")
		}
		return call.Fail(failure.Error, failure.Type)
	}
	id := generation.Text(reply, "job_id")
	if id == "" {
		return call.Fail("Krea submit response missing job_id", "invalid_response")
	}
	finished, failed := poll(ctx, token, id, func(text, kind string) generation.Result { return call.Fail(text, kind) })
	if failed != nil {
		return *failed
	}
	address, failed := outcome(finished, id, call)
	if failed != nil {
		return *failed
	}
	upscaled := false
	if upscaling(cfg, request.Upscale) {
		if enhanced := enhance(ctx, token, address, prompt); enhanced != "" {
			address, upscaled = enhanced, true
		} else {
			slog.Warn("Krea Enhance pass failed — returning native-resolution image")
		}
	}
	path, err := job.Download(ctx, generation.Image, address, generation.Fetch{Prefix: "krea_" + m.id, Origin: kreaconfig.BaseURL})
	if err != nil {
		slog.Warn(fmt.Sprintf("Krea image URL %s could not be cached (%s); falling back to bare URL.", address, err))
		path = address
	}
	extra := map[string]any{"resolution": body["resolution"], "job_id": id, "upscaled": upscaled}
	for from, to := range map[string]string{"aspect_ratio": "krea_aspect_ratio", "creativity": "creativity", "thinking_mode": "thinking_mode"} {
		if value, ok := body[from]; ok {
			extra[to] = value
		}
	}
	if upscaled {
		extra["upscale_factor"] = enhanceFactor
	}
	if completed := generation.Text(finished, "completed_at"); completed != "" {
		extra["completed_at"] = completed
	}
	modality := "text"
	if body["image_style_references"] != nil || body["image_urls"] != nil {
		modality = "image"
	}
	return call.Made(path, modality, extra)
}

// Whether the picture is enhanced: as the call asks, else as image_gen.krea.upscale says, else not
func upscaling(cfg *config.Config, asked *bool) bool {
	if asked != nil {
		return *asked
	}
	return cfg.GetString(kreaconfig.Upscale) == "true"
}

// Address of a finished job's picture, or the failure of a job that failed, was cancelled or holds none
func outcome(finished map[string]any, id string, call generation.Call) (string, *generation.Result) {
	fail := func(text, kind string) (string, *generation.Result) {
		result := call.Fail(text, kind)
		return "", &result
	}
	result := generation.Object(finished["result"])
	switch generation.Text(finished, "status") {
	case "failed":
		detail := "unknown error"
		if reason := result["error"]; reason != nil && reason != "" {
			detail = fmt.Sprint(reason)
		}
		return fail(fmt.Sprintf("Krea job %s failed: %s", id, detail), "api_error")
	case "cancelled":
		return fail(fmt.Sprintf("Krea job %s was cancelled", id), "cancelled")
	}
	if result == nil {
		return fail("Krea job completed but result was missing", "empty_response")
	}
	if u := address(result); u != "" {
		return u, nil
	}
	return fail("Krea result contained no image URL", "empty_response")
}

// First address of a result: of its urls, else its url
func address(result map[string]any) string {
	for _, candidate := range append(generation.List(result, "urls"), result["url"]) {
		if text, ok := candidate.(string); ok && strings.TrimSpace(text) != "" {
			return strings.TrimSpace(text)
		}
	}
	return ""
}

// Waits for the job to end, looking at it more and more slowly; the failure of a call whose job can't be followed. A nil fail gives up
// quietly, for the enhance pass
func poll(ctx context.Context, token, id string, fail func(text, kind string) generation.Result) (map[string]any, *generation.Result) {
	deadline := time.Now().Add(pollFor)
	interval := pollFirst
	last := ""
	quit := func(text, kind, warning string) (map[string]any, *generation.Result) {
		if fail == nil {
			slog.Warn(warning)
			return nil, nil
		}
		result := fail(text, kind)
		return nil, &result
	}
	for {
		select {
		case <-ctx.Done():
			return quit("Krea poll stopped: "+ctx.Err().Error(), "interrupted", "Krea enhance poll stopped")
		case <-time.After(time.Duration(interval * float64(time.Second))):
		}
		interval = min(interval*pollGrowth, pollMost)
		expired := !time.Now().Before(deadline)
		status, raw, err := get(ctx, kreaconfig.BaseURL+"/jobs/"+id, token)
		switch {
		case err != nil:
			if fail != nil {
				slog.Warn(fmt.Sprintf("Krea poll transient error for job %s: %s", id, err))
			}
			if expired {
				return quit(fmt.Sprintf("Krea poll timed out for job %s: %s", id, err), "timeout", fmt.Sprintf("Krea enhance poll gave up for job %s: %s", id, err))
			}
			continue
		case status >= 400:
			if fail != nil {
				slog.Error(fmt.Sprintf("Krea poll failed (%d) for job %s", status, id))
			}
			if !retried[status] || expired {
				return quit(fmt.Sprintf("Krea poll failed (%d) for job %s", status, id), "api_error", fmt.Sprintf("Krea enhance poll failed (%d) for job %s", status, id))
			}
			continue
		}
		var job map[string]any
		if err := json.Unmarshal(raw, &job); err != nil || job == nil {
			if expired {
				return quit(fmt.Sprintf("Krea poll returned invalid JSON: %v", err), "invalid_response", fmt.Sprintf("Krea enhance poll gave up for job %s: %v", id, err))
			}
			continue
		}
		if state := generation.Text(job, "status"); state != "" {
			last = state
		}
		if ended[generation.Text(job, "status")] || generation.Text(job, "completed_at") != "" {
			return job, nil
		}
		if expired {
			return quit(fmt.Sprintf("Krea job %s did not complete within %ds (last status: %s)", id, int(pollFor.Seconds()), strs.Filled(last, "unknown")), "timeout",
				fmt.Sprintf("Krea enhance job %s did not finish in %ds", id, int(pollFor.Seconds())))
		}
	}
}

// GET of a job with the token, its status and body
func get(ctx context.Context, address, token string) (int, []byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return 0, nil, err
	}
	for name, value := range headers(token) {
		request.Header.Set(name, value)
	}
	response, err := (&http.Client{Timeout: requestLimit}).Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 32<<20))
	return response.StatusCode, raw, err
}

// Address of the picture enlarged by Krea Enhance, empty when the pass fails
func enhance(ctx context.Context, token, image, prompt string) string {
	reply, failure := generation.Post(ctx, kreaconfig.BaseURL+enhancePath, headers(token), map[string]any{"image_url": image, "image_scaling_factor": enhanceFactor,
		"prompt": prompt}, requestLimit, requestLimit, "Krea", nil)
	if failure != nil {
		slog.Warn("Krea Enhance submit failed: " + failure.Error)
		return ""
	}
	id := generation.Text(reply, "job_id")
	if id == "" {
		slog.Warn("Krea Enhance submit response missing job_id")
		return ""
	}
	finished, _ := poll(ctx, token, id, nil)
	if status := generation.Text(finished, "status"); finished == nil || status == "failed" || status == "cancelled" {
		slog.Warn(fmt.Sprintf("Krea Enhance job %s did not complete successfully", id))
		return ""
	}
	return address(generation.Object(finished["result"]))
}
