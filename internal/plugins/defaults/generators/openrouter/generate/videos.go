package generate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/generation"
	openrouterconfig "github.com/nickheyer/icebath/internal/plugins/defaults/generators/openrouter/config"
	openrouterprovider "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/openrouter/config"
	"github.com/nickheyer/icebath/internal/provider"
	"github.com/nickheyer/icebath/pkg/strs"
)

const (
	videoDefault       = "minimax/hailuo-3-max"
	videoCatalogTTL    = 5 * time.Minute
	videoCatalogLimit  = 10 * time.Second
	videoRefs          = 3
	videoPollEvery     = 10 * time.Second
	videoPollFor       = 900 * time.Second
	videoRequestLimit  = 60 * time.Second
	videoResultMissing = "OpenRouter submit response did not contain a job id"
)

var (
	videoEnded       = strs.Set("completed", "failed", "cancelled", "canceled", "expired")
	resolutionOrder  = []string{"480p", "540p", "720p", "768p", "1080p", "1K", "2K", "4K"}
	resolutionHeight = map[string]int{"480p": 480, "540p": 540, "720p": 720, "768p": 768, "1080p": 1080, "1K": 1024, "2K": 1440, "4K": 2160}
	allRatios        = []string{"16:9", "9:16", "1:1", "4:3", "3:4", "3:2", "2:3", "21:9", "9:21"}
)

// Catalog the picker and default work from before the first live one is read
func offline() []map[string]any {
	var durations []any
	for d := 5; d < 16; d++ {
		durations = append(durations, float64(d))
	}
	return []map[string]any{{"id": videoDefault, "name": "MiniMax: Hailuo 3 Max", "supported_durations": durations,
		"supported_resolutions": []any{"768p", "480p"}, "supported_aspect_ratios": []any{"21:9", "16:9", "4:3", "1:1", "3:4", "9:16"},
		"supported_frame_images": []any{"first_frame", "last_frame"}, "generate_audio": false, "seed": false,
		"pricing_skus": map[string]any{"duration_seconds_480p": "0.05", "duration_seconds_768p": "0.08"}}}
}

// Video models OpenRouter lists, read again after five minutes; the offline catalog when it can't be read
func (s *Services) videoEntries(ctx context.Context, cfg *config.Config) []map[string]any {
	base := openrouterprovider.BaseURL
	if route, ok := s.route(cfg); ok {
		base = route.BaseURL
	}
	s.mu.Lock()
	held, ok := s.videos[base]
	s.mu.Unlock()
	if ok && time.Since(held.at) < videoCatalogTTL {
		return held.entries
	}
	reply, err := generation.Get(ctx, base+"/videos/models", nil, videoCatalogLimit)
	if err != nil {
		slog.Debug(fmt.Sprintf("OpenRouter video catalog unavailable: %v", err))
	}
	var entries []map[string]any
	for _, entry := range generation.List(reply, "data") {
		if object := generation.Object(entry); object != nil && generation.Text(object, "id") != "" {
			entries = append(entries, object)
		}
	}
	if len(entries) == 0 {
		return offline()
	}
	s.mu.Lock()
	s.videos[base] = videoCatalog{at: time.Now(), entries: entries}
	s.mu.Unlock()
	return entries
}

// Catalog entry of the model, empty for one it doesn't list so the request goes as it is
func (s *Services) entry(ctx context.Context, cfg *config.Config, model string) map[string]any {
	for _, e := range s.videoEntries(ctx, cfg) {
		if generation.Text(e, "id") == model {
			return e
		}
	}
	return map[string]any{}
}

// Texts of a list in an entry
func texts(entry map[string]any, key string) []string {
	var out []string
	for _, item := range generation.List(entry, key) {
		out = append(out, fmt.Sprint(item))
	}
	return out
}

// Whole numbers of a list in an entry
func wholes(entry map[string]any, key string) []int {
	var out []int
	for _, item := range generation.List(entry, key) {
		if n, ok := item.(float64); ok {
			out = append(out, int(n))
		}
	}
	return out
}

