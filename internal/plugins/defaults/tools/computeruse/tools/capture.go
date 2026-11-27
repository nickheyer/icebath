package tools

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	"image/jpeg"
	"image/png"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	_ "golang.org/x/image/bmp"
	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"

	"github.com/nickheyer/icebath/internal/auxiliary"
	auxiliaryconfig "github.com/nickheyer/icebath/internal/auxiliary/config"
	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/computeruse/cua"
	"github.com/nickheyer/icebath/internal/tools"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
	"github.com/nickheyer/icebath/pkg/strs"
)

const (
	// Smallest side of a screenshot providers take
	minImageSide = 8
	// Characters of a label the result shows
	maxLabel = 120
	// Files of each cache kept, the oldest pruned
	maxCacheFiles = 20
	// Longest side of a screenshot the vision model is sent
	maxVisionSide = 1456
)

const visionPrompt = "Describe what is visible in this desktop application screenshot in concise but specific terms. Mention the app name and window " +
	"title if visible, the overall layout, any labelled buttons, menus or text fields, and any prominent text content the user would need to know " +
	"about. Do not invent details that are not actually visible.\n\nAX/SOM index for cross-reference:\n"

// Type of a capture's screenshot, the driver's or sniffed from its start, and the extension its file takes
func format(c *cua.Capture) (string, string) {
	mime := c.Mime
	if mime == "" {
		mime = "image/png"
		if strings.HasPrefix(c.Image, "/9j/") {
			mime = "image/jpeg"
		}
	}
	if strings.EqualFold(mime, "image/jpeg") {
		return mime, ".jpg"
	}
	return mime, ".png"
}

func unknownBounds(b [4]int) bool { return b == [4]int{} }

// What a result shows of an element: its label capped, unknown bounds as none
func element(e cua.Element) map[string]any {
	out := map[string]any{"index": e.Index, "role": e.Role, "label": strs.Head(e.Label, maxLabel), "app": e.App, "bounds": nil}
	if !unknownBounds(e.Bounds) {
		out["bounds"] = e.Bounds[:]
	}
	if utf8.RuneCountInString(e.Label) > maxLabel {
		out["label_truncated"] = true
	}
	return out
}

// A line of the summary for each element up to the most, then how many more there are
func lines(elements []cua.Element, most int) []string {
	var out []string
	for i, e := range elements {
		if i >= most {
			out = append(out, fmt.Sprintf("  ... +%d more (call capture with app= to narrow)", len(elements)-most))
			break
		}
		line := fmt.Sprintf("  #%d %s '%s' ", e.Index, e.Role, strs.Head(strings.ReplaceAll(e.Label, "\n", " "), 60))
		if unknownBounds(e.Bounds) {
			line += "@ bounds-unknown (click by element index)"
		} else {
			line += fmt.Sprintf("@ (%d, %d, %d, %d)", e.Bounds[0], e.Bounds[1], e.Bounds[2], e.Bounds[3])
		}
		if e.App != "" {
			line += " [" + e.App + "]"
		}
		out = append(out, line)
	}
	return out
}

// Scale and note when the element bounds reach more than five percent past the screenshot, being in the screen's own pixels
func boundsHint(elements []cua.Element, width, height int) (float64, string) {
	if len(elements) == 0 || width <= 0 || height <= 0 {
		return 0, ""
	}
	maxX, maxY := 0, 0
	for _, e := range elements {
		maxX, maxY = max(maxX, e.Bounds[0]+e.Bounds[2]), max(maxY, e.Bounds[1]+e.Bounds[3])
	}
	if float64(maxX) <= float64(width)*1.05 && float64(maxY) <= float64(height)*1.05 {
		return 0, ""
	}
	note := fmt.Sprintf("element bounds are in native desktop coordinates (extend to ~%dx%d), NOT screenshot pixels (%dx%d). coordinate= clicks "+
		"expect the native space — derive click points from element bounds, or scale screenshot positions up accordingly", maxX, maxY, width, height)
	return math.Round(max(float64(maxX)/float64(width), float64(maxY)/float64(height))*100) / 100, note
}

