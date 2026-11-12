package tools

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/state"
)

// One recorded auxiliary completion
type auxCall struct {
	msgs []*state.Message
	opts AuxOptions
}

// Scripted auxiliary model recording every completion
type fakeVisionAux struct {
	mu    sync.Mutex
	calls []auxCall
	reply func(n int, msgs []*state.Message, opts AuxOptions) (string, error)
}

func (f *fakeVisionAux) Complete(ctx context.Context, msgs []*state.Message, opts AuxOptions) (string, error) {
	f.mu.Lock()
	f.calls = append(f.calls, auxCall{msgs: msgs, opts: opts})
	n := len(f.calls)
	f.mu.Unlock()
	if f.reply == nil {
		return "described", nil
	}
	return f.reply(n, msgs, opts)
}

// Number of completions made so far
func (f *fakeVisionAux) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// Auxiliary router double that answers the probe mode resolution
type resolvingAux struct {
	fakeVisionAux
	resolves map[string]bool
	err      error
	asked    []string
}

func (r *resolvingAux) ResolvesVision(ctx context.Context, provider string) (bool, error) {
	r.asked = append(r.asked, "vision|"+provider)
	return r.resolves[provider], r.err
}

// Media URL of the second content part of an auxiliary message
func auxMediaURL(t *testing.T, c auxCall, part string) string {
	t.Helper()
	parts := c.msgs[0].Content.([]any)
	return parts[1].(map[string]any)[part].(map[string]any)["url"].(string)
}

// Session env on the local backend with a scripted auxiliary model
func visionEnv(t *testing.T, aux Aux) *Env {
	t.Helper()
	env := isrcEnv(t, "local")
	env.Aux = aux
	stubSandbox(t, nil, nil)
	freshRepeatCounts(t)
	return env
}

// Noise PNG that defeats compression
func writeNoisePNG(t *testing.T, path string, w, h int, seed int64) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	rnd := rand.New(rand.NewSource(seed))
	rnd.Read(img.Pix)
	for i := 3; i < len(img.Pix); i += 4 {
		img.Pix[i] = 255
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := (&png.Encoder{CompressionLevel: png.BestSpeed}).Encode(f, img); err != nil {
		t.Fatal(err)
	}
	return path
}

// Size of the image inside a data URL
func dataURLSize(t *testing.T, u string) (int, int) {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(strings.SplitN(u, ",", 2)[1])
	if err != nil {
		t.Fatal(err)
	}
	w, h, _, err := rasterConfig(raw)
	if err != nil {
		t.Fatal(err)
	}
	return w, h
}

// Image URL of a native envelope
func envelopeURL(t *testing.T, result string) string {
	t.Helper()
	for _, p := range decodeResult(t, result)["content"].([]any) {
		if m := p.(map[string]any); m["type"] == "image_url" {
			return m["image_url"].(map[string]any)["url"].(string)
		}
	}
	t.Fatalf("no image part in %q", result)
	return ""
}

// Resolves test hostnames to loopback and allows private targets for one test
func loopbackHosts(t *testing.T) {
	t.Helper()
	t.Setenv("ICEBATH_ALLOW_PRIVATE_URLS", "true")
	prev := lookupHostIPs
	lookupHostIPs = func(ctx context.Context, host string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	}
	t.Cleanup(func() { lookupHostIPs = prev })
}

// URL on a named host served by the test server
func hostURL(srv *httptest.Server, host, path string) string {
	u, _ := url.Parse(srv.URL)
	return "http://" + host + ":" + u.Port() + path
}

