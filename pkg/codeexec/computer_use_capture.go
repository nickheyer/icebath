package codeexec

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	"image/jpeg"
	"image/png"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/tiff"
	_ "golang.org/x/image/webp"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/fnmatch"
	"github.com/nickheyer/icebath/pkg/internal/pybase64"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypil"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	cuMinProviderImageDimension = 8
	cuMaxElementLabelChars      = 120
	cuMaxSpillFiles             = 20
	cuMaxCaptureFiles           = cuMaxSpillFiles
	cuMaxVisionDim              = 1456
)

const cuVisionPrompt = "Describe what is visible in this desktop application screenshot in concise but specific terms. Mention " +
	"the app name and window title if visible, the overall layout, any labelled buttons, menus or text fields, " +
	"and any prominent text content the user would need to know about. Do not invent details that are not " +
	"actually visible.\n\nAX/SOM index for cross-reference:\n"

// Base64 screenshot text of a capture, empty when it carries none
func capturePNG(c *CaptureResult) string {
	if c.PNGB64 == nil {
		return ""
	}
	return *c.PNGB64
}

// MIME type from the driver else sniffed from the base64 prefix, with the matching file extension
func captureImageFormat(c *CaptureResult) (string, string) {
	mime := ""
	if c.ImageMimeType != nil {
		mime = *c.ImageMimeType
	}
	if mime == "" {
		mime = "image/png"
		if strings.HasPrefix(capturePNG(c), "/9j/") {
			mime = "image/jpeg"
		}
	}
	if pystr.Lower(mime) == "image/jpeg" {
		return mime, ".jpg"
	}
	return mime, ".png"
}

// True for a zero rect, which KDE and Qt report for elements clickable only by index
func boundsUnknown(b [4]int) bool {
	return b == [4]int{}
}

// Python slice prefix of the first n code points
func pyPrefix(s string, n int) string {
	rs := pystr.Runes(s)
	if len(rs) <= n {
		return s
	}
	return pystr.FromRunes(rs[:n])
}

// Bounds tuple as a JSON list
func boundsList(b [4]int) []any {
	return []any{b[0], b[1], b[2], b[3]}
}

// JSON form of an element with its label capped and an unknown rect nulled
func elementToDict(e UIElement) *pyjson.Object {
	o := pyjson.NewObject()
	o.Set("index", e.Index)
	o.Set("role", e.Role)
	o.Set("label", pyPrefix(e.Label, cuMaxElementLabelChars))
	if boundsUnknown(e.Bounds) {
		o.Set("bounds", nil)
	} else {
		o.Set("bounds", boundsList(e.Bounds))
	}
	o.Set("app", e.App)
	if pystr.Len(e.Label) > cuMaxElementLabelChars {
		o.Set("label_truncated", true)
	}
	return o
}

// One summary line per element up to maxLines, then a count of the rest
func formatElements(elements []UIElement, maxLines int) []string {
	out := []string{}
	for i, e := range elements {
		if i >= maxLines {
			break
		}
		line := fmt.Sprintf("  #%d %s %s ", e.Index, e.Role, pyrepr.Repr(pyPrefix(strings.ReplaceAll(e.Label, "\n", " "), 60)))
		if boundsUnknown(e.Bounds) {
			line += "@ bounds-unknown (click by element index)"
		} else {
			line += fmt.Sprintf("@ (%d, %d, %d, %d)", e.Bounds[0], e.Bounds[1], e.Bounds[2], e.Bounds[3])
		}
		if e.App != "" {
			line += " [" + e.App + "]"
		}
		out = append(out, line)
	}
	if len(elements) > maxLines {
		out = append(out, fmt.Sprintf("  ... +%d more (call capture with app= to narrow)", len(elements)-maxLines))
	}
	return out
}