// Float as Python prints it, whole ones with .0
func pyFloat(f float64) string {
	text := strconv.FormatFloat(f, 'f', -1, 64)
	if !strings.ContainsAny(text, ".e") {
		text += ".0"
	}
	return text
}

// A capture as the model is handed it: the summary, the fields of its JSON form, and the screenshot itself when the model sees it
type view struct {
	summary string
	fields  map[string]any
	media   *sessionsv1.Media
}

// The view's result, the screenshot handed to the model beside the summary when it sees it
func (v *view) render(tc *tools.Call) string {
	if v.media != nil {
		tc.Media = append(tc.Media, v.media)
		return v.summary
	}
	return tools.JSON(v.fields)
}

// Result of a capture: what it shows, an unchanged screen left out, the screenshot handed to a model that sees it or described by the
// vision model to one that does not
func (r *registrant) respond(ctx context.Context, tc *tools.Call, c *cua.Capture, most int) *view {
	cfg := tc.Env.Config
	visible := c.Elements[:min(most, len(c.Elements))]
	var raw []byte
	width, height := c.Width, c.Height
	if c.Image != "" {
		if decoded, err := base64.StdEncoding.DecodeString(c.Image); err == nil {
			raw = decoded
			if conf, _, err := image.DecodeConfig(bytes.NewReader(decoded)); err == nil && conf.Width > 0 && conf.Height > 0 {
				width, height = conf.Width, conf.Height
			}
		}
	}
	scale, boundsNote := boundsHint(visible, width, height)
	lost := len(c.Elements) > len(visible) || slices.ContainsFunc(visible, func(e cua.Element) bool { return utf8.RuneCountInString(e.Label) > maxLabel })
	tooSmall := raw != nil && min(width, height) < minImageSide
	withImage := c.Image != "" && c.Mode != "ax" && !tooSmall
	capped := c.AXMax > 0 && len(c.Elements) >= c.AXMax
	elementsFile, screenshotPath := "", ""
	if lost {
		elementsFile = r.spillElements(cfg, tc.Env, c)
	}
	if withImage {
		screenshotPath = r.persist(cfg, tc.Env, c)
	}
	var notes []string
	if boundsNote != "" {
		if scale != 0 {
			boundsNote += fmt.Sprintf("; estimated scale ~%sx (screenshot position x %s ≈ native coordinate)", pyFloat(scale), pyFloat(scale))
		}
		notes = append(notes, boundsNote)
	}
	if screenshotPath != "" {
		notes = append(notes, "shareable screenshot saved to "+screenshotPath)
	}
	if c.Note != "" {
		notes = append(notes, c.Note)
	}
	if elementsFile != "" {
		full := "full "
		if capped {
			full = ""
		}
		notes = append(notes, full+"element tree with untruncated labels saved to "+elementsFile+" — read_file/search_files it if you need dropped label "+
			"text or elements beyond the cap")
	}
	if capped {
		notes = append(notes, fmt.Sprintf("accessibility walk capped at %d elements; pass app= to narrow", c.AXMax))
	}
	head := fmt.Sprintf("capture mode=%s %dx%d", c.Mode, width, height)
	if c.App != "" {
		head += " app=" + c.App
	}
	if c.Title != "" {
		head += " window='" + c.Title + "'"
	}
	summary := []string{head, fmt.Sprintf("%d interactable element(s):", len(c.Elements))}
	for _, n := range notes {
		summary = append(summary, "  ("+n+")")
	}
	summary = append(summary, lines(visible, 40)...)
	if tooSmall {
		summary = append(summary, fmt.Sprintf("  (screenshot omitted: %dx%d is below the %dx%d provider minimum)", width, height, minImageSide, minImageSide))
	}
	elements := make([]any, len(visible))
	for i, e := range visible {
		elements[i] = element(e)
	}
	fields := map[string]any{"mode": c.Mode, "width": width, "height": height, "app": c.App, "window_title": c.Title, "elements": elements,
		"total_elements": len(c.Elements)}
	for key, value := range map[string]string{"elements_file": elementsFile, "screenshot_path": screenshotPath} {
		if value != "" {
			fields[key] = value
		}
	}
	if scale != 0 {
		fields["bounds_scale"] = scale
	}
	if truncated := len(c.Elements) - len(visible); truncated != 0 {
		fields["truncated_elements"] = truncated
	}
	mime, _ := format(c)
	switch {
	case withImage && tc.SessionID != "" && r.unchanged(tc.SessionID, digest(c), [2]string{c.App, c.Title}):
		summary = append(summary, "  (screen unchanged since the previous capture — image omitted to save context; the previous capture's "+
			"screenshot/analysis still shows the current state. Element indices below are fresh and remain the preferred way to act.)")
		fields["screen_unchanged"] = true
	case withImage && (tc.Env.Aux == nil || tc.Env.Aux.NativeImages(ctx)):
		return &view{summary: strings.Join(summary, "\n"), fields: fields, media: &sessionsv1.Media{MimeType: mime, Data: raw}}
	case withImage:
		if analysis := r.describe(ctx, tc, raw, c, strings.Join(summary, "\n")); analysis != "" {
			fields["summary"], fields["vision_analysis"], fields["vision_analysis_routed_via"] = strings.Join(summary, "\n"), analysis, "auxiliary.vision"
			return &view{summary: strings.Join(summary, "\n"), fields: fields}
		}
		summary = append(summary, "  (vision unavailable: the auxiliary vision model could not be reached; screenshot omitted. Element-index actions "+
			"still work — drive via the element list above.)")
		fields["vision_unavailable"] = true
	}
	if truncated := len(c.Elements) - len(visible); truncated != 0 {
		summary = append(summary, fmt.Sprintf("  (response truncated to %d of %d elements; the full tree is in elements_file — read_file/search_files it, "+
			"or pass app= to narrow scope)", len(visible), len(c.Elements)))
	}
	fields["summary"] = strings.Join(summary, "\n")
	return &view{summary: fields["summary"].(string), fields: fields}
}

