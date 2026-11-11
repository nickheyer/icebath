package tuiserver

import (
	"context"
	"encoding/base64"
	"io"
	"math/big"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pymimetypes"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/media"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	imageDataURLDefaultCap = 8_000_000
	imageDataURLMaxCap     = 16_000_000
)

var imagesRegistry = &handlerRegistry{}

func init() {
	imagesRegistry.method("image.generate", imageGenerate)
	imagesRegistry.install()
}

// Bytes of a remote image behind the SSRF guard with its content type, false when unsafe, failing or over cap
func fetchImageURL(ctx context.Context, env *tools.Env, ref string, limit int64) ([]byte, string, bool) {
	if !tools.IsSafeURL(env, ref) {
		return nil, "", false
	}
	client := tools.NewSSRFSafeClient(env, 60*time.Second, true)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ref, nil)
	if err != nil {
		return nil, "", false
	}
	req.Header.Set("User-Agent", "icebath-agent")
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", false
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, "", false
	}
	if raw := resp.Header.Get("Content-Length"); raw != "" {
		n, err := strconv.ParseInt(pystr.Strip(raw), 10, 64)
		if err != nil || n > limit {
			return nil, "", false
		}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, "", false
	}
	mime := resp.Header.Get("Content-Type")
	if mime == "" {
		mime = "image/png"
	}
	mime, _, _ = strings.Cut(mime, ";")
	return data, pystr.Strip(mime), true
}

// Bytes of a local image file with its guessed type, false when missing, unreadable or over cap
func readImageFile(ref string, limit int64) ([]byte, string, bool) {
	st, err := os.Stat(ref)
	if err != nil || !st.Mode().IsRegular() || st.Size() > limit {
		return nil, "", false
	}
	fh, err := os.Open(ref)
	if err != nil {
		return nil, "", false
	}
	defer fh.Close()
	data, err := io.ReadAll(io.LimitReader(fh, limit+1))
	if err != nil {
		return nil, "", false
	}
	mime, _, err := pymimetypes.GuessType(ref, true)
	if err != nil || mime == "" {
		mime = "image/png"
	}
	return data, mime, true
}

// Data URL of a fetched URL or local image path, false when missing, over cap or failing
func imageToDataURL(ctx context.Context, env *tools.Env, ref string, limit int64) (dataURL string, found bool) {
	defer func() {
		if recover() != nil {
			dataURL, found = "", false
		}
	}()
	var data []byte
	var mime string
	var ok bool
	if strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://") {
		data, mime, ok = fetchImageURL(ctx, env, ref, limit)
	} else {
		data, mime, ok = readImageFile(ref, limit)
	}
	if !ok || int64(len(data)) > limit {
		return "", false
	}
	if !strings.HasPrefix(mime, "image/") {
		mime = "image/png"
	}
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data), true
}

// Whether an image generation backend is configured, false when the probe itself fails
func imageGenerationAvailable(env *tools.Env) (available bool) {
	defer func() {
		if recover() != nil {
			available = false
		}
	}()
	return media.CheckImageGenerationRequirements(env)
}

// Data URL cap of the request, the default for a missing or unparsable value and never above the maximum
func imageDataURLCap(params map[string]any) int64 {
	raw, present := params["max_bytes"]
	if !present {
		raw = imageDataURLDefaultCap
	}
	if !pyrepr.Truthy(raw) {
		raw = imageDataURLDefaultCap
	}
	n, err := pyint.Of(raw)
	if err != nil {
		return imageDataURLDefaultCap
	}
	if n.Cmp(big.NewInt(imageDataURLMaxCap)) > 0 {
		return imageDataURLMaxCap
	}
	return n.Int64()
}

// Generated image of a prompt as its backend reference and a data URL a remote renderer can draw
func imageGenerate(ctx context.Context, rid any, params map[string]any) map[string]any {
	env := tools.EnvForProfileHome(ctx, nil, home.Dir(ctx))
	available := imageGenerationAvailable(env)
	if provider.IsTruthyValue(params["probe"]) {
		return ok(rid, map[string]any{"available": available})
	}
	if !available {
		return ok(rid, map[string]any{"available": false, "success": false,
			"error": "No image generation backend configured (run `icebath tools` to enable one)."})
	}
	prompt := paramText(params, "prompt")
	if prompt == "" {
		return rpcErr(rid, 4071, "prompt required", nil)
	}
	aspect := "square"
	if raw := params["aspect_ratio"]; pyrepr.Truthy(raw) {
		aspect = pyrepr.Str(raw)
	}
	aspect = strings.ToLower(pystr.Strip(aspect))
	limit := imageDataURLCap(params)
	result, failure := runImageGenerate(ctx, env, prompt, aspect)
	if failure != "" {
		return rpcErr(rid, 5071, failure, nil)
	}
	if !pyrepr.Truthy(result["success"]) {
		msg := "generation failed"
		if pyrepr.Truthy(result["error"]) {
			msg = pyrepr.Str(result["error"])
		}
		return ok(rid, map[string]any{"available": true, "success": false, "error": msg})
	}
	imageRef := pyrepr.Str(orEmpty(result["image"]))
	out := map[string]any{"available": true, "success": true, "image": imageRef}
	if imageRef != "" {
		if dataURL, found := imageToDataURL(ctx, env, imageRef, limit); found {
			out["image_data"] = dataURL
		}
	}
	return ok(rid, out)
}

// Decoded dispatcher result of one generation, else the message of what it raised
func runImageGenerate(ctx context.Context, env *tools.Env, prompt, aspect string) (result map[string]any, failure string) {
	defer func() {
		if r := recover(); r != nil {
			result, failure = nil, panicMessage(r)
		}
	}()
	decoded, err := pyjson.Loads(media.HandleImageGenerate(ctx, env, map[string]any{"prompt": prompt, "aspect_ratio": aspect}))
	if err != nil {
		return nil, err.Error()
	}
	d, isDict := pyjson.Plain(decoded).(map[string]any)
	if !isDict {
		return nil, "'" + pyrepr.TypeName(pyjson.Plain(decoded)) + "' object has no attribute 'get'"
	}
	return d, ""
}
