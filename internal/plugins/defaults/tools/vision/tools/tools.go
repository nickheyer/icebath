package tools

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"net/url"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/internal/auxiliary"
	auxiliaryconfig "github.com/nickheyer/icebath/internal/auxiliary/config"
	"github.com/nickheyer/icebath/internal/config"
	visionconfig "github.com/nickheyer/icebath/internal/plugins/defaults/tools/vision/config"
	"github.com/nickheyer/icebath/internal/tools"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
	"github.com/nickheyer/icebath/pkg/strs"
)

const (
	// Longest data URL any vision API takes
	maxPayload = 20 << 20
	// Size an image is shrunk to after an API turned it down as too large
	retryPayload = 5 << 20
	// Long side of an image the main model sees itself, which Anthropic shrinks to anyway
	longest = 1568
	// Longest data URL of a video, and the size past which one may be slow or turned down
	maxVideo = 50 << 20
	// Images remembered per session and image for the looks limit
	remembered = 4096
)

// Words of a provider error that say the image or payload was too large
var sizeHints = []string{"too large", "payload", "413", "content_too_large", "request_too_large", "exceeds", "size limit"}

// Explanation the model gets of a failed analysis, the first whose words the error holds
type rule struct {
	hints    []string
	template string
}

var billing = rule{[]string{"402", "insufficient", "payment required", "credits", "billing"},
	"Insufficient credits or payment required. Please top up your API provider account and try again. Error: {e}"}

var imageRules = []rule{billing,
	{[]string{"does not support", "not support image", "content_policy", "multimodal", "unrecognized request argument", "image input"},
		"{model} does not support vision or our request was not accepted by the server. Error: {e}"},
	{[]string{"invalid_request", "image_url"},
		"The vision API rejected the image. This can happen when the image is in an unsupported format, corrupted, or still too large after auto-resize. Try a smaller JPEG/PNG and retry. Error: {e}"},
}

var videoRules = []rule{billing,
	{[]string{"does not support", "not support video", "content_policy", "multimodal", "unrecognized request argument", "video input", "video_url"},
		"The model does not support video analysis or the request was rejected. Ensure you're using a video-capable model (e.g. google/gemini-2.5-flash). Error: {e}"},
	{sizeHints, "The video is too large for the API. Try compressing or trimming the video (max ~50 MB). Error: {e}"},
}

type registrant struct {
	// Slots of the image work that keeps a processor busy, as many as the setting says
	mu      sync.Mutex
	workers chan struct{}
	// Times each image was loaded into a session, oldest first
	looks []look
	log   *journal
}

type look struct {
	session, image string
	count          int
}

func Register(registry *tools.Registry) {
	registry.Register(&registrant{log: &journal{}})
}

func (r *registrant) Tools() []tools.Def {
	return []tools.Def{
		{
			Name:    "vision_analyze",
			Toolset: "vision",
			Title:   "Look",
			About:   "Look at an image from a file or a web address",
			Group:   "Vision",
			Icon:    tools.IconRead,
			Subject: "image_url",
			Schema: tools.Schema("Load an image into the conversation so you can see it. Call it any time the user references an image — then answer from what you see.",
				[]string{"image_url", "question"}, map[string]any{
					"image_url": tools.Property("string", "Image URL (http/https), local file path, or data: URL to load."),
					"question":  tools.Property("string", "Your question or request about the image."),
					"region": map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "minItems": 4, "maxItems": 4,
						"description": "Optional [x1, y1, x2, y2] crop in ORIGINAL-image pixel coordinates, applied before any downscaling — the crop keeps full " +
							"resolution. Load the full image first, then re-call with a region to zoom into small text or fine detail."},
				}),
			Handler:  r.vision,
			Effect:   tools.ReadOnly,
			Parallel: true,
			// With no session to judge by, the tool is there as far as anyone can tell
			Check: func(env *tools.Env) bool {
				return env.Aux == nil || env.Aux.NativeImages(context.Background()) || env.Aux.Resolves(context.Background(), visionconfig.Task)
			},
			Lacks: func(*tools.Env) string {
				return "It needs a model that sees images, for the agent or for image analysis."
			},
		},
		{
			Name:    "video_analyze",
			Toolset: "video",
			Title:   "Watch",
			About:   "Watch a video from a file or a web address",
			Group:   "Vision",
			Icon:    tools.IconRead,
			Subject: "video_url",
			Schema: tools.Schema("Analyze a video from a URL or local file path using a multimodal AI model. Sends the video to a video-capable model (e.g. Gemini) "+
				"for understanding. Use this for video files — for images, use vision_analyze instead. Supports mp4, webm, mov, avi, mkv, mpeg formats. "+
				"Note: large videos (>20 MB) may be slow; max ~50 MB.",
				[]string{"video_url", "question"}, map[string]any{
					"video_url": tools.Property("string", "Video URL (http/https) or local file path to analyze."),
					"question":  tools.Property("string", "Your specific question about the video. The AI will describe what happens in the video and answer your question."),
				}),
			Handler: r.video,
			Check: func(env *tools.Env) bool {
				return env.Config.GetBool(visionconfig.Video) && (env.Aux == nil || env.Aux.Resolves(context.Background(), visionconfig.VideoTask))
			},
			Lacks: func(env *tools.Env) string {
				if !env.Config.GetBool(visionconfig.Video) {
					return "Watching videos is off."
				}
				return "It needs a model that watches videos."
			},
		},
	}
}

