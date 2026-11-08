package media

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pymimetypes"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	kreaDefaultModel             = "krea-2-medium"
	kreaDefaultResolution        = "1K"
	kreaDefaultStyleStrength     = 0.6
	kreaMaxStyleReferences       = 10
	kreaMaxKrea3ReferenceImages  = 4
	kreaPollInitialInterval      = 2.0
	kreaPollMaxInterval          = 5.0
	kreaPollBackoff              = 1.3
	kreaPollTimeoutSeconds       = 180.0
	kreaEnhancePath              = "/generate/enhance/krea/enhance"
	kreaEnhanceScaleFactor       = 2
	kreaUserAgent                = "Icebath-Agent/1.0 (krea-image-gen)"
	kreaManagedUnsupportedPrefix = "Managed Krea (Nous Subscription) does not support "
)

var kreaModels = imageModelCatalog{order: []string{"krea-3", "krea-2-medium", "krea-2-large", "krea-2-medium-turbo"}, meta: map[string]map[string]any{
	"krea-3": {"display": "Krea 3", "speed": "~5s (1K) / ~40s (2K, high thinking)",
		"strengths": "Generates and edits existing images from instructions; keeps products, characters, logos and text consistent.",
		"price":     "$0.03 (1K) / $0.06 (2K), +$0.02 (1K) or $0.07 (2K) per reference image, +$0.02 high thinking",
		"path":      "/generate/image/krea/krea-3", "edit": true, "upscale": false},
	"krea-2-medium": {"display": "Krea 2 Medium", "speed": "~15-25s",
		"strengths": "Illustration, anime, painting, expressive styles. Faster + cheaper.",
		"price":     "$0.030 (text) / $0.035 (style refs) / $0.040 (moodboards)",
		"path":      "/generate/image/krea/krea-2/medium", "upscale": false},
	"krea-2-large": {"display": "Krea 2 Large", "speed": "~25-60s",
		"strengths": "Photorealism, raw textured looks (motion blur, grain), expressive styles.",
		"price":     "$0.060 (text) / $0.065 (style refs) / $0.070 (moodboards)",
		"path":      "/generate/image/krea/krea-2/large", "upscale": false},
	"krea-2-medium-turbo": {"display": "Krea 2 Medium Turbo", "speed": "~8-15s",
		"strengths": "Fastest Krea 2 — medium quality at lower latency / cost.",
		"price":     "$0.015 (text) / $0.0175 (style refs)", "path": "/generate/image/krea/krea-2/medium-turbo", "upscale": false},
}}

// Ids of every Krea model the plugin serves
var kreaModelIDs = func() map[string]bool {
	ids := map[string]bool{}
	for _, id := range kreaModels.order {
		ids[id] = true
	}
	return ids
}()

var (
	kreaAspectMap          = map[string]string{"landscape": "16:9", "square": "1:1", "portrait": "9:16"}
	kreaRemotePrefixes     = []string{"http://", "https://", "data:"}
	kreaValidCreativity    = map[string]bool{"raw": true, "low": true, "medium": true, "high": true}
	kreaK2Sliders          = []string{"intensity", "complexity", "movement"}
	kreaKrea3Resolutions   = map[string]bool{"1K": true, "2K": true}
	kreaRetryablePoll      = map[int]bool{408: true, 409: true, 425: true, 429: true, 500: true, 502: true, 503: true, 504: true}
	kreaTerminalStates     = map[string]bool{"completed": true, "failed": true, "cancelled": true}
	kreaManagedUnsupported = [][2]string{{"trained styles (LoRAs)", "styles"}, {"moodboards", "moodboards"}}
	kreaLog                = slog.Default().With("logger", "plugins.image_gen.krea")

	kreaBaseURL                = "https://api.krea.ai"
	kreaRequestTimeout         = 30 * time.Second
	kreaMaxLocalReferenceBytes = 3 * 1024 * 1024
	kreaSleep                  = time.Sleep
	kreaNow                    = time.Now
	kreaManagedGateway         = resolveManagedKreaGateway
	// The image_gen section the krea subsection lives under, swapped by tests
	kreaLoadConfig = func(env *tools.Env) map[string]any { return loadImageGenConfig(env, "") }
	// Local cache path of a result URL with the save_url_image defaults
	kreaSaveURLImage = func(ctx context.Context, env *tools.Env, rawURL, prefix string) (string, error) {
		return saveURLImage(ctx, env, rawURL, prefix, 60*time.Second, 25*1024*1024)
	}
)