// What a catalog entry takes
func takes(entry map[string]any) generation.VideoTakes {
	durations := wholes(entry, "supported_durations")
	modalities := []string{"text"}
	if len(generation.List(entry, "supported_frame_images")) > 0 {
		modalities = append(modalities, "image")
	}
	aspects := texts(entry, "supported_aspect_ratios")
	if len(aspects) == 0 {
		aspects = allRatios
	}
	resolutions := texts(entry, "supported_resolutions")
	if len(resolutions) == 0 {
		resolutions = resolutionOrder
	}
	lo, hi := 1, 30
	if len(durations) > 0 {
		lo, hi = slices.Min(durations), slices.Max(durations)
	}
	audio, _ := entry["generate_audio"].(bool)
	seed, _ := entry["seed"].(bool)
	return generation.VideoTakes{Modalities: modalities, Aspects: aspects, Resolutions: resolutions, MinDuration: lo, MaxDuration: hi, Audio: audio, Seed: seed,
		References: videoRefs}
}

// Price per second the catalog gives, empty for one priced by tokens or megapixels
func price(skus any) string {
	var perSecond []float64
	for key, value := range generation.Object(skus) {
		amount, err := strconv.ParseFloat(strings.TrimSpace(fmt.Sprint(value)), 64)
		if err != nil {
			continue
		}
		switch {
		case strings.HasPrefix(key, "duration_seconds"):
			perSecond = append(perSecond, amount)
		case strings.HasPrefix(key, "cents_per_second_output"), strings.HasPrefix(key, "cents_per_video_output_second"):
			perSecond = append(perSecond, amount/100)
		}
	}
	if len(perSecond) == 0 {
		return ""
	}
	lo, hi := slices.Min(perSecond), slices.Max(perSecond)
	if lo == hi {
		return "$" + strconv.FormatFloat(lo, 'f', 2, 64) + "/s"
	}
	return "$" + strconv.FormatFloat(lo, 'f', 2, 64) + "–" + strconv.FormatFloat(hi, 'f', 2, 64) + "/s"
}

// Picker rows of the models that make videos, those that list lengths, with their lengths, resolutions and whether they take audio and stills
func (s *Services) videoModels(ctx context.Context, cfg *config.Config) []generation.Model {
	var rows []generation.Model
	for _, entry := range s.videoEntries(ctx, cfg) {
		if len(generation.List(entry, "supported_durations")) == 0 {
			continue
		}
		t := takes(entry)
		extras := []string{fmt.Sprintf("%d-%ds", t.MinDuration, t.MaxDuration), strings.Join(t.Resolutions, "/")}
		if t.Audio {
			extras = append(extras, "audio")
		}
		if slices.Contains(t.Modalities, "image") {
			extras = append(extras, "i2v")
		}
		id := generation.Text(entry, "id")
		rows = append(rows, generation.Model{ID: id, Title: strs.Filled(generation.Text(entry, "name"), id), Strengths: strings.Join(extras, ", "),
			Price: price(entry["pricing_skus"]), Modalities: t.Modalities, MinDuration: t.MinDuration, MaxDuration: t.MaxDuration})
	}
	return rows
}

// What the model video_gen.openrouter.model names takes, the whole API's when the catalog doesn't list it
func (s *Services) videoTakes(cfg *config.Config) generation.VideoTakes {
	if e := s.entry(context.Background(), cfg, strs.Filled(strings.TrimSpace(cfg.GetString(openrouterconfig.VideoModel)), videoDefault)); len(e) > 0 {
		return takes(e)
	}
	return generation.VideoTakes{Modalities: []string{"text", "image"}, Aspects: allRatios, Resolutions: resolutionOrder, MinDuration: 1, MaxDuration: 30, Audio: true,
		Seed: true, References: videoRefs}
}

// Width over height of a ratio, false when it reads as none
func proportion(value string) (float64, bool) {
	w, h, ok := strings.Cut(value, ":")
	if !ok {
		return 0, false
	}
	a, errA := strconv.ParseFloat(strings.TrimSpace(w), 64)
	b, errB := strconv.ParseFloat(strings.TrimSpace(h), 64)
	if errA != nil || errB != nil || b == 0 {
		return 0, false
	}
	return a / b, true
}