// Scale and note when element bounds extend past the screenshot by more than five percent
func boundsHints(elements []UIElement, imageWidth, imageHeight int) (*float64, string) {
	if len(elements) == 0 || imageWidth <= 0 || imageHeight <= 0 {
		return nil, ""
	}
	maxX, maxY := 0, 0
	for _, e := range elements {
		maxX, maxY = max(maxX, e.Bounds[0]+e.Bounds[2]), max(maxY, e.Bounds[1]+e.Bounds[3])
	}
	if float64(maxX) <= float64(imageWidth)*1.05 && float64(maxY) <= float64(imageHeight)*1.05 {
		return nil, ""
	}
	note := fmt.Sprintf("element bounds are in native desktop coordinates (extend to ~%dx%d), "+
		"NOT screenshot pixels (%dx%d). coordinate= clicks expect the native "+
		"space — derive click points from element bounds, or scale screenshot positions up accordingly", maxX, maxY, imageWidth, imageHeight)
	scale := pyint.Round(max(float64(maxX)/float64(imageWidth), float64(maxY)/float64(imageHeight)), 2)
	return &scale, note
}

// Scale factor between element bounds and screenshot pixels, nil when they share a space
func boundsScale(elements []UIElement, imageWidth, imageHeight int) *float64 {
	s, _ := boundsHints(elements, imageWidth, imageHeight)
	return s
}

// Note naming the native coordinate space, empty when bounds match the screenshot
func boundsSpaceNote(elements []UIElement, imageWidth, imageHeight int) string {
	_, n := boundsHints(elements, imageWidth, imageHeight)
	return n
}

type captureView struct {
	cap            *CaptureResult
	visible        []UIElement
	total          int
	width, height  int
	truncated      int
	boundsScale    *float64
	boundsNote     string
	axCapped       int
	elementsFile   *string
	screenshotPath *string
	dimsOmitted    *[2]int
	hasImage       bool
}

// Python slice of the first n elements, a negative n dropping from the end
func elementsPrefix(es []UIElement, n int) []UIElement {
	if n < 0 {
		n = max(len(es)+n, 0)
	}
	return es[:min(n, len(es))]
}

// Facts of one capture computed once for every response branch, spilling and persisting as needed
func newCaptureView(ctx context.Context, env *tools.Env, c *CaptureResult, maxElements int) *captureView {
	visible := elementsPrefix(c.Elements, maxElements)
	var dims *[2]int
	if b64 := capturePNG(c); b64 != "" {
		if raw, err := pybase64.B64Decode(b64); err == nil {
			if w, h, ok := provider.ImageDimensionsFromBytes(raw); ok {
				dims = &[2]int{w, h}
			}
		}
	}
	width, height := c.Width, c.Height
	if dims != nil {
		width, height = dims[0], dims[1]
	}
	scale, note := boundsHints(visible, width, height)
	lostDetail := len(c.Elements) > len(visible)
	for _, e := range visible {
		if pystr.Len(e.Label) > cuMaxElementLabelChars {
			lostDetail = true
		}
	}
	tooSmall := dims != nil && min(dims[0], dims[1]) < cuMinProviderImageDimension
	v := &captureView{cap: c, visible: visible, total: len(c.Elements), width: width, height: height,
		truncated: len(c.Elements) - len(visible), boundsScale: scale, boundsNote: note,
		hasImage: capturePNG(c) != "" && c.Mode != "ax" && !tooSmall}
	if c.AXMaxElements > 0 && len(c.Elements) >= c.AXMaxElements {
		v.axCapped = c.AXMaxElements
	}
	if lostDetail {
		v.elementsFile = spillElementsToFile(tools.WithEnvHome(ctx, env), env, c)
	}
	if v.hasImage {
		v.screenshotPath = persistCaptureImage(tools.WithEnvHome(ctx, env), env, c)
	}
	if tooSmall {
		v.dimsOmitted = dims
	}
	return v
}