// Fatal poll outcome kinds mapped to their error type and message builder
var kreaPollFailures = map[string]struct {
	errorType string
	build     func(jobID, detail string) string
}{
	"http": {"api_error", func(jobID, detail string) string {
		return fmt.Sprintf("Krea poll failed (%s) for job %s", detail, jobID)
	}},
	"timeout": {"timeout", func(jobID, detail string) string {
		return fmt.Sprintf("Krea poll timed out for job %s: %s", jobID, detail)
	}},
	"invalid_json": {"invalid_response", func(_, detail string) string { return "Krea poll returned invalid JSON: " + detail }},
	"deadline": {"timeout", func(jobID, detail string) string {
		return fmt.Sprintf("Krea job %s did not complete within %ds (last status: %s)", jobID, int(kreaPollTimeoutSeconds), detail)
	}},
}

// The image_gen.krea mapping, empty when absent or not a mapping
func kreaSection(env *tools.Env) map[string]any {
	switch s := kreaLoadConfig(env)["krea"].(type) {
	case map[string]any:
		return s
	case config.Tree:
		return s
	}
	return map[string]any{}
}

// True unless image_gen.krea.fallback_to_fal is literally false
func kreaFallbackToFalEnabled(env *tools.Env) bool {
	b, isBool := kreaSection(env)["fallback_to_fal"].(bool)
	return !isBool || b
}

// Only an unreachable server or a 429 proves Krea created no job
func kreaFallbackEligible(f *httpFailure) bool {
	return f.Kind == "unreachable" || (f.Kind == "http" && f.Status == 429)
}

// Explicit, KREA_IMAGE_MODEL, image_gen.krea.model, image_gen.model, then the default model
func kreaResolveModel(env *tools.Env, explicit string) (string, map[string]any) {
	return resolveStaticModel(env, kreaModels, kreaDefaultModel, staticModelOptions{EnvVar: "KREA_IMAGE_MODEL", ConfigKey: "krea",
		Explicit: explicit, Config: kreaLoadConfig(env)})
}

// Managed Krea gateway for a nous or never configured selection without a direct key, nil otherwise
func resolveManagedKreaGateway(env *tools.Env) *tools.ManagedToolGatewayConfig {
	selected := tools.ReadSelection(env, "image_gen")
	if selected != "" && selected != tools.NousManagedProvider {
		return nil
	}
	if selected == "" && secret(env, "KREA_API_KEY") != "" {
		return nil
	}
	gw, err := tools.ResolveManagedToolGateway(env, "krea", nil, nil)
	if err != nil {
		kreaLog.Debug(fmt.Sprintf("Managed Krea gateway resolution failed: %s", err))
		return nil
	}
	return gw
}

// Valid creativity from the argument then config, medium otherwise
func kreaResolveCreativity(env *tools.Env, value any) string {
	for _, c := range []any{value, kreaSection(env)["creativity"]} {
		if s, ok := c.(string); ok && kreaValidCreativity[pystr.Lower(pystr.Strip(s))] {
			return pystr.Lower(pystr.Strip(s))
		}
	}
	return "medium"
}

// Bearer and user agent headers with a JSON content type and a fresh idempotency key when managed
func kreaHeaders(token string, managed, jsonBody bool) map[string]string {
	h := map[string]string{"Authorization": "Bearer " + token, "User-Agent": kreaUserAgent}
	if jsonBody {
		h["Content-Type"] = "application/json"
	}
	if managed {
		h["x-idempotency-key"] = uuid4()
	}
	return h
}

// Error message of a failed submit body, the first 300 characters of the text when it names none
func kreaSubmitErrorMessage(_ int, body []byte) string {
	fallback := pystr.Slice(strings.ToValidUTF8(string(body), "�"), 0, 300)
	parsed, err := pyjson.LoadsBytes(body)
	if err != nil {
		return fallback
	}
	obj, ok := parsed.(*pyjson.Object)
	if !ok {
		return fallback
	}
	var message any
	if e, ok := kreaGet(obj, "error").(*pyjson.Object); ok {
		message = kreaGet(e, "message")
	} else if message = kreaGet(obj, "message"); !pyTruthy(message) {
		message = kreaGet(obj, "detail")
	}
	if !pyTruthy(message) {
		return fallback
	}
	return pyrepr.Str(message)
}