// Writes a website blocklist into the session config file
func writeBlocklist(t *testing.T, env *Env, domains ...string) {
	t.Helper()
	body := "security:\n  website_blocklist:\n    enabled: true\n    domains:\n"
	for _, d := range domains {
		body += "      - " + d + "\n"
	}
	if err := os.WriteFile(filepath.Join(env.Home, "config.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestValidateImageURL(t *testing.T) {
	env := isrcEnv(t, "local")
	prev := lookupHostIPs
	t.Cleanup(func() { lookupHostIPs = prev })
	lookupHostIPs = func(ctx context.Context, host string) ([]netip.Addr, error) {
		if host == "localhost" {
			return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
		}
		return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
	}
	for _, u := range []string{"https://example.com/image.jpg", "http://cdn.example.org/photo.png", "https://cdn.example.com/abcdef123",
		"https://img.example.com/pic?w=200&h=200", "http://example.com:8080/image.png", "https://example.com/"} {
		if !validateImageURL(env, u) {
			t.Errorf("%q rejected", u)
		}
	}
	for _, u := range []string{"http://localhost:8080/image.png", "http://127.0.0.1/image.png", "http://", "https://", "http:", "", "   "} {
		if validateImageURL(env, u) {
			t.Errorf("%q accepted", u)
		}
	}
}

func TestImageToBase64DataURL(t *testing.T) {
	dir := t.TempDir()
	img := filepath.Join(dir, "test.png")
	os.WriteFile(img, append(append([]byte{}, isrcPNG...), make([]byte, 8)...), 0o644)
	if u, err := imageToBase64DataURL(img, ""); err != nil || !strings.HasPrefix(u, "data:image/png;base64,") {
		t.Fatalf("%q %v", u, err)
	}
	other := filepath.Join(dir, "test.bin")
	os.WriteFile(other, make([]byte, 16), 0o644)
	if u, _ := imageToBase64DataURL(other, "image/webp"); !strings.HasPrefix(u, "data:image/webp;base64,") {
		t.Fatalf("given mime ignored: %q", u)
	}
	if _, err := imageToBase64DataURL(filepath.Join(dir, "nonexistent.png"), ""); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing file: %v", err)
	}
}

func TestConfiguredAuxModelResolution(t *testing.T) {
	env := isrcEnv(t, "local")
	t.Setenv("AUXILIARY_VISION_MODEL", "custom/model-v1")
	if got := configuredAuxModel(env, []string{"vision"}, []string{"AUXILIARY_VISION_MODEL"}); got != "custom/model-v1" {
		t.Fatalf("env model %q", got)
	}
	t.Setenv("AUXILIARY_VISION_MODEL", "")
	if got := configuredAuxModel(env, []string{"vision"}, []string{"AUXILIARY_VISION_MODEL"}); got != "" {
		t.Fatalf("no model %q", got)
	}
	env.Config.Set("auxiliary.vision.model", "qwen3.7-plus")
	t.Setenv("AUXILIARY_VISION_MODEL", "env-model")
	if got := configuredAuxModel(env, []string{"vision"}, []string{"AUXILIARY_VISION_MODEL"}); got != "qwen3.7-plus" {
		t.Fatalf("config model %q", got)
	}
	env.Config.Set("auxiliary.vision.model", "")
	t.Setenv("AUXILIARY_VISION_MODEL", "fallback-model")
	if got := configuredAuxModel(env, []string{"vision"}, []string{"AUXILIARY_VISION_MODEL"}); got != "fallback-model" {
		t.Fatalf("fallback model %q", got)
	}
	t.Setenv("AUXILIARY_VIDEO_MODEL", "")
	t.Setenv("AUXILIARY_VISION_MODEL", "google/gemini-flash")
	if got := configuredAuxModel(env, []string{"video", "vision"}, []string{"AUXILIARY_VIDEO_MODEL", "AUXILIARY_VISION_MODEL"}); got != "google/gemini-flash" {
		t.Fatalf("video falls back to the vision env %q", got)
	}
}

func TestVisionHandlerPassesConfiguredModel(t *testing.T) {
	aux := &fakeVisionAux{}
	env := visionEnv(t, aux)
	img := writeSolidPNG(t, filepath.Join(t.TempDir(), "x.png"), 4, 4, red)
	env.Config.Set("auxiliary.vision.model", "qwen3.7-plus")
	result := handleVisionAnalyze(context.Background(), &Call{Name: "vision_analyze", Args: map[string]any{"image_url": img, "question": "test"}, Env: env})
	if decodeResult(t, result)["success"] != true || aux.count() != 1 || aux.calls[0].opts.Model != "qwen3.7-plus" {
		t.Fatalf("result %q calls %d", result, aux.count())
	}
	prompt := aux.calls[0].msgs[0].Content.([]any)[0].(map[string]any)["text"].(string)
	if prompt != "Fully describe and explain everything about this image, then answer the following question:\n\ntest" {
		t.Fatalf("prompt %q", prompt)
	}
}

func TestVisionTemperatureAndTimeoutFromConfig(t *testing.T) {
	aux := &fakeVisionAux{}
	env := visionEnv(t, aux)
	img := writeSolidPNG(t, filepath.Join(t.TempDir(), "x.png"), 4, 4, red)
	env.Config.Set("auxiliary.vision.temperature", 1)
	env.Config.Set("auxiliary.vision.timeout", 77)
	if decodeResult(t, visionAnalyzeTool(context.Background(), env, img, "describe", "test/model", "", nil))["success"] != true {
		t.Fatal("analysis failed")
	}
	o := aux.calls[0].opts
	if *o.Temperature != 1.0 || o.Timeout != 77*time.Second || o.MaxTokens != 0 || o.Task != "vision" || o.Model != "test/model" {
		t.Fatalf("configured options %+v", o)
	}
	env.Config.Set("auxiliary.vision", map[string]any{"max_tokens": 8000})
	visionAnalyzeTool(context.Background(), env, img, "describe", "", "", nil)
	o = aux.calls[1].opts
	if *o.Temperature != 0.1 || o.Timeout != 120*time.Second || o.MaxTokens != 0 || o.Model != "" {
		t.Fatalf("default options %+v", o)
	}
	floor := 180.0
	env.Config.Set("auxiliary.vision.timeout", 30)
	if got := auxCallOptions(env, "", 180, &floor); got.Timeout != 180*time.Second {
		t.Fatalf("video floor %v", got.Timeout)
	}
	env.Config.Set("auxiliary.vision.timeout", "bad")
	env.Config.Set("auxiliary.vision.temperature", 0.7)
	if got := auxCallOptions(env, "", 120, nil); got.Timeout != 120*time.Second || *got.Temperature != 0.1 {
		t.Fatalf("a bad timeout keeps every default %+v", got)
	}
}

func TestVisionSafetyGuards(t *testing.T) {
	t.Run("local non image file rejected before the LLM call", func(t *testing.T) {
		aux := &fakeVisionAux{}
		env := visionEnv(t, aux)
		secret := filepath.Join(t.TempDir(), "secret.txt")
		os.WriteFile(secret, []byte("TOP-SECRET=1\n"), 0o600)
		r := decodeResult(t, visionAnalyzeTool(context.Background(), env, secret, "extract text", "", "", nil))
		if r["success"] != false || !strings.Contains(r["error"].(string), "not a recognized image") || aux.count() != 0 {
			t.Fatalf("%v calls %d", r, aux.count())
		}
	})
	t.Run("env file blocked by the read guard on both paths", func(t *testing.T) {
		aux := &fakeVisionAux{}
		env := visionEnv(t, aux)
		secret := filepath.Join(t.TempDir(), ".env")
		os.WriteFile(secret, []byte("OPENAI_API_KEY=sk-super-secret\n"), 0o600)
		r := decodeResult(t, visionAnalyzeTool(context.Background(), env, secret, "extract text", "", "", nil))
		if r["success"] != false || !strings.Contains(r["error"].(string), "secret-bearing environment file") || aux.count() != 0 {
			t.Fatalf("aux path %v", r)
		}
		r = decodeResult(t, visionAnalyzeNative(context.Background(), env, "s", secret, "extract text", "", nil))
		if r["success"] != false || !strings.Contains(r["error"].(string), "secret-bearing environment file") {
			t.Fatalf("native path %v", r)
		}
	})
	t.Run("blocked remote URL short circuits before download", func(t *testing.T) {
		loopbackHosts(t)
		var hits atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
		defer srv.Close()
		aux := &fakeVisionAux{}
		env := visionEnv(t, aux)
		writeBlocklist(t, env, "blocked.test")
		r := decodeResult(t, visionAnalyzeTool(context.Background(), env, hostURL(srv, "blocked.test", "/cat.png"), "describe", "", "", nil))
		if r["success"] != false || !strings.Contains(r["error"].(string), "Blocked by website policy") || hits.Load() != 0 || aux.count() != 0 {
			t.Fatalf("%v hits %d", r, hits.Load())
		}
	})
}

func TestDownloadMedia(t *testing.T) {
	pngBody := append(append([]byte{}, isrcPNG...), make([]byte, 16)...)
	t.Run("redirected final URL blocked by policy", func(t *testing.T) {
		loopbackHosts(t)
		var srv *httptest.Server
		srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/cat.png" {
				http.Redirect(w, r, hostURL(srv, "blocked.test", "/final.png"), http.StatusFound)
				return
			}
			w.Write(pngBody)
		}))
		defer srv.Close()
		env := isrcEnv(t, "local")
		writeBlocklist(t, env, "blocked.test")
		dest := filepath.Join(t.TempDir(), "cat.png")
		err := downloadImage(context.Background(), env, hostURL(srv, "allowed.test", "/cat.png"), dest, 1)
		var policy *mediaPolicyError
		if !errors.As(err, &policy) || !strings.Contains(err.Error(), "Blocked by website policy") {
			t.Fatalf("expected policy block, got %v", err)
		}
		if _, err := os.Stat(dest); !os.IsNotExist(err) {
			t.Fatal("destination written")
		}
	})
	t.Run("size cap enforced while streaming", func(t *testing.T) {
		loopbackHosts(t)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte("12345"))
			w.(http.Flusher).Flush()
			w.Write([]byte("678901"))
		}))
		defer srv.Close()
		env := isrcEnv(t, "local")
		dir := t.TempDir()
		dest := filepath.Join(dir, "big.png")
		err := downloadMedia(context.Background(), env, hostURL(srv, "example.test", "/big.png"), dest, 1,
			mediaDownloadOptions{label: "Image", accept: "image/*", maxBytes: 10, timeout: 5 * time.Second})
		if err == nil || !strings.Contains(err.Error(), "Image too large") {
			t.Fatalf("expected size refusal, got %v", err)
		}
		left, _ := filepath.Glob(filepath.Join(dir, ".big.png.*.tmp"))
		if _, serr := os.Stat(dest); !os.IsNotExist(serr) || len(left) != 0 {
			t.Fatal("partial download left behind")
		}
	})
	t.Run("declared length over the cap refused early", func(t *testing.T) {
		loopbackHosts(t)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Length", "100")
			w.Write(make([]byte, 100))
		}))
		defer srv.Close()
		env := isrcEnv(t, "local")
		err := downloadMedia(context.Background(), env, hostURL(srv, "example.test", "/x.png"), filepath.Join(t.TempDir(), "x.png"), 1,
			mediaDownloadOptions{label: "Image", maxBytes: 10, timeout: 5 * time.Second})
		if err == nil || !strings.Contains(err.Error(), "Image too large (100 bytes, max 10)") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("stream without a length is saved whole", func(t *testing.T) {
		loopbackHosts(t)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("User-Agent") != visionDownloadUserAgent || r.Header.Get("Accept") != "image/*,*/*;q=0.8" {
				w.WriteHeader(400)
				return
			}
			w.Write(pngBody[:4])
			w.(http.Flusher).Flush()
			w.Write(pngBody[4:])
		}))
		defer srv.Close()
		env := isrcEnv(t, "local")
		dest := filepath.Join(t.TempDir(), "cat.png")
		if err := downloadImage(context.Background(), env, hostURL(srv, "example.test", "/cat.png"), dest, 1); err != nil {
			t.Fatal(err)
		}
		if b, _ := os.ReadFile(dest); string(b) != string(pngBody) {
			t.Fatal("body mismatch")
		}
	})
	t.Run("503 retries then raises", func(t *testing.T) {
		loopbackHosts(t)
		var hits atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			w.WriteHeader(503)
		}))
		defer srv.Close()
		var sleeps []time.Duration
		prev := mediaRetrySleep
		mediaRetrySleep = func(ctx context.Context, d time.Duration) error { sleeps = append(sleeps, d); return nil }
		t.Cleanup(func() { mediaRetrySleep = prev })
		env := isrcEnv(t, "local")
		err := downloadImage(context.Background(), env, hostURL(srv, "example.test", "/flaky.jpg"), filepath.Join(t.TempDir(), "y.jpg"), 3)
		var status *mediaHTTPStatusError
		if !errors.As(err, &status) || status.status != 503 || hits.Load() != 3 || len(sleeps) != 2 || sleeps[0] != 2*time.Second || sleeps[1] != 4*time.Second {
			t.Fatalf("err %v hits %d sleeps %v", err, hits.Load(), sleeps)
		}
		if !strings.HasPrefix(err.Error(), "Server error '503 Service Unavailable' for url") {
			t.Fatalf("status text %q", err.Error())
		}
	})
	t.Run("404 fails fast", func(t *testing.T) {
		loopbackHosts(t)
		var hits atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			w.WriteHeader(404)
		}))
		defer srv.Close()
		env := isrcEnv(t, "local")
		if err := downloadImage(context.Background(), env, hostURL(srv, "example.test", "/gone.jpg"), filepath.Join(t.TempDir(), "g.jpg"), 3); err == nil || hits.Load() != 1 {
			t.Fatalf("err %v hits %d", err, hits.Load())
		}
	})
	t.Run("video retries every failure class", func(t *testing.T) {
		loopbackHosts(t)
		var hits atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			w.WriteHeader(404)
		}))
		defer srv.Close()
		prev := mediaRetrySleep
		mediaRetrySleep = func(ctx context.Context, d time.Duration) error { return nil }
		t.Cleanup(func() { mediaRetrySleep = prev })
		env := isrcEnv(t, "local")
		err := downloadMedia(context.Background(), env, hostURL(srv, "example.test", "/v.mp4"), filepath.Join(t.TempDir(), "v.mp4"), 3,
			mediaDownloadOptions{label: "Video", maxBytes: 1 << 20, timeout: 5 * time.Second, retryAll: true})
		if err == nil || hits.Load() != 3 {
			t.Fatalf("err %v hits %d", err, hits.Load())
		}
	})
}