// Human readable capture summary whose line order is contract
func captureSummaryLines(v *captureView) []string {
	var notes []string
	if v.boundsNote != "" {
		n := v.boundsNote
		if v.boundsScale != nil && *v.boundsScale != 0 {
			s := pyrepr.Repr(*v.boundsScale)
			n += fmt.Sprintf("; estimated scale ~%sx (screenshot position x %s ≈ native coordinate)", s, s)
		}
		notes = append(notes, n)
	}
	if v.screenshotPath != nil && *v.screenshotPath != "" {
		notes = append(notes, "shareable screenshot saved to "+*v.screenshotPath)
	}
	if v.cap.Note != "" {
		notes = append(notes, v.cap.Note)
	}
	if v.elementsFile != nil && *v.elementsFile != "" {
		full := "full "
		if v.axCapped != 0 {
			full = ""
		}
		notes = append(notes, full+"element tree with untruncated labels saved to "+*v.elementsFile+
			" — read_file/search_files it if you need dropped label text or elements beyond the cap")
	}
	if v.axCapped != 0 {
		notes = append(notes, fmt.Sprintf("accessibility walk capped at %d elements; pass app= to narrow", v.axCapped))
	}
	head := fmt.Sprintf("capture mode=%s %dx%d", v.cap.Mode, v.width, v.height)
	if v.cap.App != "" {
		head += " app=" + v.cap.App
	}
	if v.cap.WindowTitle != "" {
		head += " window=" + pyrepr.Repr(v.cap.WindowTitle)
	}
	lines := []string{head, fmt.Sprintf("%d interactable element(s):", v.total)}
	for _, n := range notes {
		lines = append(lines, "  ("+n+")")
	}
	lines = append(lines, formatElements(v.visible, 40)...)
	if v.dimsOmitted != nil {
		lines = append(lines, fmt.Sprintf("  (screenshot omitted: %dx%d is below the %dx%d provider minimum)",
			v.dimsOmitted[0], v.dimsOmitted[1], cuMinProviderImageDimension, cuMinProviderImageDimension))
	}
	return lines
}

// Sets each optional field whose value is truthy, in the given order
func setPresent(o *pyjson.Object, truncated int, elementsFile, screenshotPath *string, scale *float64, order ...string) {
	for _, k := range order {
		switch k {
		case "truncated_elements":
			if truncated != 0 {
				o.Set(k, truncated)
			}
		case "elements_file":
			if elementsFile != nil && *elementsFile != "" {
				o.Set(k, *elementsFile)
			}
		case "screenshot_path":
			if screenshotPath != nil && *screenshotPath != "" {
				o.Set(k, *screenshotPath)
			}
		case "bounds_scale":
			if scale != nil && *scale != 0 {
				o.Set(k, *scale)
			}
		}
	}
}

// JSON text payload of the AX, vision unavailable and aux vision branches in contract key order
func textCapturePayload(v *captureView, summary string, extra *pyjson.Object) string {
	o := pyjson.NewObject()
	o.Set("mode", v.cap.Mode)
	o.Set("width", v.width)
	o.Set("height", v.height)
	o.Set("app", v.cap.App)
	o.Set("window_title", v.cap.WindowTitle)
	elems := make([]any, 0, len(v.visible))
	for _, e := range v.visible {
		elems = append(elems, elementToDict(e))
	}
	o.Set("elements", elems)
	o.Set("total_elements", v.total)
	o.Set("summary", summary)
	if extra != nil {
		for _, k := range extra.Keys {
			o.Set(k, extra.Vals[k])
		}
	}
	setPresent(o, v.truncated, v.elementsFile, v.screenshotPath, v.boundsScale,
		"truncated_elements", "elements_file", "screenshot_path", "bounds_scale")
	out, err := pyjson.Dumps(o)
	if err != nil {
		panic(err)
	}
	return out
}