// Value under key of a JSON object, nil for anything else
func kreaGet(node any, key string) any {
	v, _, _ := jsonObjectGet(node, key)
	return v
}

// A dict job whose status is terminal or whose completed_at is set
func kreaIsTerminal(job any) bool {
	if _, _, isObj := jsonObjectGet(job, "status"); !isObj {
		return false
	}
	s, _ := kreaGet(job, "status").(string)
	return kreaTerminalStates[s] || pyTruthy(kreaGet(job, "completed_at"))
}

// Request construction failure the poll loop raises instead of retrying
type kreaRequestError struct{ err error }

func (e *kreaRequestError) Error() string { return e.err.Error() }
func (e *kreaRequestError) Unwrap() error { return e.err }

// GET of a job URL returning the status and body, transport failures as errors
func kreaGetJob(ctx context.Context, rawURL string, headers map[string]string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return 0, nil, &kreaRequestError{err}
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	client := &http.Client{Timeout: kreaRequestTimeout}
	resp, err := client.Do(req)
	if err != nil {
		var urlErr *url.Error
		if errors.As(err, &urlErr) && (strings.Contains(urlErr.Err.Error(), "unsupported protocol scheme") || strings.Contains(urlErr.Err.Error(), "no Host in request URL")) {
			return 0, nil, &kreaRequestError{err}
		}
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, raw, nil
}

// Text of a poll failure detail as Python formats it
func kreaDetailText(detail any) string {
	if err, ok := detail.(error); ok {
		return err.Error()
	}
	return pyrepr.Str(detail)
}

// Polls the job until terminal, onError answering fatal main path failures, nil when the enhance path gives up
func kreaPollJob(ctx context.Context, baseURL, token, jobID string, timeout time.Duration, onError func(kind string, detail any) map[string]any) (any, error) {
	jobURL := baseURL + "/jobs/" + jobID
	headers := kreaHeaders(token, false, false)
	interval := kreaPollInitialInterval
	deadline := kreaNow().Add(timeout)
	var lastStatus any
	enhance := onError == nil
	expired := func() bool { return !kreaNow().Before(deadline) }
	giveUp := func(kind string, detail any, warning string) any {
		if onError == nil {
			kreaLog.Warn(warning)
			return nil
		}
		return onError(kind, detail)
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		kreaSleep(time.Duration(interval * float64(time.Second)))
		interval = min(interval*kreaPollBackoff, kreaPollMaxInterval)
		status, raw, err := kreaGetJob(ctx, jobURL, headers)
		if err != nil {
			var reqErr *kreaRequestError
			if errors.As(err, &reqErr) || ctx.Err() != nil {
				if !enhance || ctx.Err() != nil {
					return nil, err
				}
				if expired() {
					kreaLog.Warn(fmt.Sprintf("Krea enhance poll gave up for job %s: %s", jobID, err))
					return nil, nil
				}
				continue
			}
			if !enhance {
				kreaLog.Warn(fmt.Sprintf("Krea poll transient error for job %s: %s", jobID, err))
			}
			if expired() {
				return giveUp("timeout", err, fmt.Sprintf("Krea enhance poll gave up for job %s: %s", jobID, err)), nil
			}
			continue
		}
		if status >= 400 && status < 600 {
			if !enhance {
				kreaLog.Error(fmt.Sprintf("Krea poll failed (%d) for job %s", status, jobID))
			}
			if !kreaRetryablePoll[status] || expired() {
				return giveUp("http", status, fmt.Sprintf("Krea enhance poll failed (%d) for job %s", status, jobID)), nil
			}
			continue
		}
		job, err := pyjson.LoadsBytes(raw)
		if err != nil {
			if !enhance {
				kreaLog.Warn(fmt.Sprintf("Krea poll returned invalid JSON for job %s: %s", jobID, err))
			}
			if expired() {
				return giveUp("invalid_json", err, fmt.Sprintf("Krea enhance poll gave up for job %s: %s", jobID, err)), nil
			}
			continue
		}
		if s, ok := kreaGet(job, "status").(string); ok {
			lastStatus = s
		}
		if kreaIsTerminal(job) {
			return job, nil
		}
		if expired() {
			return giveUp("deadline", lastStatus, fmt.Sprintf("Krea enhance job %s did not finish in %ds", jobID, int(timeout.Seconds()))), nil
		}
	}
}

// First result URL from result.urls then result.url, empty when there is none
func kreaExtractResultURL(job any) string {
	result := kreaGet(job, "result")
	if _, _, isObj := jsonObjectGet(result, "urls"); !isObj {
		return ""
	}
	candidates := []any{}
	if urls, ok := kreaGet(result, "urls").([]any); ok {
		candidates = append(candidates, urls...)
	}
	candidates = append(candidates, kreaGet(result, "url"))
	for _, c := range candidates {
		if s, ok := c.(string); ok && pystr.Strip(s) != "" {
			return pystr.Strip(s)
		}
	}
	return ""
}

// Enhanced URL of a Krea Enhance pass, empty on any failure, an error only when the call is cancelled
func kreaEnhanceImage(ctx context.Context, baseURL, token, imageURL, prompt string, managed bool) (string, error) {
	payload := pyjson.NewObject()
	payload.Set("image_url", imageURL)
	payload.Set("image_scaling_factor", kreaEnhanceScaleFactor)
	payload.Set("prompt", prompt)
	body, failure, err := postJSON(ctx, baseURL+kreaEnhancePath, kreaHeaders(token, managed, true), payload,
		requestsTimeout{Connect: kreaRequestTimeout, Read: kreaRequestTimeout}, "Krea", defaultErrorMessage, true)
	if err == nil && failure != nil {
		err = errors.New(failure.Error)
	}
	if err == nil && pyTruthy(body) {
		if _, _, isObj := jsonObjectGet(body, "job_id"); !isObj {
			err = fmt.Errorf("'%s' object has no attribute 'get'", pyrepr.TypeName(body))
		}
	}
	if err != nil {
		kreaLog.Warn(fmt.Sprintf("Krea Enhance submit failed: %s", err))
		return "", ctx.Err()
	}
	jobID, _ := kreaGet(body, "job_id").(string)
	if jobID == "" {
		kreaLog.Warn("Krea Enhance submit response missing job_id")
		return "", nil
	}
	job, err := kreaPollJob(ctx, baseURL, token, jobID, kreaPollTimeoutSeconds*time.Second, nil)
	if err != nil {
		return "", err
	}
	status, _ := kreaGet(job, "status").(string)
	if _, _, isObj := jsonObjectGet(job, "status"); !isObj || status == "failed" || status == "cancelled" {
		kreaLog.Warn(fmt.Sprintf("Krea Enhance job %s did not complete successfully", jobID))
		return "", nil
	}
	return kreaExtractResultURL(job), nil
}

// Copy of a dict reference with its url replaced, nil when the value is no dict
func kreaDictWithURL(ref any, newURL string) any {
	switch m := ref.(type) {
	case *pyjson.Object:
		c := m.Copy()
		c.Set("url", newURL)
		return c
	case map[string]any:
		c := make(map[string]any, len(m)+1)
		for k, v := range m {
			c[k] = v
		}
		c["url"] = newURL
		return c
	}
	return nil
}

// Primary and reference images first, then legacy style references, strings deduped in order and capped at ten
func kreaCollectStyleRefs(imageURL string, referenceImageURLs []string, legacy any) []any {
	refs := []any{}
	for _, s := range collectSourceImages(imageURL, referenceImageURLs, -1) {
		refs = append(refs, s)
	}
	if list, ok := legacy.([]any); ok {
		for _, ref := range list {
			if s, ok := ref.(string); ok {
				if pystr.Strip(s) != "" {
					refs = append(refs, pystr.Strip(s))
				}
			} else if pyTruthy(ref) {
				refs = append(refs, ref)
			}
		}
	}
	seen := map[string]bool{}
	deduped := []any{}
	for _, r := range refs {
		if s, ok := r.(string); ok {
			if seen[s] {
				continue
			}
			seen[s] = true
		}
		deduped = append(deduped, r)
	}
	if len(deduped) > kreaMaxStyleReferences {
		deduped = deduped[:kreaMaxStyleReferences]
	}
	return deduped
}

// Local image files embedded as data URIs under the total byte budget, remote sources passed through
func kreaInlineLocalImages(env *tools.Env, refs []any, fail errorFn) ([]any, map[string]any) {
	inlined := []any{}
	total := 0
	for _, ref := range refs {
		source := ref
		if _, _, isObj := jsonObjectGet(ref, "url"); isObj {
			source = kreaGet(ref, "url")
		}
		s, ok := source.(string)
		if !ok || hasAnyPrefix(strings.ToLower(s), kreaRemotePrefixes) {
			inlined = append(inlined, ref)
			continue
		}
		path := pypath.PathStr(pypath.Expanduser(s))
		if denied := tools.ReadBlockError(env, path); denied != "" {
			return []any{}, fail(denied, "invalid_image_url")
		}
		if st, err := os.Stat(path); err != nil || !st.Mode().IsRegular() {
			return []any{}, fail("Reference image not found: "+s, "invalid_image_url")
		}
		data, err := readAtMost(path, kreaMaxLocalReferenceBytes-total+1)
		if err != nil {
			return []any{}, fail(fmt.Sprintf("Reference image unreadable: %s (%s)", s, pypath.OSErrorText(err)), "invalid_image_url")
		}
		total += len(data)
		if total > kreaMaxLocalReferenceBytes {
			mb, _ := pyrepr.Format(float64(kreaMaxLocalReferenceBytes)/float64(1<<20), "g")
			return []any{}, fail(fmt.Sprintf("Local reference images total over %s MB; resize them or pass public URLs", mb), "source_too_large")
		}
		mime, _, _ := pymimetypes.GuessType(filepath.Base(path), true)
		if mime == "" {
			mime = "image/png"
		}
		dataURI := "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data)
		if d := kreaDictWithURL(ref, dataURI); d != nil {
			inlined = append(inlined, d)
		} else {
			inlined = append(inlined, dataURI)
		}
	}
	return inlined, nil
}

// Whether s starts with any of the prefixes
func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// Up to n bytes of the file at path
func readAtMost(path string, n int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, int64(n)))
}