func TestIsRetryableDownloadError(t *testing.T) {
	for _, code := range []int{400, 403, 404, 410} {
		if isRetryableDownloadError(&mediaHTTPStatusError{status: code}) {
			t.Errorf("%d retried", code)
		}
	}
	for _, code := range []int{429, 500, 502, 503} {
		if !isRetryableDownloadError(&mediaHTTPStatusError{status: code}) {
			t.Errorf("%d not retried", code)
		}
	}
	if isRetryableDownloadError(&mediaPolicyError{msg: "blocked"}) || isRetryableDownloadError(&mediaValueError{msg: "too large"}) {
		t.Error("policy and size errors are terminal")
	}
	if !isRetryableDownloadError(errors.New("connection reset")) {
		t.Error("network blip not retried")
	}
}

func TestIsImageSizeError(t *testing.T) {
	for _, msg := range []string{"Request payload too large", "HTTP 413 Payload Too Large", "invalid_request_error: image too big", "Image exceeds maximum size"} {
		if !isImageSizeError(errors.New(msg)) {
			t.Errorf("%q not a size error", msg)
		}
	}
	for _, msg := range []string{"Connection refused", "401 Unauthorized", ""} {
		if isImageSizeError(errors.New(msg)) {
			t.Errorf("%q flagged", msg)
		}
	}
}