// Hex sha256 over the MIME type and the ASCII bytes of the base64 screenshot
func captureDigest(c *CaptureResult) string {
	mime := ""
	if c.ImageMimeType != nil {
		mime = *c.ImageMimeType
	}
	h := sha256.New()
	h.Write([]byte(mime + ":"))
	b64 := capturePNG(c)
	ascii := make([]byte, 0, len(b64))
	for i := 0; i < len(b64); i++ {
		if b64[i] < 0x80 {
			ascii = append(ascii, b64[i])
		}
	}
	h.Write(ascii)
	return hex.EncodeToString(h.Sum(nil))
}

// Tool result of a capture, the multimodal envelope or JSON text after dedup and vision routing
func captureResponse(ctx context.Context, env *tools.Env, c *CaptureResult, maxElements int, sessionID *string) (any, error) {
	v := newCaptureView(tools.WithEnvHome(ctx, env), env, c, maxElements)
	lines := captureSummaryLines(v)
	summary := strings.Join(lines, "\n")
	var extra *pyjson.Object
	switch {
	case v.hasImage && sessionID != nil && *sessionID != "" &&
		screenshotDedupCheck(scopedSID(ctx, *sessionID), captureDigest(c), [2]string{c.App, c.WindowTitle}):
		lines = append(lines, "  (screen unchanged since the previous capture — image omitted to save context; the previous "+
			"capture's screenshot/analysis still shows the current state. Element indices below are fresh "+
			"and remain the preferred way to act.)")
		extra = pyjson.NewObject()
		extra.Set("screen_unchanged", true)
	case v.hasImage:
		if !shouldRouteThroughAuxVision(ctx, env) {
			return multimodalCaptureEnvelope(v, summary), nil
		}
		if routed := routeCaptureThroughAuxVision(ctx, env, c, summary, v.visible, v.truncated, v.elementsFile, v.screenshotPath); routed != nil {
			return *routed, nil
		}
		lines = append(lines, "  (vision unavailable: the auxiliary vision model could not be reached; screenshot "+
			"omitted. Element-index actions still work — drive via the element list above.)")
		extra = pyjson.NewObject()
		extra.Set("vision_unavailable", true)
	}
	if v.truncated != 0 {
		lines = append(lines, fmt.Sprintf("  (response truncated to %d of %d elements; the full tree is in "+
			"elements_file — read_file/search_files it, or pass app= to narrow scope)", len(v.visible), v.total))
	}
	return textCapturePayload(v, strings.Join(lines, "\n"), extra), nil
}

// Multimodal envelope carrying the summary text, the screenshot data URL and the capture meta
func multimodalCaptureEnvelope(v *captureView, summary string) *pyjson.Object {
	text := pyjson.NewObject()
	text.Set("type", "text")
	text.Set("text", summary)
	url := pyjson.NewObject()
	mime, _ := captureImageFormat(v.cap)
	url.Set("url", "data:"+mime+";base64,"+capturePNG(v.cap))
	img := pyjson.NewObject()
	img.Set("type", "image_url")
	img.Set("image_url", url)
	meta := pyjson.NewObject()
	meta.Set("mode", v.cap.Mode)
	meta.Set("width", v.width)
	meta.Set("height", v.height)
	meta.Set("elements", v.total)
	meta.Set("png_bytes", v.cap.PNGBytesLen)
	setPresent(meta, 0, v.elementsFile, v.screenshotPath, v.boundsScale, "screenshot_path", "elements_file", "bounds_scale")
	out := pyjson.NewObject()
	out.Set("_multimodal", true)
	out.Set("content", []any{text, img})
	out.Set("text_summary", summary)
	out.Set("meta", meta)
	return out
}