// Krea 2 body with seed, in range sliders, styles, style reference objects and at most one moodboard
func kreaBuildPayload(prompt, kreaAR, creativity string, styleRefs []any, kwargs map[string]any) *pyjson.Object {
	payload := pyjson.NewObject()
	payload.Set("prompt", prompt)
	payload.Set("aspect_ratio", kreaAR)
	payload.Set("resolution", kreaDefaultResolution)
	payload.Set("creativity", creativity)
	if pyIsInt(kwargs["seed"]) {
		payload.Set("seed", kwargs["seed"])
	}
	for _, slider := range kreaK2Sliders {
		v := kwargs[slider]
		if _, isBool := v.(bool); isBool {
			continue
		}
		if n, ok := pyrepr.Int(v); ok && n.IsInt64() && n.Int64() >= -100 && n.Int64() <= 100 {
			payload.Set(slider, v)
		}
	}
	if styles, ok := kwargs["styles"].([]any); ok && len(styles) > 0 {
		payload.Set("styles", styles)
	}
	if len(styleRefs) > 0 {
		objs := make([]any, len(styleRefs))
		for i, ref := range styleRefs {
			if s, ok := ref.(string); ok {
				o := pyjson.NewObject()
				o.Set("url", s)
				o.Set("strength", kreaDefaultStyleStrength)
				objs[i] = o
			} else {
				objs[i] = ref
			}
		}
		payload.Set("image_style_references", objs)
	}
	if moodboards, ok := kwargs["moodboards"].([]any); ok && len(moodboards) > 0 {
		payload.Set("moodboards", moodboards[:1])
	}
	return payload
}