func TestVisionLocalPathForms(t *testing.T) {
	aux := &fakeVisionAux{reply: func(int, []*state.Message, AuxOptions) (string, error) { return "A test image", nil }}
	env := visionEnv(t, aux)
	fakeHome := filepath.Join(t.TempDir(), "fakehome")
	os.MkdirAll(fakeHome, 0o755)
	writeSolidPNG(t, filepath.Join(fakeHome, "test_image.png"), 4, 4, red)
	t.Setenv("HOME", fakeHome)
	r := decodeResult(t, visionAnalyzeTool(context.Background(), env, "~/test_image.png", "describe this", "test/model", "", nil))
	if r["success"] != true || r["analysis"] != "A test image" {
		t.Fatalf("tilde path %v", r)
	}
	if decodeResult(t, visionAnalyzeTool(context.Background(), env, "~/nonexistent.png", "describe this", "test/model", "", nil))["success"] != false {
		t.Fatal("missing tilde path")
	}
	img := writeSolidPNG(t, filepath.Join(t.TempDir(), "photo.png"), 4, 4, red)
	if decodeResult(t, visionAnalyzeTool(context.Background(), env, "file://"+img, "describe this", "test/model", "", nil))["success"] != true {
		t.Fatal("file URI")
	}
	if decodeResult(t, visionAnalyzeTool(context.Background(), env, "file://"+filepath.Join(t.TempDir(), "nonexistent.png"), "describe this", "test/model", "", nil))["success"] != false {
		t.Fatal("missing file URI")
	}
	entries, _ := os.ReadDir(filepath.Join(env.Home, "cache", "vision"))
	if len(entries) != 0 {
		t.Fatalf("temp images left behind: %d", len(entries))
	}
}

func TestVisionBase64SizeLimit(t *testing.T) {
	aux := &fakeVisionAux{}
	env := visionEnv(t, aux)
	dir := t.TempDir()
	huge := filepath.Join(dir, "huge.jpg")
	os.WriteFile(huge, append([]byte{0xff, 0xd8, 0xff}, make([]byte, 16*1024*1024)...), 0o644)
	r := decodeResult(t, visionAnalyzeTool(context.Background(), env, huge, "describe this", "", "", nil))
	if r["success"] != false || !strings.Contains(strings.ToLower(r["error"].(string)), "too large") || aux.count() != 0 {
		t.Fatalf("oversized %v calls %d", r["error"], aux.count())
	}
	small := writeSolidPNG(t, filepath.Join(dir, "small.png"), 8, 8, red)
	if decodeResult(t, visionAnalyzeTool(context.Background(), env, small, "describe this", "test/model", "", nil))["success"] != true {
		t.Fatal("small image rejected")
	}
}

func TestVisionErrorClassification(t *testing.T) {
	aux := &fakeVisionAux{reply: func(int, []*state.Message, AuxOptions) (string, error) {
		return "", errors.New("Error code: 400 - {'type': 'error', 'error': {'type': 'invalid_request_error', 'message': 'Invalid request data'}}")
	}}
	env := visionEnv(t, aux)
	img := writeSolidPNG(t, filepath.Join(t.TempDir(), "test.png"), 4, 4, red)
	r := decodeResult(t, visionAnalyzeTool(context.Background(), env, img, "describe", "test/model", "", nil))
	a := strings.ToLower(r["analysis"].(string))
	if r["success"] != false || !strings.Contains(a, "rejected the image") || !strings.Contains(a, "smaller") {
		t.Fatalf("%v", r)
	}
	aux.reply = func(int, []*state.Message, AuxOptions) (string, error) { return "", errors.New("402 payment required") }
	r = decodeResult(t, visionAnalyzeTool(context.Background(), env, img, "describe", "test/model", "", nil))
	if !strings.HasPrefix(r["analysis"].(string), "Insufficient credits") || r["error"] != "Error analyzing image: 402 payment required" {
		t.Fatalf("billing %v", r)
	}
	aux.reply = func(int, []*state.Message, AuxOptions) (string, error) {
		return "", errors.New("model does not support image input")
	}
	r = decodeResult(t, visionAnalyzeTool(context.Background(), env, img, "describe", "", "", nil))
	if !strings.HasPrefix(r["analysis"].(string), "None does not support vision") {
		t.Fatalf("capability %v", r)
	}
}

func TestVisionEmptyAnswerRetriedOnce(t *testing.T) {
	aux := &fakeVisionAux{reply: func(n int, _ []*state.Message, _ AuxOptions) (string, error) {
		if n == 1 {
			return "", nil
		}
		return "second answer", nil
	}}
	env := visionEnv(t, aux)
	img := writeSolidPNG(t, filepath.Join(t.TempDir(), "test.png"), 4, 4, red)
	r := decodeResult(t, visionAnalyzeTool(context.Background(), env, img, "describe", "", "", nil))
	if r["analysis"] != "second answer" || aux.count() != 2 {
		t.Fatalf("%v calls %d", r, aux.count())
	}
	aux.reply = func(int, []*state.Message, AuxOptions) (string, error) { return "", nil }
	r = decodeResult(t, visionAnalyzeTool(context.Background(), env, img, "describe", "", "", nil))
	if r["success"] != true || r["analysis"] != "There was a problem with the request and the image could not be analyzed." {
		t.Fatalf("empty twice %v", r)
	}
}