// Runs work that keeps a processor busy in one of the slots the setting allows, one per processor core when it names none
func work[T any](r *registrant, call *tools.Call, run func() T) T {
	wanted := call.Env.Config.GetInt(auxiliary.Task{Name: visionconfig.Task}.Key(visionconfig.MaxConcurrency))
	if wanted < 1 {
		wanted = max(runtime.NumCPU(), 1)
	}
	r.mu.Lock()
	if cap(r.workers) != wanted {
		r.workers = make(chan struct{}, wanted)
	}
	slots := r.workers
	r.mu.Unlock()
	slots <- struct{}{}
	defer func() { <-slots }()
	return run()
}

func (r *registrant) vision(ctx context.Context, call *tools.Call) string {
	var args struct {
		ImageURL any `json:"image_url"`
		Question any `json:"question"`
		Region   any `json:"region"`
	}
	if err := json.Unmarshal([]byte(cmp.Or(call.RawArgs, "{}")), &args); err != nil {
		return tools.ErrorResult(err)
	}
	source, _ := args.ImageURL.(string)
	if call.Env.Aux != nil && call.Env.Aux.NativeImages(ctx) {
		return r.native(ctx, call, args.ImageURL, args.Question, args.Region)
	}
	prompt := "Fully describe and explain everything about this image, then answer the following question:\n\n" + text(args.Question)
	return r.analyze(ctx, call, "image", source, prompt, func() (string, string, error) {
		return r.describe(ctx, call, args.ImageURL, args.Region, prompt)
	})
}

// Image handed to the main model itself, shrunk to the budget of images kept in the conversation
func (r *registrant) native(ctx context.Context, call *tools.Call, raw, question, region any) string {
	source, ok := raw.(string)
	if !ok || strings.TrimSpace(source) == "" {
		return failure("image_url is required")
	}
	key := imageKey(call, source)
	limit := looks(call.Env)
	if limit > 0 {
		if refused := r.reserve(call.SessionID, key, limit); refused != "" {
			return refused
		}
	}
	loaded := false
	defer func() {
		if !loaded && limit > 0 {
			r.release(call.SessionID, key)
		}
	}()
	if ctx.Err() != nil {
		return failure("Interrupted")
	}
	data, mime, offset, cropped, err := r.prepare(ctx, call, source, region, true)
	if err != nil {
		var refused *prepError
		if errors.As(err, &refused) {
			return failure(refused.text)
		}
		return failure("Native vision failed: " + err.Error())
	}
	budget := min(max(call.Env.Config.GetInt(visionconfig.EmbedTargetBytes), 64<<10), 4<<20)
	result := work(r, call, func() resizedResult {
		w, h, known := dimensions(data, mime)
		if size(len(data), mime) <= budget && !(known && max(w, h) > longest) {
			return resizedResult{data: data, mime: mime}
		}
		out, kind, s, err := resize(data, mime, budget, longest, true)
		return resizedResult{out, kind, s, err}
	})
	if result.err != nil {
		return failure("Native vision failed: " + result.err.Error())
	}
	if size(len(result.data), result.mime) > maxPayload {
		return failure(tooLarge(size(len(result.data), result.mime)))
	}
	if limit <= 0 {
		r.reserve(call.SessionID, key, 0)
	}
	loaded = true
	call.Media = append(call.Media, &sessionsv1.Media{MimeType: result.mime, Data: result.data})
	answer := "Image loaded into your context — you can see it natively now. Use your built-in vision to answer the user."
	if q := strings.TrimSpace(text(question)); q != "" {
		answer += "\n\nQuestion: " + q
	}
	if scaled := note(result.scale, offset, cropped); scaled != "" {
		answer += "\n\nNote: " + scaled
	}
	return answer
}