// Krea 2 body whose pictures are style references, refusing managed LoRAs and moodboards before reading local files
func kreaKrea2Payload(env *tools.Env, prompt, kreaAR, imageURL string, referenceImageURLs []string, kwargs map[string]any, managed bool, fail errorFn) (*pyjson.Object, map[string]any) {
	styleRefs := kreaCollectStyleRefs(imageURL, referenceImageURLs, kwargs["image_style_references"])
	payload := kreaBuildPayload(prompt, kreaAR, kreaResolveCreativity(env, kwargs["creativity"]), styleRefs, kwargs)
	if managed {
		for _, u := range kreaManagedUnsupported {
			if _, ok := payload.Get(u[1]); ok {
				return payload, fail(fmt.Sprintf("%s%s. Set KREA_API_KEY to use Krea directly, or omit `%s`.", kreaManagedUnsupportedPrefix, u[0], u[1]),
					"unsupported_argument")
			}
		}
	}
	if refs, _ := payload.Get("image_style_references"); pyTruthy(refs) {
		inlined, errEnv := kreaInlineLocalImages(env, refs.([]any), fail)
		payload.Set("image_style_references", inlined)
		if errEnv != nil {
			return payload, errEnv
		}
	}
	return payload, nil
}

// Krea 3 body whose pictures are edited, with priced resolution and thinking mode and no aspect ratio for an edit
func kreaKrea3Payload(env *tools.Env, prompt, kreaAR, imageURL string, referenceImageURLs []string, kwargs map[string]any, fail errorFn) (*pyjson.Object, map[string]any) {
	seen := map[string]bool{}
	sources := []any{}
	for _, s := range collectSourceImages(imageURL, referenceImageURLs, -1) {
		if !seen[s] {
			seen[s] = true
			sources = append(sources, s)
		}
	}
	if len(sources) > kreaMaxKrea3ReferenceImages {
		sources = sources[:kreaMaxKrea3ReferenceImages]
	}
	imageURLs, errEnv := kreaInlineLocalImages(env, sources, fail)
	if errEnv != nil {
		return pyjson.NewObject(), errEnv
	}
	resolution := ""
	if r := kwargs["resolution"]; pyTruthy(r) {
		resolution = strings.ToUpper(pyrepr.Str(r))
	}
	if !kreaKrea3Resolutions[resolution] {
		resolution = kreaDefaultResolution
	}
	thinking := "fast"
	if t, _ := kwargs["thinking"].(string); t == "high" {
		thinking = "high"
	}
	payload := pyjson.NewObject()
	payload.Set("prompt", prompt)
	payload.Set("resolution", resolution)
	payload.Set("thinking_mode", thinking)
	if len(imageURLs) > 0 {
		payload.Set("image_urls", imageURLs)
	} else {
		payload.Set("aspect_ratio", kreaAR)
	}
	if pyIsInt(kwargs["seed"]) {
		payload.Set("seed", kwargs["seed"])
	}
	return payload, nil
}