func TestVisionSizeRejectionResizesAndRetries(t *testing.T) {
	var lengths []int
	aux := &fakeVisionAux{}
	aux.reply = func(n int, msgs []*state.Message, _ AuxOptions) (string, error) {
		lengths = append(lengths, len(auxMediaURL(t, auxCall{msgs: msgs}, "image_url")))
		if n == 1 {
			return "", errors.New("HTTP 413 Payload Too Large")
		}
		return "fits now", nil
	}
	env := visionEnv(t, aux)
	big := writeNoisePNG(t, filepath.Join(t.TempDir(), "big.png"), 1500, 1500, 3)
	r := decodeResult(t, visionAnalyzeTool(context.Background(), env, big, "describe", "", "", nil))
	if r["success"] != true || aux.count() != 2 || len(lengths) != 2 {
		t.Fatalf("%v calls %d", r, aux.count())
	}
	if lengths[0] <= visionResizeTargetBytes || lengths[1] > visionResizeTargetBytes {
		t.Fatalf("first %d second %d", lengths[0], lengths[1])
	}
	if !strings.Contains(r["scale_note"].(string), "downscaled from 1500x1500") {
		t.Fatalf("scale note %v", r["scale_note"])
	}
	small := writeSolidPNG(t, filepath.Join(t.TempDir(), "small.png"), 8, 8, red)
	lengths = nil
	aux.reply = func(int, []*state.Message, AuxOptions) (string, error) {
		return "", errors.New("HTTP 413 Payload Too Large")
	}
	r = decodeResult(t, visionAnalyzeTool(context.Background(), env, small, "describe", "", "", nil))
	if r["success"] != false || aux.count() != 3 {
		t.Fatalf("a small payload size error is not retried: %v calls %d", r, aux.count())
	}
}

func TestResizeImageForVision(t *testing.T) {
	env := isrcEnv(t, "local")
	dir := t.TempDir()
	small := writeSolidPNG(t, filepath.Join(dir, "small.png"), 10, 10, red)
	if u, err := resizeImageForVision(env, small, "image/png", ResizeOptions{}); err != nil || !strings.HasPrefix(u, "data:image/png;base64,") {
		t.Fatalf("small %v", err)
	}
	if u, _ := resizeImageForVision(env, small, "image/png", ResizeOptions{MaxBase64Bytes: 256 * 1024, MaxDimension: 1568, ForceJPEG: true}); !strings.HasPrefix(u, "data:image/png;base64,") {
		t.Fatal("force_jpeg must leave an under cap PNG untouched")
	}
	junk := filepath.Join(dir, "junk.png")
	os.WriteFile(junk, append(append([]byte{}, isrcPNG[:20]...), make([]byte, 1000)...), 0o644)
	if u, _ := resizeImageForVision(env, junk, "image/png", ResizeOptions{MaxBase64Bytes: 100}); len(u) <= 100 {
		t.Fatal("undecodable image must come back as the raw payload")
	}
	rnd := rand.New(rand.NewSource(42))
	img := image.NewRGBA(image.Rect(0, 0, 1568, 900))
	for y := 0; y < 900; y++ {
		for x := 0; x < 1568; x++ {
			img.SetRGBA(x, y, color.RGBA{uint8(min(255, x*255/1568+rnd.Intn(12))), uint8(min(255, y*255/900+rnd.Intn(12))), uint8(min(255, (x+y)*255/2468+rnd.Intn(12))), 255})
		}
	}
	dense := filepath.Join(dir, "dense.png")
	f, _ := os.Create(dense)
	png.Encode(f, img)
	f.Close()
	scale := &ScaleInfo{}
	u, err := resizeImageForVision(env, dense, "image/png", ResizeOptions{MaxBase64Bytes: 256 * 1024, MaxDimension: 1568, ForceJPEG: true, Scale: scale})
	if err != nil || !strings.HasPrefix(u, "data:image/jpeg;base64,") || len(u) > 256*1024 {
		t.Fatalf("dense %v len %d", err, len(u))
	}
	if w, h := dataURLSize(t, u); max(w, h) != 1568 || scale.Applied() {
		t.Fatalf("resolution not kept %dx%d", w, h)
	}
	for _, mode := range []string{"RGBA", "LA", "P"} {
		p := filepath.Join(dir, "img_"+mode+".png")
		var src image.Image
		switch mode {
		case "RGBA":
			m := image.NewNRGBA(image.Rect(0, 0, 600, 400))
			rnd.Read(m.Pix)
			src = m
		case "LA":
			m := image.NewNRGBA(image.Rect(0, 0, 600, 400))
			for i := 0; i < len(m.Pix); i += 4 {
				v := uint8(rnd.Intn(256))
				m.Pix[i], m.Pix[i+1], m.Pix[i+2], m.Pix[i+3] = v, v, v, uint8(rnd.Intn(256))
			}
			src = m
		case "P":
			pal := make(color.Palette, 256)
			for i := range pal {
				pal[i] = color.RGBA{uint8(i), uint8(255 - i), uint8(i * 7), 255}
			}
			m := image.NewPaletted(image.Rect(0, 0, 600, 400), pal)
			rnd.Read(m.Pix)
			src = m
		}
		f, _ := os.Create(p)
		png.Encode(f, src)
		f.Close()
		u, err := resizeImageForVision(env, p, "image/png", ResizeOptions{MaxBase64Bytes: 16 * 1024, MaxDimension: 1568, ForceJPEG: true})
		if err != nil || !strings.HasPrefix(u, "data:image/jpeg;base64,") {
			t.Fatalf("%s: %v %q", mode, err, u[:min(len(u), 40)])
		}
	}
}

func TestBuildScaleNote(t *testing.T) {
	if BuildScaleNote(nil, nil) != "" || BuildScaleNote(&ScaleInfo{}, map[string]int{}) != "" {
		t.Fatal("note without changes")
	}
	note := BuildScaleNote(&ScaleInfo{OrigWidth: 3024, OrigHeight: 1964, NewWidth: 1512, NewHeight: 982, set: true}, nil)
	if !strings.Contains(note, "3024x1964") || !strings.Contains(note, "1512x982") || !strings.Contains(note, "any coordinates you report by 2.00") {
		t.Fatalf("scale note %q", note)
	}
	note = BuildScaleNote(&ScaleInfo{OrigWidth: 3000, OrigHeight: 1000, NewWidth: 1000, NewHeight: 500, set: true}, nil)
	if !strings.Contains(note, "any x coordinates you report by 3.00 and any y coordinates by 2.00") {
		t.Fatalf("per axis note %q", note)
	}
	note = BuildScaleNote(nil, map[string]int{"x": 300, "y": 200, "width": 10, "height": 10})
	if !strings.Contains(note, "(300, 200)") || !strings.Contains(strings.ToLower(note), "crop") {
		t.Fatalf("crop note %q", note)
	}
}