// Creates a media cache dir owner only unless the install is managed, falling back to a plain mkdir
func secureDirPolicy(ctx context.Context, env *tools.Env, cacheDir string) error {
	h := envHome(env)
	if home.ManagedSystem(h) != "" {
		return os.MkdirAll(cacheDir, 0o777)
	}
	if _, err := home.MakeUserDir(tools.WithEnvHome(ctx, env), cacheDir, 0o700, true, true); err != nil {
		slog.Debug(fmt.Sprintf("computer_use: cache dir creation failed for %s: %v", cacheDir, err))
		return os.MkdirAll(cacheDir, 0o777)
	}
	config.SecureDir(h, cacheDir)
	return nil
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

// Path of a new home cache file after pruning the oldest pattern matches down to cap minus one
func cacheFile(ctx context.Context, env *tools.Env, subdir, legacy, name, pattern string, limit int) (string, error) {
	dir := home.HermesDir(envHome(env), subdir, legacy)
	if err := secureDirPolicy(tools.WithEnvHome(ctx, env), env, dir); err != nil {
		return "", err
	}
	if pattern != "" {
		pruneCacheFiles(dir, pattern, limit)
	}
	return filepath.Join(dir, name), nil
}

// Unlinks the oldest matching entries by mtime, stopping at the first failure as the suppressed loop does
func pruneCacheFiles(dir, pattern string, limit int) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type stamped struct {
		path  string
		mtime int64
	}
	var files []stamped
	for _, e := range entries {
		if !fnmatch.Fnmatch(e.Name(), pattern) {
			continue
		}
		p := filepath.Join(dir, e.Name())
		st, err := os.Stat(p)
		if err != nil {
			return
		}
		files = append(files, stamped{p, st.ModTime().UnixNano()})
	}
	sort.SliceStable(files, func(i, j int) bool { return files[i].mtime < files[j].mtime })
	for _, f := range files[:max(0, min(len(files), len(files)-(limit-1)))] {
		if err := os.Remove(f.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return
		}
	}
}

// Bounded cache write returning the path, nil on any failure so capture keeps working
func writeCacheFile(ctx context.Context, env *tools.Env, what, subdir, legacy, name, pattern string, limit int, write func(string) error) *string {
	path, err := cacheFile(tools.WithEnvHome(ctx, env), env, subdir, legacy, name, pattern, limit)
	if err == nil {
		err = write(path)
	}
	if err != nil {
		slog.Debug(fmt.Sprintf("computer_use: %s failed: %v", what, err))
		return nil
	}
	return &path
}

// Copy of the screenshot in the image cache for attachment surfaces, nil without an image
var persistCaptureImage = func(ctx context.Context, env *tools.Env, c *CaptureResult) *string {
	b64 := capturePNG(c)
	if b64 == "" {
		return nil
	}
	_, ext := captureImageFormat(c)
	return writeCacheFile(tools.WithEnvHome(ctx, env), env, "screenshot persistence", "cache/images", "image_cache", "computer_use_"+uuid4Hex()+ext,
		"computer_use_*.*", cuMaxCaptureFiles, func(p string) error {
			raw, err := pybase64.B64Decode(b64)
			if err != nil {
				return err
			}
			return writePrivateBytes(p, raw)
		})
}

// Full element tree with untruncated labels in a cache file for read_file and search_files
var spillElementsToFile = func(ctx context.Context, env *tools.Env, c *CaptureResult) *string {
	payload := pyjson.NewObject()
	payload.Set("app", c.App)
	payload.Set("window_title", c.WindowTitle)
	payload.Set("total_elements", len(c.Elements))
	elems := make([]any, 0, len(c.Elements))
	for _, e := range c.Elements {
		o := pyjson.NewObject()
		o.Set("index", e.Index)
		o.Set("role", e.Role)
		o.Set("label", e.Label)
		o.Set("bounds", boundsList(e.Bounds))
		o.Set("app", e.App)
		elems = append(elems, o)
	}
	payload.Set("elements", elems)
	return writeCacheFile(tools.WithEnvHome(ctx, env), env, "element spill", "cache/computer_use", "computer_use_cache", "elements_"+uuid4Hex()+".json",
		"elements_*.json", cuMaxSpillFiles, func(p string) error {
			text, err := pyjson.Dumps(payload, pyjson.EnsureASCII(false), pyjson.Indent(1))
			if err != nil {
				return err
			}
			if runtime.GOOS == "windows" {
				text = strings.ReplaceAll(text, "\n", "\r\n")
			}
			return os.WriteFile(p, []byte(text), 0o666)
		})
}