// Submits the generation body and returns the job id or an error envelope marking whether a FAL fallback is safe
func kreaSubmitJob(ctx context.Context, baseURL, token, modelPath string, payload *pyjson.Object, managed bool, modelID string, fail errorFn) (string, map[string]any, error) {
	body, failure, err := postJSON(ctx, baseURL+modelPath, kreaHeaders(token, managed, true), payload,
		requestsTimeout{Connect: kreaRequestTimeout, Read: kreaRequestTimeout}, "Krea", kreaSubmitErrorMessage, false)
	if err != nil {
		return "", nil, err
	}
	if failure != nil {
		submitFail := func(errText, errorType string) map[string]any {
			out := fail(errText, errorType)
			out["fallback_eligible"] = kreaFallbackEligible(failure)
			return out
		}
		switch failure.Kind {
		case "http":
			kreaLog.Error(fmt.Sprintf("Krea submit failed (%d): %s", failure.Status, failure.Message))
			if managed && failure.Status >= 400 && failure.Status < 500 {
				hint := fmt.Sprintf("Model '%s' may not be enabled/priced on the Nous Portal's Krea gateway. "+
					"Set KREA_API_KEY to use Krea directly, or pick a different model via `icebath tools` → Image Generation.", modelID)
				if failure.Status == 429 {
					hint = "Krea's shared-key concurrency cap was hit — retry shortly."
				}
				return "", submitFail(fmt.Sprintf("Nous Subscription Krea gateway rejected '%s' (HTTP %d): %s. %s", modelID, failure.Status, failure.Message, hint), "api_error"), nil
			}
			return "", submitFail(failure.Error, "api_error"), nil
		case "timeout":
			return "", submitFail("Krea submit timed out (30s)", "timeout"), nil
		case "invalid_json":
			return "", submitFail("Krea returned invalid JSON on submit: "+failure.Message, "invalid_response"), nil
		}
		return "", submitFail(failure.Error, failure.ErrorType), nil
	}
	if _, _, isObj := jsonObjectGet(body, "job_id"); !isObj {
		return "", nil, fmt.Errorf("'%s' object has no attribute 'get'", pyrepr.TypeName(body))
	}
	jobID, _ := kreaGet(body, "job_id").(string)
	if jobID == "" {
		return "", fail("Krea submit response missing job_id", "invalid_response"), nil
	}
	return jobID, nil, nil
}