func TestVisionScaleDisclosure(t *testing.T) {
	aux := &fakeVisionAux{}
	env := visionEnv(t, aux)
	dir := t.TempDir()
	big := writeNoisePNG(t, filepath.Join(dir, "big_noise.png"), 3024, 1964, 11)
	r := decodeResult(t, visionAnalyzeTool(context.Background(), env, big, "describe", "test/model", "", nil))
	note, _ := r["scale_note"].(string)
	m := regexp.MustCompile(`downscaled from (\d+)x(\d+) to (\d+)x(\d+)`).FindStringSubmatch(note)
	if r["success"] != true || m == nil || m[1] != "3024" || m[2] != "1964" || !strings.HasPrefix(r["analysis"].(string), "["+note+"]") {
		t.Fatalf("downscale note %v", r)
	}
	var nw, nh float64
	fmt.Sscan(m[3], &nw)
	fmt.Sscan(m[4], &nh)
	if !strings.Contains(note, fmt.Sprintf("%.2f", 3024/nw)) {
		t.Fatalf("factor missing %q", note)
	}
	small := writeSolidPNG(t, filepath.Join(dir, "small.png"), 64, 64, red)
	r = decodeResult(t, visionAnalyzeTool(context.Background(), env, small, "describe", "test/model", "", nil))
	if _, has := r["scale_note"]; has || strings.HasPrefix(r["analysis"].(string), "[") {
		t.Fatalf("small image disclosed %v", r)
	}
	prev := maxBase64Limit
	maxBase64Limit = 8 * 1024 * 1024
	t.Cleanup(func() { maxBase64Limit = prev })
	r = decodeResult(t, visionAnalyzeTool(context.Background(), env, big, "describe", "test/model", "", []any{300.0, 100.0, 2700.0, 1900.0}))
	note, _ = r["scale_note"].(string)
	m = regexp.MustCompile(`downscaled from (\d+)x(\d+) to (\d+)x(\d+)`).FindStringSubmatch(note)
	if r["success"] != true || m == nil || m[1] != "2400" || m[2] != "1800" || !strings.Contains(note, "(300, 100)") || !strings.Contains(note, "relative") {
		t.Fatalf("region plus downscale %v", r)
	}
	r = decodeResult(t, visionAnalyzeTool(context.Background(), env, big, "describe", "test/model", "", []any{100.0, 50.0, 400.0, 300.0}))
	note, _ = r["scale_note"].(string)
	if !strings.Contains(note, "(100, 50)") || strings.Contains(note, "downscaled") {
		t.Fatalf("region only %q", note)
	}
}

func TestBuildNativeVisionToolResult(t *testing.T) {
	m := decodeResult(t, buildNativeVisionToolResult("/tmp/foo.png", "what does it say?", "data:image/png;base64,XYZ", 1024, ""))
	parts := m["content"].([]any)
	if m["_multimodal"] != true || len(parts) != 2 || parts[0].(map[string]any)["type"] != "text" || parts[1].(map[string]any)["type"] != "image_url" {
		t.Fatalf("envelope %v", m)
	}
	if parts[1].(map[string]any)["image_url"].(map[string]any)["url"] != "data:image/png;base64,XYZ" || !strings.Contains(parts[0].(map[string]any)["text"].(string), "what does it say?") {
		t.Fatalf("parts %v", parts)
	}
	if m["text_summary"] != "Image attached natively for the main model (1.0 KB). Answer using built-in vision." {
		t.Fatalf("summary %v", m["text_summary"])
	}
	meta := m["meta"].(map[string]any)
	if meta["image_url"] != "/tmp/foo.png" || meta["size_bytes"] != 1024.0 || meta["native_vision"] != true {
		t.Fatalf("meta %v", meta)
	}
}

func TestVisionAnalyzeNative(t *testing.T) {
	env := visionEnv(t, nil)
	dir := t.TempDir()
	img := filepath.Join(dir, "test.png")
	os.WriteFile(img, isrcPNG, 0o644)
	result := visionAnalyzeNative(context.Background(), env, "s", img, "what is this?", "", nil)
	if !embeddedResult(t, result) || !strings.HasPrefix(envelopeURL(t, result), "data:image/") {
		t.Fatalf("local file %q", result)
	}
	if !embeddedResult(t, visionAnalyzeNative(context.Background(), env, "s", "file://"+img, "?", "", nil)) {
		t.Fatal("file URI")
	}
	truncated := filepath.Join(dir, "truncated.png")
	os.WriteFile(truncated, isrcPNG[:len(isrcPNG)-22], 0o644)
	r := decodeResult(t, visionAnalyzeNative(context.Background(), env, "s", truncated, "describe", "", nil))
	if e := strings.ToLower(r["error"].(string)); r["success"] != false || !(strings.Contains(e, "decode") || strings.Contains(e, "not a recognized image")) {
		t.Fatalf("truncated %v", r)
	}
	bad := filepath.Join(dir, "bad.png")
	os.WriteFile(bad, isrcCorruptPNG, 0o644)
	r = decodeResult(t, visionAnalyzeNative(context.Background(), env, "s", bad, "what is this?", "", nil))
	if r["success"] != false || !strings.Contains(r["error"].(string), "recognized image") {
		t.Fatalf("corrupt %v", r)
	}
	gifPath := filepath.Join(dir, "truncated.gif")
	two := animatedGIF(t, red, blue)
	os.WriteFile(gifPath, two[:len(two)-3], 0o644)
	r = decodeResult(t, visionAnalyzeNative(context.Background(), env, "s", gifPath, "describe", "", nil))
	if r["success"] != false || !strings.Contains(strings.ToLower(r["error"].(string)), "decode") {
		t.Fatalf("truncated animation %v", r)
	}
	r = decodeResult(t, visionAnalyzeNative(context.Background(), env, "s", 12345.0, "q", "", nil))
	if r["error"] != "image_url is required" {
		t.Fatalf("non string url %v", r)
	}
	if defaultEmbedTargetBytes > 512*1024 || VisionEmbedMaxDimension > 2048 {
		t.Fatal("embed caps must stay sized for history reuse")
	}
}