// Image of the source made ready for a model: read, put in a format vision APIs read, checked whole when asked, and cut to the region
func (r *registrant) prepare(ctx context.Context, call *tools.Call, source string, region any, check bool) ([]byte, string, image.Point, bool, error) {
	data, mime, err := resolve(ctx, call, source)
	if err != nil {
		return nil, "", image.Point{}, false, err
	}
	if data, mime, err = normalize(ctx, data, mime); err != nil {
		return nil, "", image.Point{}, false, err
	}
	if check {
		if err := work(r, call, func() error { return validate(data, mime) }); err != nil {
			return nil, "", image.Point{}, false, err
		}
	}
	if region == nil {
		return data, mime, image.Point{}, false, nil
	}
	cut, offset, err := crop(data, mime, region)
	if err != nil {
		return nil, "", image.Point{}, false, err
	}
	return cut, "image/png", offset, true, nil
}

// Description of the image by the vision helper model, and the note on how it was scaled
func (r *registrant) describe(ctx context.Context, call *tools.Call, raw, region any, prompt string) (string, string, error) {
	source, ok := raw.(string)
	if !ok {
		return "", "", &prepError{"image_url is required"}
	}
	data, mime, offset, cropped, err := r.prepare(ctx, call, source, region, false)
	if err != nil {
		return "", "", err
	}
	var scaled *scale
	shrink := func() error {
		var err error
		data, mime, scaled, err = work(r, call, func() resizedResult {
			out, kind, s, err := resize(data, mime, retryPayload, 0, false)
			return resizedResult{out, kind, s, err}
		}).unpack()
		return err
	}
	if size(len(data), mime) > maxPayload {
		if err := shrink(); err != nil {
			return "", "", err
		}
		if size(len(data), mime) > maxPayload {
			return "", "", errors.New(tooLarge(size(len(data), mime)))
		}
	}
	if call.Env.Aux == nil {
		return "", "", errors.New("no auxiliary model is configured for vision")
	}
	request := r.request(call, visionconfig.Task, prompt, &sessionsv1.Media{MimeType: mime, Data: data})
	analysis, err := call.Env.Aux.Complete(ctx, request)
	if err != nil {
		if !(hinted(err, append(sizeHints, "image_url", "invalid_request")) && size(len(data), mime) > retryPayload) {
			return "", "", err
		}
		if err := shrink(); err != nil {
			return "", "", err
		}
		request.Messages = auxiliary.Prompt("", prompt, &sessionsv1.Media{MimeType: mime, Data: data})
		if analysis, err = call.Env.Aux.Complete(ctx, request); err != nil {
			return "", "", err
		}
	}
	if analysis == "" {
		if analysis, err = call.Env.Aux.Complete(ctx, request); err != nil {
			return "", "", err
		}
	}
	return analysis, note(scaled, offset, cropped), nil
}

// Image a resize gave, or why it failed
type resizedResult struct {
	data  []byte
	mime  string
	scale *scale
	err   error
}

func (r resizedResult) unpack() ([]byte, string, *scale, error) {
	return r.data, r.mime, r.scale, r.err
}