// Result URL of a terminal job or the failed, cancelled or empty result envelope
func kreaTerminalResultURL(job any, jobID string, fail errorFn) (string, map[string]any) {
	result := kreaGet(job, "result")
	_, _, resultIsObj := jsonObjectGet(result, "error")
	switch status, _ := kreaGet(job, "status").(string); status {
	case "failed":
		var errV any
		if resultIsObj {
			errV = kreaGet(result, "error")
		}
		detail := "unknown error"
		if pyTruthy(errV) {
			detail = pyrepr.Str(errV)
		}
		return "", fail(fmt.Sprintf("Krea job %s failed: %s", jobID, detail), "api_error")
	case "cancelled":
		return "", fail(fmt.Sprintf("Krea job %s was cancelled", jobID), "cancelled")
	}
	if !resultIsObj {
		return "", fail("Krea job completed but result was missing", "empty_response")
	}
	u := kreaExtractResultURL(job)
	if u == "" {
		return "", fail("Krea result contained no image URL", "empty_response")
	}
	return u, nil
}

// Explicit bool, then image_gen.krea.upscale, then the catalog default
func kreaUpscaleRequested(env *tools.Env, explicit any, meta map[string]any) bool {
	if b, ok := explicit.(bool); ok {
		return b
	}
	if b, ok := kreaSection(env)["upscale"].(bool); ok {
		return b
	}
	return pyTruthy(meta["upscale"])
}

// Krea image backend serving Krea 3 edits and the Krea 2 tiers
type kreaImageGenProvider struct {
	staticImageGenProvider
	env func() *tools.Env
}

func newKreaImageGenProvider() *kreaImageGenProvider {
	return &kreaImageGenProvider{env: pluginEnv, staticImageGenProvider: staticImageGenProvider{providerID: "krea", label: "Krea",
		models: kreaModels, defaultModelID: kreaDefaultModel, catalogFields: defaultCatalogFields,
		setup: apiKeySetup{Name: "Krea", Badge: "paid",
			Tag: "Krea 3 — generates and edits images ($0.03 at 1K, $0.06 at 2K) — and Krea 2 Medium ($0.03), Large ($0.06), Medium Turbo ($0.015) with style transfer and moodboards. Direct key or managed Nous Subscription gateway.",
			Key: "KREA_API_KEY", Prompt: "Krea API key", URL: "https://www.krea.ai/settings/api-tokens"}}}
}

// Available with a direct key or a ready managed Nous gateway
func (p *kreaImageGenProvider) IsAvailable() bool {
	env := p.env()
	return secret(env, "KREA_API_KEY") != "" || tools.IsManagedToolGatewayReady(env, "krea", nil, nil)
}

// Krea 3 edit controls or Krea 2 style controls for the stored model the dispatcher names
func (p *kreaImageGenProvider) Capabilities() map[string]any {
	env := p.env()
	stored, _ := kreaLoadConfig(env)["model"].(string)
	if _, meta := kreaResolveModel(env, stored); pyTruthy(meta["edit"]) {
		return map[string]any{"modalities": []any{"text", "image"}, "max_reference_images": kreaMaxKrea3ReferenceImages,
			"supports_upscale": true, "creative_controls": []any{"thinking", "resolution"}}
	}
	return map[string]any{"modalities": []any{"text", "image"}, "max_reference_images": kreaMaxStyleReferences,
		"supports_upscale": true, "creative_controls": []any{"creativity", "intensity", "complexity", "movement"},
		"source_image_role": "style"}
}