func TestVisionNativeOversizedResizedUnderEmbedCap(t *testing.T) {
	env := visionEnv(t, nil)
	big := writeNoisePNG(t, filepath.Join(t.TempDir(), "big.png"), 1600, 1600, 5)
	result := visionAnalyzeNative(context.Background(), env, "s", big, "describe", "", nil)
	u := envelopeURL(t, result)
	if len(u) > defaultEmbedTargetBytes || !strings.HasPrefix(u, "data:image/jpeg;base64,") {
		t.Fatalf("embed %d bytes", len(u))
	}
	text := decodeResult(t, result)["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "Note: Image downscaled from 1600x1600") {
		t.Fatalf("scale disclosure missing: %q", text)
	}
}

func TestVisionNativeRegion(t *testing.T) {
	env := visionEnv(t, nil)
	dir := t.TempDir()
	src := writeSolidPNG(t, filepath.Join(dir, "img.png"), 100, 50, color.RGBA{200, 30, 30, 255})
	result := visionAnalyzeNative(context.Background(), env, "s", src, "zoom", "", []any{10.0, 10.0, 60.0, 40.0})
	if w, h := dataURLSize(t, envelopeURL(t, result)); w != 50 || h != 30 {
		t.Fatalf("crop %dx%d", w, h)
	}
	if !strings.Contains(decodeResult(t, result)["content"].([]any)[0].(map[string]any)["text"].(string), "offset (10, 10)") {
		t.Fatal("crop offset not disclosed")
	}
	if w, h := dataURLSize(t, envelopeURL(t, visionAnalyzeNative(context.Background(), env, "s", src, "zoom", "", nil))); w != 100 || h != 50 {
		t.Fatalf("full %dx%d", w, h)
	}
	r := decodeResult(t, visionAnalyzeNative(context.Background(), env, "s", src, "zoom", "", []any{200.0, 200.0, 300.0, 300.0}))
	if msg := r["error"].(string); r["success"] != false || !strings.Contains(msg, "100") || !strings.Contains(msg, "50") {
		t.Fatalf("zero area %v", r)
	}
	tall := writeSolidPNG(t, filepath.Join(dir, "big.png"), 200, VisionEmbedMaxDimension+500, color.RGBA{0, 100, 0, 255})
	if w, h := dataURLSize(t, envelopeURL(t, visionAnalyzeNative(context.Background(), env, "s", tall, "zoom", "", []any{0.0, 0.0, 200.0, 300.0}))); w != 200 || h != 300 {
		t.Fatalf("crop before downscale %dx%d", w, h)
	}
}

func TestVisionHandlerGating(t *testing.T) {
	caps := installVisionCaps(t, standardCaps())
	caps.catalog["openrouter|anthropic/claude-opus-4.6"] = true
	aux := &fakeVisionAux{reply: func(int, []*state.Message, AuxOptions) (string, error) { return "aux-path", nil }}
	env := visionEnv(t, aux)
	img := writeSolidPNG(t, filepath.Join(t.TempDir(), "x.png"), 50, 50, red)
	env.Values.Store("runtime_main", map[string]any{"provider": "openrouter", "model": "anthropic/claude-opus-4.6"})
	result := handleVisionAnalyze(context.Background(), &Call{Args: map[string]any{"image_url": img, "question": "?", "region": []any{1.0, 2.0, 30.0, 40.0}}, Env: env})
	if !embeddedResult(t, result) || aux.count() != 0 {
		t.Fatalf("fast path %q", result)
	}
	if w, h := dataURLSize(t, envelopeURL(t, result)); w != 29 || h != 38 {
		t.Fatalf("region not passed to the native path: %dx%d", w, h)
	}
	env.Values.Store("runtime_main", map[string]any{"provider": "brand-new-provider", "model": "anthropic/claude-opus-4.6"})
	result = handleVisionAnalyze(context.Background(), &Call{Args: map[string]any{"image_url": img, "question": "?"}, Env: env})
	if embeddedResult(t, result) || decodeResult(t, result)["analysis"] != "aux-path" {
		t.Fatalf("unknown provider must fall through %q", result)
	}
}

func TestVisionRequirements(t *testing.T) {
	installVisionCaps(t, standardCaps())
	env := isrcEnv(t, "local")
	if checkVisionRequirements(env) || checkVideoRequirements(env) {
		t.Fatal("no aux and no native route")
	}
	env.Aux = &fakeVisionAux{}
	func() {
		defer func() {
			if rec := recover(); rec == nil || !strings.Contains(fmt.Sprint(rec), "AuxTaskResolver") {
				t.Fatalf("an unprobeable aux client must not read as available: %v", rec)
			}
		}()
		checkVideoRequirements(env)
	}()
	r := &resolvingAux{resolves: map[string]bool{"auto": true}}
	env.Aux = r
	if !checkVideoRequirements(env) || strings.Join(r.asked, ",") != "vision|,vision|auto" {
		t.Fatalf("auto chain %v", r.asked)
	}
	r.resolves = map[string]bool{}
	if checkVideoRequirements(env) {
		t.Fatal("nothing resolves")
	}
	env.Values.Store("runtime_main", map[string]any{"provider": "anthropic", "model": "claude"})
	VisionCapabilities.(*fakeVisionCaps).catalog["anthropic|claude"] = true
	if !checkVisionRequirements(env) {
		t.Fatal("native route keeps vision_analyze available")
	}
	r.err = errors.New("named custom provider lookup failed")
	defer func() {
		if rec := recover(); rec == nil || !strings.Contains(fmt.Sprint(rec), "lookup failed") {
			t.Fatalf("resolver crash swallowed: %v", rec)
		}
	}()
	checkVideoRequirements(env)
}

func TestVisionCPUBurstCap(t *testing.T) {
	prevCPUs := detectHostCPUs
	t.Cleanup(func() { detectHostCPUs = prevCPUs })
	env := isrcEnv(t, "local")
	for _, c := range []struct {
		cpus int
		env  string
		want int
	}{{64, "", 64}, {2, "", 2}, {2, "16", 16}, {2, "0", 2}} {
		detectHostCPUs = func() int { return c.cpus }
		t.Setenv("ICEBATH_VISION_MAX_CONCURRENCY", c.env)
		if got := resolveVisionCPUWorkers(env); got != c.want {
			t.Errorf("cpus %d env %q: got %d", c.cpus, c.env, got)
		}
	}
	visionCPU.once.Do(func() {})
	prevSem := visionCPU.sem
	visionCPU.sem = make(chan struct{}, 3)
	t.Cleanup(func() { visionCPU.sem = prevSem })
	var inflight, peak, callsInflight, callsPeak atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := callsInflight.Add(1)
			for {
				p := callsPeak.Load()
				if c <= p || callsPeak.CompareAndSwap(p, c) {
					break
				}
			}
			RunOnVisionCPU(env, func() bool {
				n := inflight.Add(1)
				for {
					p := peak.Load()
					if n <= p || peak.CompareAndSwap(p, n) {
						break
					}
				}
				time.Sleep(40 * time.Millisecond)
				inflight.Add(-1)
				return true
			})
			time.Sleep(20 * time.Millisecond)
			callsInflight.Add(-1)
		}()
	}
	wg.Wait()
	if peak.Load() != 3 || callsPeak.Load() <= 3 {
		t.Fatalf("encode peak %d call peak %d", peak.Load(), callsPeak.Load())
	}
}