// Call of a helper task about one image or video, with the vision temperature
func (r *registrant) request(call *tools.Call, task, prompt string, media *sessionsv1.Media) auxiliary.Request {
	temperature := auxiliary.VisionTemperature(call.Env.Config)
	return auxiliary.Request{Task: task, Messages: auxiliary.Prompt("", prompt, media), Temperature: &temperature}
}

func (r *registrant) video(ctx context.Context, call *tools.Call) string {
	var args struct {
		VideoURL any `json:"video_url"`
		Question any `json:"question"`
	}
	if err := json.Unmarshal([]byte(cmp.Or(call.RawArgs, "{}")), &args); err != nil {
		return tools.ErrorResult(err)
	}
	source := text(args.VideoURL)
	prompt := "Fully describe and explain everything happening in this video, including visual content, motion, audio cues, text overlays, and scene " +
		"transitions. Then answer the following question:\n\n" + text(args.Question)
	return r.analyze(ctx, call, "video", source, prompt, func() (string, string, error) {
		if _, ok := args.VideoURL.(string); !ok {
			return "", "", errors.New("Invalid video source. Provide an HTTP/HTTPS URL or a valid local file path.")
		}
		data, mime, err := materialize(ctx, call, source)
		if err != nil {
			return "", "", err
		}
		if payload := size(len(data), mime); payload > maxVideo {
			return "", "", fmt.Errorf("Video too large for API: base64 payload is %.1f MB (limit %.0f MB). Compress or trim the video and retry.", megabytes(payload), megabytes(maxVideo))
		}
		if call.Env.Aux == nil {
			return "", "", errors.New("no auxiliary model is configured for vision")
		}
		request := r.request(call, visionconfig.VideoTask, prompt, &sessionsv1.Media{MimeType: mime, Data: data})
		analysis, err := call.Env.Aux.Complete(ctx, request)
		if err == nil && analysis == "" {
			analysis, err = call.Env.Aux.Complete(ctx, request)
		}
		return analysis, "", err
	})
}

// Result of an image or video analysis as the model reads it, a failure explained by what the error says
func (r *registrant) analyze(ctx context.Context, call *tools.Call, kind, source, prompt string, stage func() (string, string, error)) string {
	if ctx.Err() != nil {
		return failure("Interrupted")
	}
	model := r.model(call, kind)
	entry := map[string]any{"parameters": map[string]any{kind + "_url": source, "user_prompt": strs.Clip(prompt, 200), "model": model}, "success": false}
	analysis, scaled, err := stage()
	var body any
	if err != nil {
		message := fmt.Sprintf("Error analyzing %s: %s", kind, err)
		template := fmt.Sprintf("There was a problem with the request and the %s could not be analyzed. Error: {e}", kind)
		rules := imageRules
		if kind == "video" {
			rules = videoRules
		}
		for _, rule := range rules {
			if hinted(err, rule.hints) {
				template = rule.template
				break
			}
		}
		named := model
		if named == "" {
			named = "None"
		}
		entry["error"] = message
		body = struct {
			Success  bool   `json:"success"`
			Error    string `json:"error"`
			Analysis string `json:"analysis"`
		}{false, message, strings.ReplaceAll(strings.ReplaceAll(template, "{e}", err.Error()), "{model}", named)}
	} else {
		entry["success"], entry["analysis_length"] = true, len([]rune(analysis))
		if analysis == "" {
			analysis = fmt.Sprintf("There was a problem with the request and the %s could not be analyzed.", kind)
		}
		if scaled != "" {
			analysis = "[" + scaled + "] " + analysis
		}
		body = struct {
			Success   bool   `json:"success"`
			Analysis  string `json:"analysis"`
			ScaleNote string `json:"scale_note,omitempty"`
		}{true, analysis, scaled}
	}
	r.log.record(call, kind+"_analyze_tool", entry)
	out, _ := json.MarshalIndent(body, "", "  ")
	return string(out)
}

// Model the analysis runs on as its settings name it, empty when icebath picks
func (r *registrant) model(call *tools.Call, kind string) string {
	key := func(task string) string {
		return strings.TrimSpace(call.Env.Config.GetString(auxiliary.Task{Name: task}.Key(auxiliaryconfig.Model)))
	}
	if kind == "video" {
		return cmp.Or(key(visionconfig.VideoTask), key(visionconfig.Task))
	}
	return key(visionconfig.Task)
}