// Supported value nearest the one asked for: by pixel height for resolutions, by proportion for ratios; the value itself when nothing is
// listed or it is listed
func nearest(value string, supported []string, heights bool) string {
	if len(supported) == 0 || slices.Contains(supported, value) {
		return value
	}
	score := func(candidate string) (float64, bool) {
		if heights {
			target, ok := resolutionHeight[value]
			h, known := resolutionHeight[candidate]
			return math.Abs(float64(h - target)), ok && known
		}
		target, ok := proportion(value)
		r, known := proportion(candidate)
		return math.Abs(r - target), ok && known
	}
	best, bestScore := supported[0], math.Inf(1)
	for _, candidate := range supported {
		if d, ok := score(candidate); ok && d < bestScore {
			best, bestScore = candidate, d
		}
	}
	return best
}

// Supported length nearest the one asked for, the shorter of two as near
func nearestLength(asked int, supported []int) int {
	if len(supported) == 0 || slices.Contains(supported, asked) {
		return asked
	}
	best := supported[0]
	for _, candidate := range supported[1:] {
		if d, b := abs(candidate-asked), abs(best-asked); d < b || d == b && candidate < best {
			best = candidate
		}
	}
	return best
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// Request with every value moved to the nearest the model's live limits take, toggles it doesn't take left out
func videoPayload(entry map[string]any, model string, request generation.VideoRequest, image string, refs []string) map[string]any {
	payload := map[string]any{"model": model, "prompt": request.Prompt}
	if request.Aspect != "" {
		payload["aspect_ratio"] = nearest(request.Aspect, texts(entry, "supported_aspect_ratios"), false)
	}
	if request.Resolution != "" {
		payload["resolution"] = nearest(request.Resolution, texts(entry, "supported_resolutions"), true)
	}
	if request.Duration != nil && *request.Duration != 0 {
		payload["duration"] = nearestLength(*request.Duration, wholes(entry, "supported_durations"))
	}
	if image != "" {
		payload["frame_images"] = []any{map[string]any{"type": "image_url", "image_url": map[string]any{"url": image}, "frame_type": "first_frame"}}
	}
	if len(refs) > 0 {
		var parts []any
		for _, u := range refs[:min(len(refs), videoRefs)] {
			parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": u}})
		}
		payload["input_references"] = parts
	}
	if audio, _ := entry["generate_audio"].(bool); audio && request.Audio != nil {
		payload["generate_audio"] = *request.Audio
	}
	if seed, _ := entry["seed"].(bool); seed && request.Seed != nil {
		payload["seed"] = *request.Seed
	}
	return payload
}

// Makes one video: submits, waits for the job and downloads the output from the configured address with the key, never from an address
// the service hands back
func (s *Services) makeVideo(ctx context.Context, job *generation.Job, request generation.VideoRequest) generation.Result {
	cfg := job.Config
	prompt := strings.TrimSpace(request.Prompt)
	model := strs.Filled(request.Model, videoDefault)
	call := generation.Call{Provider: openrouterconfig.Name, Model: model, Prompt: prompt, Aspect: request.Aspect}
	if prompt == "" {
		return call.Fail("prompt is required", "invalid_request")
	}
	route, ok := s.route(cfg)
	if !ok {
		return call.Fail("No OpenRouter credential: set OPENROUTER_API_KEY or connect OpenRouter in /providers", "missing_credentials")
	}
	image := strings.TrimSpace(request.Image)
	var refs []string
	for _, ref := range request.References {
		if ref = strings.TrimSpace(ref); ref != "" {
			refs = append(refs, ref)
		}
	}
	for _, input := range append([]string{image}, refs...) {
		lower := strings.ToLower(input)
		if input != "" && !strings.HasPrefix(lower, "https://") && !strings.HasPrefix(lower, "data:image/") {
			return call.Fail("image inputs must be public HTTPS URLs or data:image/ URLs (OpenRouter fetches them itself)", "invalid_request")
		}
	}
	request.Prompt = prompt
	payload := videoPayload(s.entry(ctx, cfg, model), model, request, image, refs)
	id, reply, path, failure, kind, err := s.runVideo(ctx, job, route, payload)
	if err != nil {
		slog.Debug("OpenRouter video generation failed", "error", err)
		return call.Fail(fmt.Sprintf("OpenRouter video generation failed: %v", err), "api_error")
	}
	if failure != "" {
		return call.Fail(failure, kind)
	}
	extra := map[string]any{"job_id": id}
	if cost := generation.Object(reply["usage"])["cost"]; cost != nil {
		extra["cost"] = cost
	}
	modality := "text"
	if image != "" {
		modality = "image"
	}
	call.Aspect, _ = payload["aspect_ratio"].(string)
	made := call.Made(path, modality, extra)
	made.Duration, _ = payload["duration"].(int)
	return made
}

// Submits the job, waits for it and saves its output: the job id, its last state and the saved file, or the failure the model reads
func (s *Services) runVideo(ctx context.Context, job *generation.Job, route provider.Route, payload map[string]any) (string, map[string]any, string, string, string, error) {
	headers := requestHeaders(route.APIKey)
	body, err := json.Marshal(payload)
	if err != nil {
		return "", nil, "", "", "", err
	}
	status, raw, err := request(ctx, http.MethodPost, route.BaseURL+"/videos", headers, body, videoRequestLimit)
	if err != nil {
		return "", nil, "", "", "", err
	}
	if status >= 400 {
		var reply map[string]any
		detail := ""
		if json.Unmarshal(raw, &reply) == nil {
			detail = generation.Text(generation.Object(reply["error"]), "message")
		}
		return "", nil, "", fmt.Sprintf("OpenRouter rejected the request (HTTP %d): %s", status, strs.Filled(detail, strs.Head(string(raw), 300))), "api_error", nil
	}
	var submitted map[string]any
	if err := json.Unmarshal(raw, &submitted); err != nil {
		return "", nil, "", "", "", err
	}
	id := strings.TrimSpace(generation.Text(submitted, "id"))
	if id == "" {
		return "", nil, "", videoResultMissing, "api_error", nil
	}
	deadline := time.Now().Add(videoPollFor)
	last := "unknown"
	var reply map[string]any
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return "", nil, "", "", "", fmt.Errorf("video job %s did not finish within %ds (last status=%s)", id, int(videoPollFor.Seconds()), last)
		}
		status, raw, err := request(ctx, http.MethodGet, route.BaseURL+"/videos/"+id, headers, nil, max(time.Millisecond, min(videoRequestLimit, remaining)))
		if err != nil {
			return "", nil, "", "", "", err
		}
		if status < 200 || status > 299 {
			return "", nil, "", "", "", generation.Status(status, route.BaseURL+"/videos/"+id)
		}
		if err := json.Unmarshal(raw, &reply); err != nil {
			return "", nil, "", "", "", err
		}
		last = strs.Filled(strings.ToLower(generation.Text(reply, "status")), "unknown")
		if videoEnded[last] {
			break
		}
		select {
		case <-ctx.Done():
			return "", nil, "", "", "", ctx.Err()
		case <-time.After(min(videoPollEvery, max(0, time.Until(deadline)))):
		}
	}
	if last != "completed" {
		text := "video job ended with status='" + last + "'"
		if failed := reply["error"]; failed != nil && failed != "" {
			text = fmt.Sprint(failed)
			if object, ok := failed.(map[string]any); ok {
				compact, _ := json.Marshal(object)
				text = string(compact)
			}
		}
		return id, reply, "", text, "job_failed", nil
	}
	path, err := job.Download(ctx, generation.Video, route.BaseURL+"/videos/"+id+"/content", generation.Fetch{Prefix: openrouterconfig.Name, Headers: headers, Strict: true,
		Origin: route.BaseURL})
	if err != nil {
		return "", nil, "", "", "", err
	}
	return id, reply, path, "", "", nil
}

// One request to the videos API with a limit on each wait, the status and the body
func request(ctx context.Context, method, address string, headers map[string]string, body []byte, limit time.Duration) (int, []byte, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, address, reader)
	if err != nil {
		return 0, nil, err
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	response, err := (&http.Client{Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, ResponseHeaderTimeout: limit, TLSHandshakeTimeout: limit,
		ForceAttemptHTTP2: true}}).Do(req)
	if err != nil {
		if ctx.Err() == nil {
			return 0, nil, err
		}
		return 0, nil, fmt.Errorf("Read timed out after %s: %w", limit, err)
	}
	defer response.Body.Close()
	raw, err := generation.Idle(response.Body, limit, cancel)
	return response.StatusCode, raw, err
}