func TestVideoAnalyze(t *testing.T) {
	for in, want := range map[string]string{"clip.mp4": "video/mp4", "clip.webm": "video/webm", "CLIP.MP4": "video/mp4", "a.mkv": "video/mp4", "a.flv": ""} {
		if got := detectVideoMimeType(in); got != want {
			t.Errorf("%s: %q", in, got)
		}
	}
	if unsupportedVideoFormat(".flv") != "Unsupported video format: '.flv'. Supported: .avi, .mkv, .mov, .mp4, .mpeg, .mpg, .webm" {
		t.Fatal(unsupportedVideoFormat(".flv"))
	}
	t.Run("local file success with the video_url message shape", func(t *testing.T) {
		aux := &fakeVisionAux{reply: func(int, []*state.Message, AuxOptions) (string, error) { return "A short video showing a demo.", nil }}
		env := visionEnv(t, aux)
		video := filepath.Join(t.TempDir(), "demo.mp4")
		os.WriteFile(video, make([]byte, 1024), 0o644)
		r := decodeResult(t, videoAnalyzeTool(context.Background(), env, video, "What is this?", "", ""))
		if r["success"] != true || !strings.Contains(strings.ToLower(r["analysis"].(string)), "demo") {
			t.Fatalf("%v", r)
		}
		parts := aux.calls[0].msgs[0].Content.([]any)
		if len(aux.calls[0].msgs) != 1 || len(parts) != 2 || parts[0].(map[string]any)["type"] != "text" || parts[1].(map[string]any)["type"] != "video_url" {
			t.Fatalf("message shape %v", parts)
		}
		if !strings.HasPrefix(auxMediaURL(t, aux.calls[0], "video_url"), "data:video/mp4;base64,") || aux.calls[0].opts.MaxTokens != 0 || aux.calls[0].opts.Timeout != 180*time.Second {
			t.Fatalf("options %+v", aux.calls[0].opts)
		}
	})
	t.Run("env file disguised as a video is blocked", func(t *testing.T) {
		aux := &fakeVisionAux{}
		env := visionEnv(t, aux)
		dir := t.TempDir()
		secret := filepath.Join(dir, ".env")
		os.WriteFile(secret, []byte("OPENAI_API_KEY=sk-super-secret\n"), 0o600)
		disguised := filepath.Join(dir, "video.mp4")
		if err := os.Symlink(secret, disguised); err != nil {
			t.Skip("symlinks unsupported")
		}
		r := decodeResult(t, videoAnalyzeTool(context.Background(), env, disguised, "What is this?", "", ""))
		if r["success"] != false || !strings.Contains(r["error"].(string), "secret-bearing environment file") || aux.count() != 0 {
			t.Fatalf("%v", r)
		}
	})
	t.Run("unsupported format", func(t *testing.T) {
		env := visionEnv(t, &fakeVisionAux{})
		clip := filepath.Join(t.TempDir(), "clip.flv")
		os.WriteFile(clip, make([]byte, 100), 0o644)
		r := decodeResult(t, videoAnalyzeTool(context.Background(), env, clip, "What is this?", "", ""))
		if r["success"] != false || !strings.Contains(strings.ToLower(r["analysis"].(string)), "unsupported video format") {
			t.Fatalf("%v", r)
		}
	})
	t.Run("non local backend reads the sandbox copy", func(t *testing.T) {
		aux := &fakeVisionAux{reply: func(int, []*state.Message, AuxOptions) (string, error) { return "sandbox video", nil }}
		env := isrcEnv(t, "docker")
		env.Aux = aux
		freshRepeatCounts(t)
		host := filepath.Join(t.TempDir(), "clip.mp4")
		os.WriteFile(host, []byte("HOST-VIDEO"), 0o644)
		remote := []byte("REMOTE-SANDBOX-VIDEO")
		var lookups []string
		stubSandbox(t, nil, func(taskID string) Environment {
			lookups = append(lookups, taskID)
			return &visionFakeSandbox{exec: func(cmd string) (*ExecResult, error) {
				return &ExecResult{Output: base64.StdEncoding.EncodeToString(remote)}, nil
			}}
		})
		r := decodeResult(t, videoAnalyzeTool(context.Background(), env, host, "Describe this", "", "task-123"))
		if r["success"] != true || len(lookups) != 1 || lookups[0] != "task-123" {
			t.Fatalf("%v lookups %v", r, lookups)
		}
		up, _ := base64.StdEncoding.DecodeString(strings.SplitN(auxMediaURL(t, aux.calls[0], "video_url"), ",", 2)[1])
		if string(up) != string(remote) {
			t.Fatalf("uploaded %q", up)
		}
		entries, _ := os.ReadDir(filepath.Join(env.Home, "cache", "video"))
		if len(entries) != 0 {
			t.Fatal("terminal video temp file left behind")
		}
	})
}

func TestVisionToolsRegistered(t *testing.T) {
	registerVisionTools()
	v := Default.Get("vision_analyze")
	if v == nil || v.Toolset != "vision" || v.Emoji != "\U0001F441️" || v.Schema["name"] != "vision_analyze" {
		t.Fatalf("vision_analyze %+v", v)
	}
	req := v.Schema["parameters"].(map[string]any)["required"].([]any)
	if len(req) != 2 || req[0] != "image_url" || req[1] != "question" {
		t.Fatalf("required %v", req)
	}
	vid := Default.Get("video_analyze")
	if vid == nil || vid.Toolset != "video" || vid.Emoji != "\U0001F3AC" {
		t.Fatalf("video_analyze %+v", vid)
	}
}