// Loads a subagent may make of one image when the profile sets no limit of its own, the main agent's staying unlimited
const subagentLooks = 3

// Most loads of one image the session may make: the profile's limit when it sets one, else a subagent's own; 0 for no limit
func looks(env *tools.Env) int {
	cfg := env.Config
	if _, setting, ok := cfg.Setting(visionconfig.MaxCallsPerImage); ok {
		if source, _ := cfg.Source(setting); source != config.FromDefault {
			return max(cfg.GetInt(visionconfig.MaxCallsPerImage), 0)
		}
	}
	if env.Delegated != nil {
		return subagentLooks
	}
	return 0
}

// Counts a load of the image in the session, refusing it past the limit. A limit of 0 only counts
func (r *registrant) reserve(session, key string, limit int) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, seen := range r.looks {
		if seen.session != session || seen.image != key {
			continue
		}
		if limit > 0 && seen.count >= limit {
			return failure(fmt.Sprintf("vision_analyze refused: this image has already been loaded into context %d time(s) in this session (region crops of the same "+
				"file count too), and every native load re-sends the full image on each later API call. Answer from what you can already see, or ask the user. "+
				"(vision.max_calls_per_image = %d; 0 = unlimited)", seen.count, limit))
		}
		r.looks[i].count++
		return ""
	}
	r.looks = append(r.looks, look{session: session, image: key, count: 1})
	if len(r.looks) > remembered {
		r.looks = r.looks[len(r.looks)-remembered:]
	}
	return ""
}

// Takes back a load counted for an image that did not load
func (r *registrant) release(session, key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, seen := range r.looks {
		if seen.session == session && seen.image == key && seen.count > 0 {
			r.looks[i].count--
		}
	}
}

// What makes two loads the same image: the hash of a data URL, a URL without its fragment, else the file's full path
func imageKey(call *tools.Call, source string) string {
	source = strings.TrimSpace(source)
	switch {
	case strings.HasPrefix(source, "data:"):
		sum := sha256.Sum256([]byte(source))
		return "data:" + hex.EncodeToString(sum[:])[:32]
	case strings.HasPrefix(source, "http://") || strings.HasPrefix(source, "https://"):
		if parsed, err := url.Parse(source); err == nil {
			parsed.Fragment = ""
			return "url:" + parsed.String()
		}
		return "url:" + source
	}
	if len(source) >= 7 && strings.EqualFold(source[:7], "file://") {
		source = source[7:]
	}
	path := home(source)
	if !filepath.IsAbs(path) {
		path = filepath.Join(call.Env.Cwd, path)
	}
	if real, err := filepath.EvalSymlinks(path); err == nil {
		path = real
	}
	return "file:" + path
}

// Failure of the tool as the model reads it
func failure(message string) string {
	out, _ := json.Marshal(struct {
		Error   string `json:"error"`
		Success bool   `json:"success"`
	}{message, false})
	return string(out)
}

func tooLarge(payload int) string {
	return fmt.Sprintf("Image too large for vision API: base64 payload is %.1f MB (limit %.0f MB) even after resizing. Compress the image manually and retry.",
		megabytes(payload), megabytes(maxPayload))
}

func megabytes(n int) float64 { return float64(n) / (1 << 20) }

func hinted(err error, hints []string) bool {
	text := strings.ToLower(err.Error())
	for _, hint := range hints {
		if strings.Contains(text, hint) {
			return true
		}
	}
	return false
}

// Text of an argument as the model sent it, a string as it is and anything else as its JSON
func text(value any) string {
	switch value := value.(type) {
	case nil:
		return ""
	case string:
		return value
	}
	out, _ := json.Marshal(value)
	return string(out)
}

// The group of the tools that look at images and videos, whose options are the vision section
func (r *registrant) Groups() []tools.Group {
	return []tools.Group{{Title: "Vision", Sections: []string{visionconfig.Key}, Order: 50}}
}