// Digest of a screenshot and its type, telling one frame from another
func digest(c *cua.Capture) string {
	sum := sha256.Sum256([]byte(c.Mime + ":" + c.Image))
	return hex.EncodeToString(sum[:])
}

// Random id of 32 hex digits
func hexID() string { return strings.ReplaceAll(uuid.NewString(), "-", "") }

// New file of a cache folder of the profile, readable by its owner alone, the oldest files of the pattern pruned to make room
func cacheFile(cfg *config.Config, sub, name, pattern string) (string, error) {
	dir := filepath.Join(cfg.ProfileDir(cfg.Profile()), filepath.FromSlash(sub))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	os.Chmod(dir, 0o700)
	if pattern != "" {
		prune(dir, pattern, maxCacheFiles)
	}
	return filepath.Join(dir, name), nil
}

// Removes the oldest files of the pattern until one more fits under the cap
func prune(dir, pattern string, limit int) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type stamped struct {
		path string
		at   int64
	}
	var files []stamped
	for _, e := range entries {
		if !tools.Glob(e.Name(), pattern) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			return
		}
		files = append(files, stamped{filepath.Join(dir, e.Name()), info.ModTime().UnixNano()})
	}
	slices.SortStableFunc(files, func(a, b stamped) int { return int(a.at - b.at) })
	for _, f := range files[:max(0, len(files)-(limit-1))] {
		if err := os.Remove(f.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return
		}
	}
}

// Path a cache file is named by to the session's commands
func visible(env *tools.Env, path string) string {
	if env.Visible != nil {
		return env.Visible(path)
	}
	return path
}

// Copy of the screenshot among the profile's images, for surfaces that attach it; empty when it could not be saved
func (r *registrant) persist(cfg *config.Config, env *tools.Env, c *cua.Capture) string {
	_, ext := format(c)
	path, err := cacheFile(cfg, "cache/images", "computer_use_"+hexID()+ext, "computer_use_*.*")
	if err == nil {
		var raw []byte
		if raw, err = base64.StdEncoding.DecodeString(c.Image); err == nil {
			err = os.WriteFile(path, raw, 0o600)
		}
	}
	if err != nil {
		slog.Debug(fmt.Sprintf("computer_use: screenshot persistence failed: %v", err))
		return ""
	}
	return visible(env, path)
}

