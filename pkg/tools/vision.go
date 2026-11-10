package tools

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/nickheyer/icebath/pkg/state"
)

// Hard cap on downloaded media, bounding memory and disk against attacker hosted files
const visionMaxDownloadBytes = 50 * 1024 * 1024

// VisionMaxBase64Bytes is the absolute ceiling for one vision payload, no major provider accepts more
const VisionMaxBase64Bytes = 20 * 1024 * 1024

// Payload ceiling the vision paths enforce, replaced in tests
var maxBase64Limit = VisionMaxBase64Bytes

// VisionEmbedMaxDimension is the long edge cap for history embeds, Anthropic downsamples to it anyway
const VisionEmbedMaxDimension = 1568

// Target when auto resizing after a provider size rejection
const visionResizeTargetBytes = 5 * 1024 * 1024

// Video payload bounds
const (
	maxVideoBase64Bytes = 50 * 1024 * 1024
	videoSizeWarnBytes  = 20 * 1024 * 1024
)

// Browser user agent for media downloads, some CDNs refuse library agents
const visionDownloadUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
	"(KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"

// Error substrings that mark a provider rejection as an image or payload size problem
var visionSizeErrorHints = []string{"too large", "payload", "413", "content_too_large", "request_too_large", "exceeds", "size limit"}

var (
	visionDebugOnce sync.Once
	visionDebug     *DebugSession
)

// Debug session recording vision calls when VISION_TOOLS_DEBUG is true
func visionDebugSession(env *Env) *DebugSession {
	visionDebugOnce.Do(func() { visionDebug = NewDebugSession(env, "vision_tools", "VISION_TOOLS_DEBUG") })
	return visionDebug
}

// Value of auxiliary.vision.<key>, nil when unset
func cfgAuxVision(env *Env, key string) any {
	return cfgTree(env).Sub("auxiliary").Sub("vision")[key]
}

// Python float of a scalar, bools included
func pyFloatOf(v any) (float64, bool) {
	switch t := v.(type) {
	case bool:
		if t {
			return 1, true
		}
		return 0, true
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	case uint64:
		return float64(t), true
	case float64:
		return t, true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		return f, err == nil
	}
	return 0, false
}

// Vision setting from the env var then auxiliary.vision, skipping values that fail to parse or fall below the minimum
func readVisionFloat(env *Env, envVar, key string, minimum *float64) (float64, bool) {
	accept := func(raw any) (float64, bool) {
		if raw == nil {
			return 0, false
		}
		f, ok := pyFloatOf(raw)
		if !ok || math.IsNaN(f) || (minimum != nil && f < *minimum) {
			return 0, false
		}
		return f, true
	}
	if s := secretValue(env, envVar); s != "" {
		if f, ok := accept(s); ok {
			return f, true
		}
	}
	return accept(cfgAuxVision(env, key))
}

// Integer vision setting from the env var then auxiliary.vision, values below the minimum skipped
func readVisionInt(env *Env, envVar, key string, minimum int) (int, bool) {
	accept := func(raw any) (int, bool) {
		if raw == nil {
			return 0, false
		}
		if _, isBool := raw.(bool); isBool {
			b := raw.(bool)
			n := 0
			if b {
				n = 1
			}
			return n, n >= minimum
		}
		n, ok := pyInt(raw)
		if !ok || n < minimum {
			return 0, false
		}
		return n, true
	}
	if s := secretValue(env, envVar); s != "" {
		if n, ok := accept(s); ok {
			return n, true
		}
	}
	return accept(cfgAuxVision(env, key))
}

// HTTP download timeout for media, separate from the LLM call timeout
func visionDownloadTimeout(env *Env) time.Duration {
	if f, ok := readVisionFloat(env, "ICEBATH_VISION_DOWNLOAD_TIMEOUT", "download_timeout", nil); ok {
		return time.Duration(f * float64(time.Second))
	}
	return 30 * time.Second
}

// Bounded worker slots for the CPU heavy encode, resize, and decode steps
var visionCPU struct {
	once sync.Once
	sem  chan struct{}
}

// Usable CPU count honouring the affinity mask, at least one, replaced in tests
var detectHostCPUs = func() int { return max(1, runtime.NumCPU()) }

// Worker count from ICEBATH_VISION_MAX_CONCURRENCY then auxiliary.vision.max_concurrency then usable cores
func resolveVisionCPUWorkers(env *Env) int {
	if n, ok := readVisionInt(env, "ICEBATH_VISION_MAX_CONCURRENCY", "max_concurrency", 1); ok && n > 0 {
		return n
	}
	return detectHostCPUs()
}

// RunOnVisionCPU runs a CPU heavy vision step inside the bounded vision worker pool
func RunOnVisionCPU[T any](env *Env, fn func() T) T {
	visionCPU.once.Do(func() { visionCPU.sem = make(chan struct{}, resolveVisionCPUWorkers(env)) })
	visionCPU.sem <- struct{}{}
	defer func() { <-visionCPU.sem }()
	return fn()
}

// Reports whether a URL is http or https with a host and passes the SSRF guard
func validateImageURL(env *Env, raw string) bool {
	if !strings.HasPrefix(raw, "http://") && !strings.HasPrefix(raw, "https://") {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return false
	}
	return isSafeURL(env, raw)
}

// Website policy refusal of a media download, never retried
type mediaPolicyError struct{ msg string }

func (e *mediaPolicyError) Error() string { return e.msg }

// Size or redirect refusal of a media download, never retried
type mediaValueError struct{ msg string }

func (e *mediaValueError) Error() string { return e.msg }

// Non success HTTP status of a media download
type mediaHTTPStatusError struct {
	status int
	url    string
}

func (e *mediaHTTPStatusError) Error() string {
	kind := "Client error"
	switch {
	case e.status < 200:
		kind = "Informational response"
	case e.status < 400:
		kind = "Redirect response"
	case e.status >= 500:
		kind = "Server error"
	}
	return fmt.Sprintf("%s '%d %s' for url '%s'\nFor more information check: https://developer.mozilla.org/en-US/docs/Web/HTTP/Status/%d",
		kind, e.status, http.StatusText(e.status), e.url, e.status)
}

// Transient failures only, 429 and 5xx and transport errors, never policy, size, or other 4xx
func isRetryableDownloadError(err error) bool {
	var policy *mediaPolicyError
	var value *mediaValueError
	if errors.As(err, &policy) || errors.As(err, &value) {
		return false
	}
	var status *mediaHTTPStatusError
	if errors.As(err, &status) {
		return status.status == 429 || !(status.status >= 400 && status.status < 500)
	}
	return true
}