// Pillow mode facts of encoded bytes, alpha for RGBA and LA PNGs and nearest resampling for palette and bilevel
func visionImageMode(raw []byte, img image.Image) (hasAlpha, nearest bool) {
	if bytes.HasPrefix(raw, []byte("\x89PNG\r\n\x1a\n")) && len(raw) > 25 {
		depth, colorType := raw[24], raw[25]
		return colorType == 4 || colorType == 6, colorType == 3 || (colorType == 0 && depth == 1)
	}
	switch img.(type) {
	case *image.Paletted:
		return false, true
	case *image.NRGBA, *image.RGBA, *image.NRGBA64, *image.RGBA64:
		return true, false
	}
	return false, false
}

// Nearest neighbour resize sampling each output pixel at its centre as the Pillow affine transform does
func nearestResize(img image.Image, w, h int) image.Image {
	b := img.Bounds()
	sx, sy := float64(b.Dx())/float64(w), float64(b.Dy())/float64(h)
	at := func(x, y int) (int, int) {
		return b.Min.X + int((float64(x)+0.5)*sx), b.Min.Y + int((float64(y)+0.5)*sy)
	}
	switch src := img.(type) {
	case *image.Paletted:
		out := image.NewPaletted(image.Rect(0, 0, w, h), src.Palette)
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				ix, iy := at(x, y)
				out.SetColorIndex(x, y, src.ColorIndexAt(ix, iy))
			}
		}
		return out
	default:
		out := image.NewGray(image.Rect(0, 0, w, h))
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				ix, iy := at(x, y)
				out.Set(x, y, img.At(ix, iy))
			}
		}
		return out
	}
}

// Downscales encoded image bytes so the longest side fits, with a note giving the coordinate factor
func shrinkCaptureForVision(raw []byte, ext string, maxDim int) ([]byte, *string) {
	out, note, err := shrinkCaptureBytes(raw, ext, maxDim)
	if err != nil {
		slog.Debug(fmt.Sprintf("computer_use: vision downscale skipped: %v", err))
		return raw, nil
	}
	return out, note
}

// Pillow thumbnail of the capture saved as JPEG or PNG by extension, the bytes unchanged when they fit
func shrinkCaptureBytes(raw []byte, ext string, maxDim int) ([]byte, *string, error) {
	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, nil, errors.New("cannot identify image file")
	}
	origW, origH := img.Bounds().Dx(), img.Bounds().Dy()
	if max(origW, origH) <= maxDim {
		return raw, nil, nil
	}
	hasAlpha, nearest := visionImageMode(raw, img)
	var small image.Image
	if nearest {
		w, h, _ := pypil.ThumbnailSize(origW, origH, maxDim)
		small = nearestResize(img, w, h)
	} else {
		small = pypil.Thumbnail(pypil.ToNRGBA(img), maxDim, hasAlpha)
	}
	newW, newH := small.Bounds().Dx(), small.Bounds().Dy()
	var buf bytes.Buffer
	if ext == ".jpg" {
		if hasAlpha || nearest && !isGray(small) {
			return nil, nil, errors.New("cannot write mode with alpha or palette as JPEG")
		}
		err = jpeg.Encode(&buf, small, &jpeg.Options{Quality: 75})
	} else {
		err = png.Encode(&buf, small)
	}
	if err != nil {
		return nil, nil, err
	}
	fx, fy := 1.0, 1.0
	if newW != 0 {
		fx = float64(origW) / float64(newW)
	}
	if newH != 0 {
		fy = float64(origH) / float64(newH)
	}
	clause := fmt.Sprintf("multiply any coordinates you report by %.2f to map back to the real screen.", fx)
	if fmt.Sprintf("%.2f", fx) != fmt.Sprintf("%.2f", fy) {
		clause = fmt.Sprintf("multiply any x coordinates you report by %.2f and any y coordinates by %.2f to map back to the real screen.", fx, fy)
	}
	note := fmt.Sprintf("Screenshot downscaled from %dx%d to %dx%d for vision; %s", origW, origH, newW, newH, clause)
	return buf.Bytes(), &note, nil
}