// The whole element tree with untruncated labels in a file read_file and search_files can open; empty when it could not be written
func (r *registrant) spillElements(cfg *config.Config, env *tools.Env, c *cua.Capture) string {
	elements := make([]map[string]any, len(c.Elements))
	for i, e := range c.Elements {
		elements[i] = map[string]any{"index": e.Index, "role": e.Role, "label": e.Label, "bounds": e.Bounds[:], "app": e.App}
	}
	path, err := cacheFile(cfg, "cache/computer_use", "elements_"+hexID()+".json", "elements_*.json")
	if err == nil {
		var text []byte
		if text, err = json.MarshalIndent(map[string]any{"app": c.App, "window_title": c.Title, "total_elements": len(c.Elements), "elements": elements},
			"", " "); err == nil {
			err = os.WriteFile(path, text, 0o600)
		}
	}
	if err != nil {
		slog.Debug(fmt.Sprintf("computer_use: element spill failed: %v", err))
		return ""
	}
	return visible(env, path)
}

// Screenshot shrunk to fit the longest side, with how to map positions back; the bytes as they are when they fit or can't be decoded
func shrink(raw []byte, ext string) ([]byte, string) {
	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		slog.Debug(fmt.Sprintf("computer_use: vision downscale skipped: decode capture image: %v", err))
		return raw, ""
	}
	w, h := img.Bounds().Dx(), img.Bounds().Dy()
	if max(w, h) <= maxVisionSide {
		return raw, ""
	}
	ratio := float64(maxVisionSide) / float64(max(w, h))
	nw, nh := max(1, int(math.Round(float64(w)*ratio))), max(1, int(math.Round(float64(h)*ratio)))
	small := image.NewNRGBA(image.Rect(0, 0, nw, nh))
	scaler := draw.Interpolator(draw.CatmullRom)
	if _, paletted := img.(*image.Paletted); paletted {
		scaler = draw.NearestNeighbor
	}
	scaler.Scale(small, small.Bounds(), img, img.Bounds(), draw.Src, nil)
	var buf bytes.Buffer
	if ext == ".jpg" {
		err = jpeg.Encode(&buf, small, &jpeg.Options{Quality: 75})
	} else {
		err = png.Encode(&buf, small)
	}
	if err != nil {
		slog.Debug(fmt.Sprintf("computer_use: vision downscale skipped: %v", err))
		return raw, ""
	}
	fx, fy := float64(w)/float64(nw), float64(h)/float64(nh)
	clause := fmt.Sprintf("multiply any coordinates you report by %.2f to map back to the real screen.", fx)
	if fmt.Sprintf("%.2f", fx) != fmt.Sprintf("%.2f", fy) {
		clause = fmt.Sprintf("multiply any x coordinates you report by %.2f and any y coordinates by %.2f to map back to the real screen.", fx, fy)
	}
	return buf.Bytes(), fmt.Sprintf("Screenshot downscaled from %dx%d to %dx%d for vision; %s", w, h, nw, nh, clause)
}

// What the vision model sees in the screenshot, for a main model that can't look itself; empty when it could not be asked
func (r *registrant) describe(ctx context.Context, tc *tools.Call, raw []byte, c *cua.Capture, summary string) string {
	if raw == nil || tc.Env.Aux == nil {
		return ""
	}
	mime, ext := format(c)
	raw, note := shrink(raw, ext)
	prompt := visionPrompt + summary
	if note != "" {
		prompt += "\n\nNote: " + note
	}
	temperature := auxiliary.VisionTemperature(tc.Env.Config)
	analysis, err := tc.Env.Aux.Complete(ctx, auxiliary.Request{Task: auxiliaryconfig.Vision, Messages: auxiliary.Prompt("", prompt,
		&sessionsv1.Media{MimeType: mime, Data: raw}), Temperature: &temperature})
	if err != nil {
		slog.Warn(fmt.Sprintf("computer_use: auxiliary.vision pre-analysis failed (%v); returning to caller without aux analysis", err))
		return ""
	}
	return strings.TrimSpace(analysis)
}