// Backoff sleep between download attempts, replaced in tests
var mediaRetrySleep = func(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Media download tuning per media kind
type mediaDownloadOptions struct {
	label    string
	accept   string
	maxBytes int64
	timeout  time.Duration
	retryAll bool
}

// SSRF safe streaming download with exponential backoff, images retrying only transient failures
func downloadMedia(ctx context.Context, env *Env, rawURL, destination string, maxRetries int, o mediaDownloadOptions) error {
	if err := os.MkdirAll(filepath.Dir(destination), 0o777); err != nil {
		return err
	}
	log := envLog(env)
	var lastErr error
	for attempt := 0; attempt < maxRetries; attempt++ {
		err := downloadMediaOnce(ctx, env, rawURL, destination, o)
		if err == nil {
			return nil
		}
		lastErr = err
		if attempt >= maxRetries-1 || (!o.retryAll && !isRetryableDownloadError(err)) {
			log.Error(fmt.Sprintf("%s download failed after %d attempt(s): %s", o.label, attempt+1, clipRunes(err.Error(), 100)))
			if !o.retryAll {
				return err
			}
			break
		}
		wait := time.Duration(1<<(attempt+1)) * time.Second
		log.Warn(fmt.Sprintf("%s download failed (attempt %d/%d): %s", o.label, attempt+1, maxRetries, clipRunes(err.Error(), 50)))
		if !o.retryAll {
			log.Warn(fmt.Sprintf("Retrying in %ds...", int(wait/time.Second)))
		}
		if err := mediaRetrySleep(ctx, wait); err != nil {
			return err
		}
	}
	if lastErr != nil {
		return lastErr
	}
	return fmt.Errorf("_download_%s exited retry loop without attempting (max_retries=%d)", strings.ToLower(o.label), maxRetries)
}

// One download attempt with per operation idle timeouts, a capped stream, and an atomic move into place
func downloadMediaOnce(ctx context.Context, env *Env, rawURL, destination string, o mediaDownloadOptions) error {
	if blocked := checkWebsiteAccess(env, rawURL); blocked != nil {
		return &mediaPolicyError{msg: blocked.Message}
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var timedOut atomic.Bool
	idle := time.AfterFunc(o.timeout, func() {
		timedOut.Store(true)
		cancel()
	})
	defer idle.Stop()
	client := newSSRFSafeClient(env, 0, true, nil)
	base := client.CheckRedirect
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if err := base(req, via); err != nil {
			if strings.HasPrefix(err.Error(), "Blocked redirect") {
				return &mediaValueError{msg: err.Error()}
			}
			return err
		}
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", visionDownloadUserAgent)
	req.Header.Set("Accept", o.accept)
	timeoutErr := func(err error) error {
		if timedOut.Load() {
			return fmt.Errorf("%s download timed out after %.0fs: %w", o.label, o.timeout.Seconds(), err)
		}
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		var value *mediaValueError
		if errors.As(err, &value) {
			return value
		}
		var ssrf *SSRFConnectionBlocked
		if errors.As(err, &ssrf) {
			return &mediaValueError{msg: ssrf.Error()}
		}
		return timeoutErr(err)
	}
	defer resp.Body.Close()
	idle.Reset(o.timeout)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &mediaHTTPStatusError{status: resp.StatusCode, url: resp.Request.URL.String()}
	}
	if cl := resp.Header.Get("Content-Length"); cl != "" {
		if declared, err := strconv.ParseInt(strings.TrimSpace(cl), 10, 64); err == nil && declared > o.maxBytes {
			return &mediaValueError{msg: fmt.Sprintf("%s too large (%d bytes, max %d)", o.label, declared, o.maxBytes)}
		}
	}
	if blocked := checkWebsiteAccess(env, resp.Request.URL.String()); blocked != nil {
		return &mediaPolicyError{msg: blocked.Message}
	}
	tmp := filepath.Join(filepath.Dir(destination), "."+filepath.Base(destination)+"."+strings.ReplaceAll(uuid.NewString(), "-", "")+".tmp")
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	fail := func(err error) error {
		f.Close()
		if rmErr := os.Remove(tmp); rmErr != nil && !os.IsNotExist(rmErr) {
			envLog(env).Debug("Could not delete partial download", "path", tmp, "error", rmErr)
		}
		return err
	}
	var written int64
	buf := make([]byte, 64*1024)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			idle.Reset(o.timeout)
			written += int64(n)
			if written > o.maxBytes {
				return fail(&mediaValueError{msg: fmt.Sprintf("%s too large (%d bytes, max %d)", o.label, written, o.maxBytes)})
			}
			if _, werr := f.Write(buf[:n]); werr != nil {
				return fail(werr)
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return fail(timeoutErr(rerr))
		}
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, destination); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// Downloads an image with SSRF protection and error class aware retry
func downloadImage(ctx context.Context, env *Env, imageURL, destination string, maxRetries int) error {
	return downloadMedia(ctx, env, imageURL, destination, maxRetries, mediaDownloadOptions{
		label: "Image", accept: "image/*,*/*;q=0.8", maxBytes: visionMaxDownloadBytes,
		timeout: visionDownloadTimeout(env), retryAll: false})
}

// Managed system owning this install from ICEBATH_MANAGED or the home marker, empty when unmanaged
func managedSystem(env *Env) string {
	marker, set := strings.ToLower(strings.TrimSpace(os.Getenv("ICEBATH_MANAGED"))), false
	if marker != "" {
		set = true
	}
	if !set {
		path := filepath.Join(envHome(env), ".managed")
		if _, err := os.Stat(path); err == nil {
			set = true
			if b, err := os.ReadFile(path); err == nil {
				marker = strings.ToLower(strings.TrimSpace(strings.ToValidUTF8(string(b), "�")))
			} else {
				marker = ""
			}
		}
	}
	switch {
	case !set, marker == "brew", marker == "homebrew", marker == "false", marker == "0", marker == "no", marker == "off":
		return ""
	case marker == "", marker == "true", marker == "1", marker == "yes":
		return "nixos"
	}
	return marker
}

// Reports whether chmod is skipped for containers or by operator override
func containerOrChmodSkipped() bool {
	if os.Getenv("ICEBATH_CONTAINER") != "" || os.Getenv("ICEBATH_SKIP_CHMOD") != "" {
		return true
	}
	return runningInContainer()
}

// Applies ICEBATH_UID and ICEBATH_GID ownership when set, failures are non fatal
func chownToIcebathUID(path string) {
	if runtime.GOOS == "windows" {
		return
	}
	envInt := func(name string) int {
		n, err := strconv.Atoi(strings.TrimSpace(os.Getenv(name)))
		if err != nil {
			return -1
		}
		return n
	}
	uid, gid := envInt("ICEBATH_UID"), envInt("ICEBATH_GID")
	if uid < 0 && gid < 0 {
		return
	}
	os.Chown(path, uid, gid)
}

// File mode from octal permission bits including setuid, setgid, and sticky
func octalFileMode(mode uint64) os.FileMode {
	m := os.FileMode(mode & 0o777)
	if mode&0o4000 != 0 {
		m |= os.ModeSetuid
	}
	if mode&0o2000 != 0 {
		m |= os.ModeSetgid
	}
	if mode&0o1000 != 0 {
		m |= os.ModeSticky
	}
	return m
}

// Owner only directory policy unless managed, container defaults, or ICEBATH_HOME_MODE say otherwise
func applySecureDirPolicy(env *Env, path string) {
	if managedSystem(env) != "" {
		return
	}
	explicit := strings.TrimSpace(os.Getenv("ICEBATH_HOME_MODE"))
	if containerOrChmodSkipped() && explicit == "" {
		chownToIcebathUID(path)
		return
	}
	modeText := explicit
	if modeText == "" {
		modeText = "700"
	}
	mode, err := strconv.ParseUint(modeText, 8, 32)
	if err != nil {
		mode = 0o700
	}
	os.Chmod(path, octalFileMode(mode))
	chownToIcebathUID(path)
}

// Media cache directory created owner only, honouring a populated legacy directory and managed installs
func secureCacheDir(env *Env, newSubpath, oldName string) (string, error) {
	dir := homeSubdir(envHome(env), filepath.FromSlash(newSubpath), oldName)
	if managedSystem(env) != "" {
		return dir, os.MkdirAll(dir, 0o777)
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o777); err != nil {
		return "", err
	}
	if err := os.Mkdir(dir, 0o700); err != nil && !os.IsExist(err) {
		return "", err
	}
	applySecureDirPolicy(env, dir)
	return dir, nil
}

// Writes bytes owner only, a plain write when the private open fails
func writePrivateBytes(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return os.WriteFile(path, data, 0o666)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// Base64 data URL for a file, the MIME from the extension when not given
func imageToBase64DataURL(path, mime string) (string, error) {
	if mime == "" {
		mime = determineMimeType(path)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(b), nil
}

// Reports whether a provider error is about image or payload size
func isImageSizeError(err error) bool {
	s := strings.ToLower(err.Error())
	for _, h := range append(append([]string{}, visionSizeErrorHints...), "image_url", "invalid_request") {
		if strings.Contains(s, h) {
			return true
		}
	}
	return false
}

// ScaleInfo records an applied downscale from the original dimensions to the embedded ones
type ScaleInfo struct {
	OrigWidth, OrigHeight int
	NewWidth, NewHeight   int
	set                   bool
}

// Applied reports whether a downscale was recorded
func (s *ScaleInfo) Applied() bool { return s != nil && s.set }

// BuildScaleNote is the coordinate mapping disclosure for a downscale and a region crop, empty when neither applied
func BuildScaleNote(scale *ScaleInfo, crop map[string]int) string {
	var parts []string
	if scale.Applied() {
		fx, fy := 1.0, 1.0
		if scale.NewWidth != 0 {
			fx = float64(scale.OrigWidth) / float64(scale.NewWidth)
		}
		if scale.NewHeight != 0 {
			fy = float64(scale.OrigHeight) / float64(scale.NewHeight)
		}
		sx, sy := fmt.Sprintf("%.2f", fx), fmt.Sprintf("%.2f", fy)
		axes := "any coordinates you report by " + sx
		if sx != sy {
			axes = "any x coordinates you report by " + sx + " and any y coordinates by " + sy
		}
		parts = append(parts, fmt.Sprintf("Image downscaled from %dx%d to %dx%d for vision; multiply %s to map back to the original image.",
			scale.OrigWidth, scale.OrigHeight, scale.NewWidth, scale.NewHeight, axes))
	}
	if len(crop) > 0 {
		parts = append(parts, fmt.Sprintf("Analysis was performed on a cropped region of the original image starting at offset (%d, %d); "+
			"coordinates are relative to that crop origin — add the offset to map back to the full image.", crop["x"], crop["y"]))
	}
	return strings.Join(parts, " ")
}

// ResizeOptions bounds one resize for vision
type ResizeOptions struct {
	MaxBase64Bytes int
	MaxDimension   int
	ForceJPEG      bool
	Scale          *ScaleInfo
}

// ResizeImageForVision returns a base64 data URL progressively downscaled while over the byte or long edge budget
func ResizeImageForVision(env *Env, path, mime string, o ResizeOptions) (string, error) {
	return resizeImageForVision(env, path, mime, o)
}

// Halves dimensions up to four times with a 64px floor, JPEG also walking quality 85, 70, 50 per step
func resizeImageForVision(env *Env, path, mime string, o ResizeOptions) (string, error) {
	if o.MaxBase64Bytes <= 0 {
		o.MaxBase64Bytes = visionResizeTargetBytes
	}
	st, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	log := envLog(env)
	fileSize := st.Size()
	estimated := (fileSize*4)/3 + 100
	dataURL := ""
	if estimated <= int64(o.MaxBase64Bytes) && !(o.MaxDimension > 0 && imageExceedsDimension(path, o.MaxDimension)) {
		dataURL, err = imageToBase64DataURL(path, mime)
		if err != nil {
			return "", err
		}
		if len(dataURL) <= o.MaxBase64Bytes {
			return dataURL, nil
		}
	}
	raw := func() (string, error) {
		if dataURL != "" {
			return dataURL, nil
		}
		return imageToBase64DataURL(path, mime)
	}
	log.Info(fmt.Sprintf("Image file is %.1f MB (estimated base64 %.1f MB, limit %.1f MB, max_dimension=%s), auto-resizing...",
		float64(fileSize)/(1024*1024), float64(estimated)/(1024*1024), float64(o.MaxBase64Bytes)/(1024*1024), pyOptionalInt(o.MaxDimension)))
	effective := mime
	if effective == "" {
		effective = determineMimeType(path)
	}
	isPNG := effective == "image/png" && !o.ForceJPEG
	img, err := openRasterFile(path)
	if err != nil {
		log.Info("Cannot open image for resizing: " + err.Error())
		return raw()
	}
	if !isPNG && img.mode != "RGB" && img.mode != "L" {
		img = convertRaster(img, "RGB")
	}
	qualitySteps := []int{0}
	outMime := "image/png"
	if !isPNG {
		qualitySteps, outMime = []int{85, 70, 50}, "image/jpeg"
	}
	b := img.img.Bounds()
	origW, origH := b.Dx(), b.Dy()
	prevW, prevH := origW, origH
	record := func(w, h int) {
		if o.Scale != nil && (w != origW || h != origH) {
			*o.Scale = ScaleInfo{OrigWidth: origW, OrigHeight: origH, NewWidth: w, NewHeight: h, set: true}
		}
	}
	candidate := ""
	for attempt := 0; attempt < 5; attempt++ {
		cw, ch := img.img.Bounds().Dx(), img.img.Bounds().Dy()
		if attempt > 0 {
			newW, newH := max(int(float64(cw)*0.5), 64), max(int(float64(ch)*0.5), 64)
			if newW == 64 && cw > 0 {
				newH = max(int(float64(ch)*(64/float64(cw))), 64)
			} else if newH == 64 && ch > 0 {
				newW = max(int(float64(cw)*(64/float64(ch))), 64)
			}
			if newW == prevW && newH == prevH {
				break
			}
			img = resizeRaster(img, newW, newH)
			prevW, prevH = newW, newH
			cw, ch = newW, newH
			log.Info(fmt.Sprintf("Resized to %dx%d (attempt %d)", newW, newH, attempt))
		}
		dimsOK := o.MaxDimension <= 0 || max(cw, ch) <= o.MaxDimension
		for _, q := range qualitySteps {
			var enc []byte
			if isPNG {
				enc, err = encodePNG(img.img)
			} else {
				enc, err = encodeJPEG(img.img, q)
			}
			if err != nil {
				return "", err
			}
			candidate = "data:" + outMime + ";base64," + base64.StdEncoding.EncodeToString(enc)
			if len(candidate) <= o.MaxBase64Bytes && dimsOK {
				log.Info(fmt.Sprintf("Auto-resized image fits: %.1f MB (quality=%s, %dx%d)", float64(len(candidate))/(1024*1024), pyOptionalInt(q), cw, ch))
				record(cw, ch)
				return candidate, nil
			}
		}
	}
	if candidate == "" {
		return raw()
	}
	log.Warn(fmt.Sprintf("Auto-resize could not fit image under %.1f MB (best: %.1f MB)", float64(o.MaxBase64Bytes)/(1024*1024), float64(len(candidate))/(1024*1024)))
	record(img.img.Bounds().Dx(), img.img.Bounds().Dy())
	return candidate, nil
}

// Python rendering of an optional integer, None for zero
func pyOptionalInt(n int) string {
	if n == 0 {
		return "None"
	}
	return strconv.Itoa(n)
}

// Text content part of a multimodal message
type textPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// Image content part of a multimodal message
type imageURLPart struct {
	Type     string `json:"type"`
	ImageURL struct {
		URL string `json:"url"`
	} `json:"image_url"`
}

// Metadata of a native vision result
type nativeVisionMeta struct {
	ImageURL     string `json:"image_url"`
	SizeBytes    int64  `json:"size_bytes"`
	NativeVision bool   `json:"native_vision"`
}

// Multimodal tool result envelope the agent loop unwraps into tool content parts
type nativeVisionEnvelope struct {
	Multimodal  bool             `json:"_multimodal"`
	Content     []any            `json:"content"`
	TextSummary string           `json:"text_summary"`
	Meta        nativeVisionMeta `json:"meta"`
}

// Multimodal envelope with a minimal text part and the image, plus a text summary fallback
func buildNativeVisionToolResult(imageURL string, question any, imageDataURL string, sizeBytes int64, scaleNote string) string {
	text := "Image loaded into your context — you can see it natively now. Use your built-in vision to answer the user."
	if q, ok := question.(string); ok && strings.TrimSpace(q) != "" {
		text += "\n\nQuestion: " + strings.TrimSpace(q)
	}
	if scaleNote != "" {
		text += "\n\nNote: " + scaleNote
	}
	img := imageURLPart{Type: "image_url"}
	img.ImageURL.URL = imageDataURL
	return jsonCompact(nativeVisionEnvelope{
		Multimodal:  true,
		Content:     []any{textPart{Type: "text", Text: text}, img},
		TextSummary: fmt.Sprintf("Image attached natively for the main model (%.1f KB). Answer using built-in vision.", float64(sizeBytes)/1024),
		Meta:        nativeVisionMeta{ImageURL: clipRunes(imageURL, 200), SizeBytes: sizeBytes, NativeVision: true},
	})
}

// Removes a temp file ignoring every failure
func unlinkQuietly(path string) {
	if path != "" {
		os.Remove(path)
	}
}

// User facing failure of image preparation
type imagePrepError struct{ msg string }

func (e *imagePrepError) Error() string { return e.msg }

// Temp image ready to encode, the path owned by the caller
type preparedImage struct {
	path       string
	mime       string
	sizeBytes  int64
	cropOffset map[string]int
}

// Resolves, materializes, normalizes, optionally validates, and optionally crops, leaving no temp file on error
func prepareImage(ctx context.Context, env *Env, imageURL any, taskID string, region any, validateDecode bool) (*preparedImage, error) {
	src, isStr := imageURL.(string)
	if !isStr {
		return nil, &imagePrepError{msg: "image_url is required"}
	}
	resolved, err := ResolveImageSource(ctx, env, src, taskID)
	if err != nil {
		var re *ImageResolutionError
		if errors.As(err, &re) {
			return nil, &imagePrepError{msg: re.Error()}
		}
		return nil, err
	}
	dir, err := secureCacheDir(env, "cache/vision", "temp_vision_images")
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "temp_image_"+uuid.NewString()+".img")
	if err := writePrivateBytes(path, resolved.Data); err != nil {
		return nil, err
	}
	p := &preparedImage{path: path, mime: resolved.Mime, sizeBytes: int64(len(resolved.Data)), cropOffset: map[string]int{}}
	fail := func(err error) (*preparedImage, error) {
		unlinkQuietly(p.path)
		return nil, err
	}
	normalized, mime, normErr := normalizeToSupportedImage(env, p.path, p.mime)
	if normErr != "" || normalized == "" {
		if normErr == "" {
			normErr = "Image normalization failed."
		}
		return fail(&imagePrepError{msg: normErr})
	}
	p.mime = mime
	if normalized != p.path {
		unlinkQuietly(p.path)
		p.path = normalized
		st, err := os.Stat(p.path)
		if err != nil {
			return fail(err)
		}
		p.sizeBytes = st.Size()
	}
	if validateDecode {
		if msg := RunOnVisionCPU(env, func() string {
			return validateRasterImageDecodable(p.path, visionMaxValidatedFrameCount, visionMaxValidatedAggregatePixels)
		}); msg != "" {
			return fail(&imagePrepError{msg: msg})
		}
	}
	if region != nil {
		cropped, croppedMime, cropErr := cropImageRegion(p.path, region, p.cropOffset)
		if cropErr != "" || cropped == "" {
			if cropErr == "" {
				cropErr = "Region crop failed."
			}
			return fail(&imagePrepError{msg: cropErr})
		}
		unlinkQuietly(p.path)
		p.path, p.mime = cropped, croppedMime
		st, err := os.Stat(p.path)
		if err != nil {
			return fail(err)
		}
		p.sizeBytes = st.Size()
	}
	return p, nil
}

// Data URL or failure of one resize
type resizedImage struct {
	url string
	err error
}

// Resizes a prepared image inside the bounded vision worker pool
func resizeOnVisionCPU(env *Env, p *preparedImage, o ResizeOptions) resizedImage {
	return RunOnVisionCPU(env, func() resizedImage {
		u, err := resizeImageForVision(env, p.path, p.mime, o)
		return resizedImage{u, err}
	})
}

// Refusal text for a payload still over the ceiling after resizing
func tooLargeMessage(imageDataURL string) string {
	return fmt.Sprintf("Image too large for vision API: base64 payload is %.1f MB (limit %.0f MB) even after resizing. "+
		"Compress the image manually and retry.", float64(len(imageDataURL))/(1024*1024), float64(maxBase64Limit)/(1024*1024))
}

// Tool error with success false
func visionToolError(msg string) string {
	return toolErrorJSON(msg, Dict("success", false))
}

// Fast path for vision capable main models, a multimodal envelope on success or a JSON error
func visionAnalyzeNative(ctx context.Context, env *Env, sessionID string, imageURL, question any, taskID string, region any) (result string) {
	src, isStr := imageURL.(string)
	if !isStr || strings.TrimSpace(src) == "" {
		return visionToolError("image_url is required")
	}
	if dup := nativeTurnDuplicate(env, src, region); dup != "" {
		return dup
	}
	if refusal := repeatRefusal(env, sessionID, src); refusal != "" {
		return refusal
	}
	reserved := resolveRepeatCap(env) > 0
	var prepared *preparedImage
	embedded := false
	defer func() {
		if reserved && !embedded {
			releaseEmbed(env, sessionID, src)
		}
		if prepared != nil {
			unlinkQuietly(prepared.path)
		}
	}()
	failed := func(err error) string {
		envLog(env).Warn("Native vision fast path failed", "error", err)
		return visionToolError("Native vision failed: " + err.Error())
	}
	if IsInterrupted(ctx) {
		return visionToolError("Interrupted")
	}
	p, err := prepareImage(ctx, env, src, taskID, region, true)
	if err != nil {
		var pe *imagePrepError
		if errors.As(err, &pe) {
			return visionToolError(pe.Error())
		}
		return failed(err)
	}
	prepared = p
	type encoded struct {
		url string
		err error
	}
	enc := RunOnVisionCPU(env, func() encoded {
		u, err := imageToBase64DataURL(p.path, p.mime)
		return encoded{u, err}
	})
	if enc.err != nil {
		return failed(enc.err)
	}
	dataURL := enc.url
	scale := &ScaleInfo{}
	target := resolveEmbedTargetBytes(env)
	overDims := RunOnVisionCPU(env, func() bool { return imageExceedsDimension(p.path, VisionEmbedMaxDimension) })
	if len(dataURL) > target || overDims {
		res := resizeOnVisionCPU(env, p, ResizeOptions{MaxBase64Bytes: target, MaxDimension: VisionEmbedMaxDimension, ForceJPEG: true, Scale: scale})
		if res.err != nil {
			return failed(res.err)
		}
		dataURL = res.url
		if len(dataURL) > maxBase64Limit {
			return visionToolError(tooLargeMessage(dataURL))
		}
	}
	embedded = true
	if !reserved {
		recordEmbed(env, sessionID, src)
	}
	return buildNativeVisionToolResult(src, question, dataURL, p.sizeBytes, BuildScaleNote(scale, p.cropOffset))
}

// Aux call options with auxiliary.vision timeout and temperature, a floor for video
func auxCallOptions(env *Env, model string, defaultTimeout float64, minTimeout *float64) AuxOptions {
	timeout, temperature := defaultTimeout, 0.1
	func() {
		if raw := cfgAuxVision(env, "timeout"); raw != nil {
			f, ok := pyFloatOf(raw)
			if !ok {
				return
			}
			if minTimeout != nil {
				f = math.Max(f, *minTimeout)
			}
			timeout = f
		}
		if raw := cfgAuxVision(env, "temperature"); raw != nil {
			f, ok := pyFloatOf(raw)
			if !ok {
				return
			}
			temperature = f
		}
	}()
	t := temperature
	return AuxOptions{Task: "vision", Model: model, Temperature: &t, Timeout: time.Duration(timeout * float64(time.Second))}
}

// Single user message holding the prompt and one media data URL part
func mediaMessages(userPrompt, partType, dataURL string) []*state.Message {
	return []*state.Message{{Role: "user", Content: []any{
		map[string]any{"type": "text", "text": userPrompt},
		map[string]any{"type": partType, partType: map[string]any{"url": dataURL}},
	}}}
}

// Replaces the media URL of a message built by mediaMessages
func setMediaURL(msgs []*state.Message, partType, dataURL string) {
	parts := msgs[0].Content.([]any)
	parts[1].(map[string]any)[partType] = map[string]any{"url": dataURL}
}

// Error hint sets and their user facing explanation, first match wins
type analysisRule struct {
	hints    []string
	template string
}

var billingHints = []string{"402", "insufficient", "payment required", "credits", "billing"}

const billingTemplate = "Insufficient credits or payment required. Please top up your API provider account and try again. Error: {e}"

var imageErrorRules = []analysisRule{
	{billingHints, billingTemplate},
	{[]string{"does not support", "not support image", "content_policy", "multimodal", "unrecognized request argument", "image input"},
		"{model} does not support vision or our request was not accepted by the server. Error: {e}"},
	{[]string{"invalid_request", "image_url"},
		"The vision API rejected the image. This can happen when the image is in an unsupported format, corrupted, or still too " +
			"large after auto-resize. Try a smaller JPEG/PNG and retry. Error: {e}"},
}

var videoErrorRules = []analysisRule{
	{billingHints, billingTemplate},
	{[]string{"does not support", "not support video", "content_policy", "multimodal", "unrecognized request argument", "video input", "video_url"},
		"The model does not support video analysis or the request was rejected. Ensure you're using a video-capable model " +
			"(e.g. google/gemini-2.5-flash). Error: {e}"},
	{visionSizeErrorHints, "The video is too large for the API. Try compressing or trimming the video (max ~50 MB). Error: {e}"},
}

// Debug tool name and error rules per analysis kind
var analysisKinds = map[string]struct {
	tool  string
	rules []analysisRule
}{
	"image": {"vision_analyze_tool", imageErrorRules},
	"video": {"video_analyze_tool", videoErrorRules},
}

// Debug record skeleton for one analysis call
func debugCallData(kind, source, userPrompt, model string) map[string]any {
	prompt := userPrompt
	if len([]rune(userPrompt)) > 200 {
		prompt = clipRunes(userPrompt, 200) + "..."
	}
	var modelVal any
	if model != "" {
		modelVal = model
	}
	return map[string]any{
		"parameters": map[string]any{kind + "_url": source, "user_prompt": prompt, "model": modelVal},
		"error":      nil, "success": false, "analysis_length": 0, "model_used": modelVal, kind + "_size_bytes": 0,
	}
}

// Aux vision completion retrying once when the first answer came back empty
func callVisionLLM(ctx context.Context, env *Env, msgs []*state.Message, opts AuxOptions, emptyLog string, first *string) (string, error) {
	if env == nil || env.Aux == nil {
		return "", errors.New("no auxiliary model is configured for vision")
	}
	var analysis string
	if first != nil {
		analysis = *first
	} else {
		var err error
		if analysis, err = env.Aux.Complete(ctx, msgs, opts); err != nil {
			return "", err
		}
	}
	if analysis == "" {
		envLog(env).Warn(emptyLog)
		return env.Aux.Complete(ctx, msgs, opts)
	}
	return analysis, nil
}

// Ordered success payload of an analysis
type analysisSuccess struct {
	Success   bool   `json:"success"`
	Analysis  string `json:"analysis"`
	ScaleNote string `json:"scale_note,omitempty"`
}

// Ordered failure payload of an analysis
type analysisFailure struct {
	Success  bool   `json:"success"`
	Error    string `json:"error"`
	Analysis string `json:"analysis"`
}

// Analysis stage returning the text and an optional scale note, temp files appended for cleanup
type analysisStage func(userPrompt string, debug map[string]any, tempPaths *[]string) (string, string, error)

// Interrupt check, stage, and JSON result shared by image and video analysis
func runAnalysis(ctx context.Context, env *Env, kind, source, userPrompt, model string, stage analysisStage) string {
	spec := analysisKinds[kind]
	debug := debugCallData(kind, source, userPrompt, model)
	var tempPaths []string
	log := envLog(env)
	defer func() {
		for _, p := range tempPaths {
			if p == "" {
				continue
			}
			if _, err := os.Stat(p); err != nil {
				continue
			}
			if err := os.Remove(p); err != nil {
				log.Warn("Could not delete temporary file", "error", err)
			} else {
				log.Debug("Cleaned up temporary " + kind + " file")
			}
		}
	}()
	finish := func(v any) string {
		dbg := visionDebugSession(env)
		dbg.LogCall(spec.tool, debug)
		dbg.Save(env)
		return jsonIndent(v)
	}
	if IsInterrupted(ctx) {
		return visionToolError("Interrupted")
	}
	log.Info(fmt.Sprintf("Analyzing %s: %s", kind, clipRunes(source, 60)))
	log.Info("User prompt: " + clipRunes(userPrompt, 100))
	analysis, scaleNote, err := stage(userPrompt, debug, &tempPaths)
	if err != nil {
		errorMsg := fmt.Sprintf("Error analyzing %s: %s", kind, err.Error())
		log.Error(errorMsg)
		errStr := strings.ToLower(err.Error())
		template := fmt.Sprintf("There was a problem with the request and the %s could not be analyzed. Error: {e}", kind)
	rules:
		for _, rule := range spec.rules {
			for _, h := range rule.hints {
				if strings.Contains(errStr, h) {
					template = rule.template
					break rules
				}
			}
		}
		modelText := model
		if modelText == "" {
			modelText = "None"
		}
		text := strings.ReplaceAll(strings.ReplaceAll(template, "{e}", err.Error()), "{model}", modelText)
		debug["error"] = errorMsg
		return finish(analysisFailure{Success: false, Error: errorMsg, Analysis: text})
	}
	length := len([]rune(analysis))
	log.Info(fmt.Sprintf("%s analysis completed (%d characters)", strings.ToUpper(kind[:1])+kind[1:], length))
	if analysis == "" {
		analysis = fmt.Sprintf("There was a problem with the request and the %s could not be analyzed.", kind)
	}
	result := analysisSuccess{Success: true, Analysis: analysis}
	if scaleNote != "" {
		result.Analysis = "[" + scaleNote + "] " + analysis
		result.ScaleNote = scaleNote
	}
	debug["success"], debug["analysis_length"] = true, length
	return finish(result)
}

// VisionAnalyzeTool describes an image with the default auxiliary vision model and returns the JSON result text
func VisionAnalyzeTool(ctx context.Context, env *Env, imageURL, userPrompt string) string {
	return visionAnalyzeTool(visionCtx(ctx, env), env, imageURL, userPrompt, "", "", nil)
}

// Describes an image with the auxiliary vision model, the prompt already formatted by the caller
func visionAnalyzeTool(ctx context.Context, env *Env, imageURL any, userPrompt, model, taskID string, region any) string {
	source := pyStrOrEmpty(imageURL)
	return runAnalysis(ctx, env, "image", source, userPrompt, model, func(prompt string, debug map[string]any, tempPaths *[]string) (string, string, error) {
		log := envLog(env)
		p, err := prepareImage(ctx, env, imageURL, taskID, region, false)
		if err != nil {
			return "", "", err
		}
		*tempPaths = append(*tempPaths, p.path)
		log.Info(fmt.Sprintf("Image ready (%.1f KB)", float64(p.sizeBytes)/1024))
		log.Info("Converting image to base64...")
		type encoded struct {
			url string
			err error
		}
		enc := RunOnVisionCPU(env, func() encoded {
			u, err := imageToBase64DataURL(p.path, p.mime)
			return encoded{u, err}
		})
		if enc.err != nil {
			return "", "", enc.err
		}
		dataURL := enc.url
		log.Info(fmt.Sprintf("Image converted to base64 (%.1f KB)", float64(len(dataURL))/1024))
		scale := &ScaleInfo{}
		if len(dataURL) > maxBase64Limit {
			res := resizeOnVisionCPU(env, p, ResizeOptions{Scale: scale})
			if res.err != nil {
				return "", "", res.err
			}
			if dataURL = res.url; len(dataURL) > maxBase64Limit {
				return "", "", errors.New(tooLargeMessage(dataURL))
			}
		}
		debug["image_size_bytes"] = p.sizeBytes
		msgs := mediaMessages(prompt, "image_url", dataURL)
		log.Info("Processing image with vision model...")
		opts := auxCallOptions(env, model, 120.0, nil)
		if env == nil || env.Aux == nil {
			return "", "", errors.New("no auxiliary model is configured for vision")
		}
		response, apiErr := env.Aux.Complete(ctx, msgs, opts)
		if apiErr != nil {
			if !(isImageSizeError(apiErr) && len(dataURL) > visionResizeTargetBytes) {
				return "", "", apiErr
			}
			log.Info(fmt.Sprintf("API rejected image (%.1f MB, likely too large); auto-resizing to ~%.0f MB and retrying...",
				float64(len(dataURL))/(1024*1024), float64(visionResizeTargetBytes)/(1024*1024)))
			res := resizeOnVisionCPU(env, p, ResizeOptions{Scale: scale})
			if res.err != nil {
				return "", "", res.err
			}
			dataURL = res.url
			setMediaURL(msgs, "image_url", dataURL)
			if response, apiErr = env.Aux.Complete(ctx, msgs, opts); apiErr != nil {
				return "", "", apiErr
			}
		}
		analysis, err := callVisionLLM(ctx, env, msgs, opts, "Vision LLM returned empty content, retrying once", &response)
		if err != nil {
			return "", "", err
		}
		return analysis, BuildScaleNote(scale, p.cropOffset), nil
	})
}

// Reports whether an auxiliary vision client resolves explicitly or through the auto chain, a resolver crash panicking into the registry
func checkVideoRequirements(env *Env) bool {
	if env == nil || env.Aux == nil {
		return false
	}
	r, ok := env.Aux.(AuxTaskResolver)
	if !ok {
		panic(fmt.Errorf("auxiliary client %T does not implement tools.AuxTaskResolver, the vision gate cannot probe it", env.Aux))
	}
	for _, provider := range []string{"", "auto"} {
		resolved, err := r.ResolvesTask("vision", provider)
		if err != nil {
			panic(err)
		}
		if resolved {
			return true
		}
	}
	return false
}

// AuxTaskResolver is implemented by auxiliary routers that resolve a task to a client in probe mode without building it
type AuxTaskResolver interface {
	ResolvesTask(task, provider string) (bool, error)
}

// Image gate, an auxiliary vision client or the native fast path
func checkVisionRequirements(env *Env) bool {
	return ShouldUseNativeVisionFastPath(context.Background(), env) || checkVideoRequirements(env)
}

var visionAnalyzeSchema = map[string]any{
	"name":        "vision_analyze",
	"description": "Load an image into the conversation so you can see it. Call it any time the user references an image — then answer from what you see.",
	"parameters": map[string]any{
		"type": "object",
		"properties": map[string]any{
			"image_url": map[string]any{
				"type":        "string",
				"description": "Image URL (http/https), local file path, or data: URL to load.",
			},
			"question": map[string]any{
				"type":        "string",
				"description": "Your question or request about the image.",
			},
			"region": map[string]any{
				"type":     "array",
				"items":    map[string]any{"type": "integer"},
				"minItems": 4,
				"maxItems": 4,
				"description": "Optional [x1, y1, x2, y2] crop in ORIGINAL-image pixel coordinates, applied before any downscaling — the crop " +
					"keeps full resolution. Load the full image first, then re-call with a region to zoom into small text or fine detail.",
			},
		},
		"required": []any{"image_url", "question"},
	},
}

// First non empty auxiliary.<section>.model, else the first non empty legacy env var
func configuredAuxModel(env *Env, sections, envVars []string) string {
	for _, section := range sections {
		v := cfgTree(env).Sub("auxiliary").Sub(section)["model"]
		if truthy(v) {
			if s := strings.TrimSpace(pyStr(v)); s != "" {
				return s
			}
			break
		}
	}
	for _, e := range envVars {
		if v := secretValue(env, e); v != "" {
			return v
		}
	}
	return ""
}

// Argument value as Python str would render it, empty when absent
func argPyStr(args map[string]any, key string) string {
	v, ok := args[key]
	if !ok {
		return ""
	}
	return pyStr(v)
}

// vision_analyze handler choosing the native fast path or the auxiliary description
func handleVisionAnalyze(ctx context.Context, call *Call) string {
	env := call.Env
	imageURL, ok := call.Args["image_url"]
	if !ok {
		imageURL = ""
	}
	question, ok := call.Args["question"]
	if !ok {
		question = ""
	}
	region := call.Args["region"]
	if ShouldUseNativeVisionFastPath(ctx, env) {
		envLog(env).Info("vision_analyze: native fast path")
		return visionAnalyzeNative(ctx, env, call.SessionID, imageURL, question, call.TaskID, region)
	}
	fullPrompt := "Fully describe and explain everything about this image, then answer the following question:\n\n" + argPyStr(call.Args, "question")
	model := configuredAuxModel(env, []string{"vision"}, []string{"AUXILIARY_VISION_MODEL"})
	return visionAnalyzeTool(ctx, env, imageURL, fullPrompt, model, call.TaskID, region)
}

// Extension to video MIME, avi and mkv falling back to mp4
var videoMimeTypes = map[string]string{
	".mp4": "video/mp4", ".webm": "video/webm", ".mov": "video/mov", ".avi": "video/mp4",
	".mkv": "video/mp4", ".mpeg": "video/mpeg", ".mpg": "video/mpeg",
}

// Video MIME type from the path extension, empty when unsupported
func detectVideoMimeType(path string) string {
	return videoMimeTypes[strings.ToLower(pySuffix(path))]
}

// Refusal naming the supported video extensions
func unsupportedVideoFormat(suffix string) string {
	return fmt.Sprintf("Unsupported video format: '%s'. Supported: .avi, .mkv, .mov, .mp4, .mpeg, .mpg, .webm", suffix)
}

// Local video path for a sandbox path, host file, or http URL, only files made here join tempPaths
func materializeVideo(ctx context.Context, env *Env, videoURL, taskID string, tempPaths *[]string) (string, error) {
	log := envLog(env)
	source := strings.TrimPrefix(videoURL, "file://")
	localPath := pyExpandUser(source)
	lowered := strings.ToLower(strings.TrimSpace(videoURL))
	pathLike := lowered != "" && !strings.HasPrefix(lowered, "http://") && !strings.HasPrefix(lowered, "https://") && !strings.HasPrefix(lowered, "data:")
	local, err := isLocalTerminalBackend(env)
	if err != nil {
		return "", err
	}
	if !local && pathLike {
		log.Info("Reading video source via terminal backend: " + videoURL)
		suffix := strings.ToLower(pySuffix(source))
		if _, ok := videoMimeTypes[suffix]; !ok {
			return "", errors.New(unsupportedVideoFormat(suffix))
		}
		resolved, err := ResolveImageSource(ctx, env, videoURL, taskID, "video")
		if err != nil {
			var re *ImageResolutionError
			if errors.As(err, &re) {
				return "", fmt.Errorf("Could not read video from terminal backend: %s", re.Error())
			}
			return "", err
		}
		dir, err := secureCacheDir(env, "cache/video", "temp_video_files")
		if err != nil {
			return "", err
		}
		path := filepath.Join(dir, "terminal_video_"+uuid.NewString()+suffix)
		if err := writePrivateBytes(path, resolved.Data); err != nil {
			return "", err
		}
		*tempPaths = append(*tempPaths, path)
		return path, nil
	}
	if abs := resolveAgainstCwd(env, localPath); isRegularFile(abs) {
		if blocked := readBlockError(env, localPath); blocked != "" {
			return "", errors.New(blocked)
		}
		log.Info("Using local video file: " + videoURL)
		return abs, nil
	}
	if validateImageURL(env, videoURL) {
		if blocked := checkWebsiteAccess(env, videoURL); blocked != nil {
			return "", &mediaPolicyError{msg: blocked.Message}
		}
		dir, err := secureCacheDir(env, "cache/video", "temp_video_files")
		if err != nil {
			return "", err
		}
		path := filepath.Join(dir, "temp_video_"+uuid.NewString()+".mp4")
		*tempPaths = append(*tempPaths, path)
		if err := downloadMedia(ctx, env, videoURL, path, 3, mediaDownloadOptions{
			label: "Video", accept: "video/*,*/*;q=0.8", maxBytes: maxVideoBase64Bytes,
			timeout: 60 * time.Second, retryAll: true}); err != nil {
			return "", err
		}
		return path, nil
	}
	return "", errors.New("Invalid video source. Provide an HTTP/HTTPS URL or a valid local file path.")
}

// Analyzes a video with the multimodal auxiliary model
func videoAnalyzeTool(ctx context.Context, env *Env, videoURL any, userPrompt, model, taskID string) string {
	source := pyStrOrEmpty(videoURL)
	return runAnalysis(ctx, env, "video", source, userPrompt, model, func(prompt string, debug map[string]any, tempPaths *[]string) (string, string, error) {
		log := envLog(env)
		src, isStr := videoURL.(string)
		if !isStr {
			return "", "", errors.New("Invalid video source. Provide an HTTP/HTTPS URL or a valid local file path.")
		}
		path, err := materializeVideo(ctx, env, src, taskID, tempPaths)
		if err != nil {
			return "", "", err
		}
		st, err := os.Stat(path)
		if err != nil {
			return "", "", err
		}
		size := st.Size()
		sizeMB := float64(size) / (1024 * 1024)
		log.Info(fmt.Sprintf("Video ready (%.1f MB)", sizeMB))
		mime := detectVideoMimeType(path)
		if mime == "" {
			return "", "", errors.New(unsupportedVideoFormat(pySuffix(path)))
		}
		if size > videoSizeWarnBytes {
			log.Warn(fmt.Sprintf("Video is %.1f MB — may be slow or rejected", sizeMB))
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return "", "", err
		}
		dataURL := "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(raw)
		if len(dataURL) > maxVideoBase64Bytes {
			return "", "", fmt.Errorf("Video too large for API: base64 payload is %.1f MB (limit %.0f MB). Compress or trim the video and retry.",
				float64(len(dataURL))/(1024*1024), float64(maxVideoBase64Bytes)/(1024*1024))
		}
		debug["video_size_bytes"] = size
		floor := 180.0
		opts := auxCallOptions(env, model, 180.0, &floor)
		analysis, err := callVisionLLM(ctx, env, mediaMessages(prompt, "video_url", dataURL), opts, "Empty video response, retrying once", nil)
		if err != nil {
			return "", "", err
		}
		return analysis, "", nil
	})
}

var videoAnalyzeSchema = map[string]any{
	"name": "video_analyze",
	"description": "Analyze a video from a URL or local file path using a multimodal AI model. " +
		"Sends the video to a video-capable model (e.g. Gemini) for understanding. " +
		"Use this for video files — for images, use vision_analyze instead. " +
		"Supports mp4, webm, mov, avi, mkv, mpeg formats. " +
		"Note: large videos (>20 MB) may be slow; max ~50 MB.",
	"parameters": map[string]any{
		"type": "object",
		"properties": map[string]any{
			"video_url": map[string]any{
				"type":        "string",
				"description": "Video URL (http/https) or local file path to analyze.",
			},
			"question": map[string]any{
				"type":        "string",
				"description": "Your specific question about the video. The AI will describe what happens in the video and answer your question.",
			},
		},
		"required": []any{"video_url", "question"},
	},
}

// video_analyze handler building the full description prompt
func handleVideoAnalyze(ctx context.Context, call *Call) string {
	videoURL, ok := call.Args["video_url"]
	if !ok {
		videoURL = ""
	}
	fullPrompt := "Fully describe and explain everything happening in this video, including visual content, motion, audio cues, " +
		"text overlays, and scene transitions. Then answer the following question:\n\n" + argPyStr(call.Args, "question")
	model := configuredAuxModel(call.Env, []string{"video", "vision"}, []string{"AUXILIARY_VIDEO_MODEL", "AUXILIARY_VISION_MODEL"})
	return videoAnalyzeTool(ctx, call.Env, videoURL, fullPrompt, model, call.TaskID)
}

// Registers vision_analyze and video_analyze
func registerVisionTools() {
	Register(Def{Name: "vision_analyze", Toolset: "vision", Schema: visionAnalyzeSchema, Handler: handleVisionAnalyze,
		Check: checkVisionRequirements, Emoji: "\U0001F441️"})
	Register(Def{Name: "video_analyze", Toolset: "video", Schema: videoAnalyzeSchema, Handler: handleVideoAnalyze,
		Check: checkVideoRequirements, Emoji: "\U0001F3AC"})
}