// True for a single channel grey image, which JPEG accepts as Pillow mode one
func isGray(img image.Image) bool {
	_, ok := img.(*image.Gray)
	return ok
}

// True when the main model cannot take tool result pixels natively, so captures go to auxiliary vision
var shouldRouteThroughAuxVision = func(ctx context.Context, env *tools.Env) (route bool) {
	defer func() {
		if rec := recover(); rec != nil {
			slog.Debug(fmt.Sprintf("computer_use: aux-vision routing decision failed: %v", rec))
			route = false
		}
	}()
	return !cuNativeVisionFastPath(ctx, env)
}

// Shared native tool result image gate applied to the session main model
var cuNativeVisionFastPath = tools.ShouldUseNativeVisionFastPath

// Vision analysis of an image path with a prompt, the JSON text of vision_analyze
var cuVisionAnalyze = tools.VisionAnalyzeTool

// Pre analyses the screenshot through auxiliary vision and merges it with the element summary, nil on failure
var routeCaptureThroughAuxVision = func(ctx context.Context, env *tools.Env, c *CaptureResult, summary string, visible []UIElement,
	truncated int, elementsFile, screenshotPath *string) *string {
	b64 := capturePNG(c)
	if b64 == "" {
		return nil
	}
	raw, err := pybase64.B64Decode(b64)
	if err != nil {
		slog.Debug(fmt.Sprintf("computer_use: failed to decode capture base64: %v", err))
		return nil
	}
	_, ext := captureImageFormat(c)
	tempPath, err := cacheFile(tools.WithEnvHome(ctx, env), env, "cache/vision", "temp_vision_images", "computer_use_"+uuid4Hex()+ext, "", 0)
	if err != nil {
		slog.Warn(fmt.Sprintf("computer_use: auxiliary.vision pre-analysis failed (%v); returning to caller without aux analysis", err))
		return nil
	}
	defer os.Remove(tempPath)
	raw, scaleNote := shrinkCaptureForVision(raw, ext, cuMaxVisionDim)
	if err := writePrivateBytes(tempPath, raw); err != nil {
		slog.Warn(fmt.Sprintf("computer_use: auxiliary.vision pre-analysis failed (%v); returning to caller without aux analysis", err))
		return nil
	}
	prompt := cuVisionPrompt + summary
	if scaleNote != nil {
		prompt += "\n\nNote: " + *scaleNote
	}
	analysis := pystr.Strip(cuVisionAnalyze(ctx, env, tempPath, prompt))
	if parsed, err := pyjson.Loads(analysis); err == nil {
		analysis = ""
		if obj, ok := parsed.(*pyjson.Object); ok {
			if a := obj.Vals["analysis"]; pyrepr.Truthy(a) {
				analysis = pystr.Strip(pyrepr.Str(a))
			}
		}
	}
	if analysis == "" {
		return nil
	}
	if visible == nil {
		visible = c.Elements
	}
	view := &captureView{cap: c, visible: visible, total: len(c.Elements), width: c.Width, height: c.Height,
		truncated: truncated, elementsFile: elementsFile, screenshotPath: screenshotPath}
	extra := pyjson.NewObject()
	extra.Set("vision_analysis", analysis)
	extra.Set("vision_analysis_routed_via", "auxiliary.vision")
	out := textCapturePayload(view, summary, extra)
	return &out
}

// Schema of the computer_use tool as the registry advertises it
func getComputerUseToolSchema() map[string]any {
	return GetComputerUseSchema()
}