// Submits, polls, optionally enhances and caches one Krea generation through the managed gateway or a direct key
func (p *kreaImageGenProvider) Generate(ctx context.Context, env *tools.Env, req ImageGenRequest) (map[string]any, error) {
	prompt := pystr.Strip(req.Prompt)
	aspect := resolveAspectRatio(req.AspectRatio)
	kreaAR, ok := kreaAspectMap[aspect]
	if !ok {
		kreaAR = "1:1"
	}
	if prompt == "" {
		return promptRequiredError("krea", aspect), nil
	}
	kwargs := req.Kwargs
	managed := kreaManagedGateway(env)
	var baseURL, token string
	if managed != nil {
		baseURL, token = strings.TrimRight(managed.GatewayOrigin, "/"), managed.NousUserToken
	} else {
		baseURL, token = kreaBaseURL, secret(env, "KREA_API_KEY")
		if token == "" {
			return errorFactory("krea", aspect, "", "")("KREA_API_KEY not set. Run `icebath tools` → Image "+
				"Generation → Krea to configure, get a key at "+
				"https://www.krea.ai/settings/api-tokens, or sign in to "+
				"a Nous account with the managed Krea gateway enabled "+
				"(`icebath setup`).", "auth_required"), nil
		}
	}
	explicit, _ := kwargs["model"].(string)
	modelID, meta := kreaResolveModel(env, explicit)
	fail := errorFactory("krea", aspect, modelID, prompt)
	var payload *pyjson.Object
	var errEnv map[string]any
	if pyTruthy(meta["edit"]) {
		payload, errEnv = kreaKrea3Payload(env, prompt, kreaAR, req.ImageURL, req.ReferenceImageURLs, kwargs, fail)
	} else {
		payload, errEnv = kreaKrea2Payload(env, prompt, kreaAR, req.ImageURL, req.ReferenceImageURLs, kwargs, managed != nil, fail)
	}
	if errEnv != nil {
		return errEnv, nil
	}
	modelPath, _ := meta["path"].(string)
	jobID, errEnv, err := kreaSubmitJob(ctx, baseURL, token, modelPath, payload, managed != nil, modelID, fail)
	if err != nil {
		return nil, err
	}
	if errEnv != nil {
		return errEnv, nil
	}
	var pollErrors []map[string]any
	onError := func(kind string, detail any) map[string]any {
		f, ok := kreaPollFailures[kind]
		if !ok {
			f = kreaPollFailures["deadline"]
		}
		text := kreaDetailText(detail)
		if kind == "deadline" && !pyTruthy(detail) {
			text = "unknown"
		}
		pollErrors = append(pollErrors, fail(f.build(jobID, text), f.errorType))
		return pollErrors[len(pollErrors)-1]
	}
	job, err := kreaPollJob(ctx, baseURL, token, jobID, kreaPollTimeoutSeconds*time.Second, onError)
	if err != nil {
		return nil, err
	}
	if len(pollErrors) > 0 {
		return pollErrors[0], nil
	}
	if _, _, isObj := jsonObjectGet(job, "status"); !isObj {
		return fail("Krea returned non-dict job body", "invalid_response"), nil
	}
	resultURL, errEnv := kreaTerminalResultURL(job, jobID, fail)
	if errEnv != nil {
		return errEnv, nil
	}
	upscaled := false
	if kreaUpscaleRequested(env, kwargs["upscale"], meta) {
		enhanced, err := kreaEnhanceImage(ctx, baseURL, token, resultURL, prompt, managed != nil)
		if err != nil {
			return nil, err
		}
		if enhanced != "" {
			resultURL, upscaled = enhanced, true
		} else {
			kreaLog.Warn("Krea Enhance pass failed — returning native-resolution image")
		}
	}
	imageRef, err := kreaSaveURLImage(ctx, env, resultURL, "krea_"+modelID)
	if err != nil {
		kreaLog.Warn(fmt.Sprintf("Krea image URL %s could not be cached (%s); falling back to bare URL.", resultURL, err))
		imageRef = resultURL
	}
	resolution, _ := payload.Get("resolution")
	extra := map[string]any{"resolution": resolution, "job_id": jobID, "upscaled": upscaled}
	for _, kn := range [][2]string{{"aspect_ratio", "krea_aspect_ratio"}, {"creativity", "creativity"}, {"thinking_mode", "thinking_mode"}} {
		if v, ok := payload.Get(kn[0]); ok {
			extra[kn[1]] = v
		}
	}
	if upscaled {
		extra["upscale_factor"] = kreaEnhanceScaleFactor
	}
	if s, ok := kreaGet(job, "completed_at").(string); ok {
		extra["completed_at"] = s
	}
	refs, _ := payload.Get("image_style_references")
	urls, _ := payload.Get("image_urls")
	modality := "text"
	if pyTruthy(refs) || pyTruthy(urls) {
		modality = "image"
	}
	return imageSuccessResponse(imageRef, modelID, prompt, aspect, "krea", modality, extra), nil
}

func init() {
	plugins.RegisterBundled(imageGenBackendPlugin{name: "krea", version: "1.1.0", requiresEnv: []string{"KREA_API_KEY"},
		description: "Krea image generation backend: Krea 3 (generates and edits) and Krea 2 Medium, Large and Medium Turbo. Direct KREA_API_KEY or managed Nous Subscription gateway.",
		provider:    func() ImageGenProvider { return newKreaImageGenProvider() }})
}
