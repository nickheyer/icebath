package agent

import (
	"fmt"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pybase64"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	imageShrinkTargetBytes         = 4 * 1024 * 1024
	imageShrinkDefaultMaxDimension = 8000
)

var imageSuffixByMIME = map[string]string{
	"image/png": ".png", "image/gif": ".gif", "image/webp": ".webp", "image/jpeg": ".jpg", "image/jpg": ".jpg",
	"image/bmp": ".bmp",
}

// Re-encodes the image file at path under the byte and long side budgets, the tools vision resizer in production
var imageShrinkResize = func(path, mime string, maxBase64Bytes, maxDimension int) (string, error) {
	return tools.ResizeImageForVision(nil, path, mime, tools.ResizeOptions{MaxBase64Bytes: maxBase64Bytes, MaxDimension: maxDimension})
}

// Pixel width and height of decoded image bytes, an error when the bytes are no readable image
var imageShrinkOpenSize = tools.RasterSize

// Image mime named by a data URL header, else the fallback
func dataURLMIME(header, fallback string) string {
	if strings.HasPrefix(header, "data:") {
		candidate := pystr.Strip(strings.SplitN(header[len("data:"):], ";", 2)[0])
		if strings.HasPrefix(candidate, "image/") {
			return candidate
		}
	}
	return fallback
}

// Width and height of a base64 data URL image, false when the payload does not decode
func decodePixels(dataURL string) (w, h int, ok bool) {
	defer func() {
		if recover() != nil {
			w, h, ok = 0, 0, false
		}
	}()
	_, data, _ := strings.Cut(dataURL, ",")
	if data == "" || !strings.HasPrefix(dataURL, "data:") {
		return 0, 0, false
	}
	raw, err := pybase64.B64Decode(data)
	if err != nil {
		return 0, 0, false
	}
	w, h, err = imageShrinkOpenSize(raw)
	if err != nil {
		return 0, 0, false
	}
	return w, h, true
}

// Rewritten data URL gated on the constraint that triggered the shrink, unshrinkable when resizing failed that constraint
func shrinkDataURL(value any, maxDimension int) (resized string, unshrinkable bool) {
	url, ok := value.(string)
	if !ok || !strings.HasPrefix(url, "data:") {
		return "", false
	}
	urlLen := utf8.RuneCountInString(url)
	triggeredByBytes := urlLen > imageShrinkTargetBytes
	if !triggeredByBytes {
		w, h, ok := decodePixels(url)
		if !ok || max(w, h) <= maxDimension {
			return "", false
		}
	}
	defer func() {
		if r := recover(); r != nil {
			compressionLog().Warn(fmt.Sprintf("image-shrink recovery: re-encode failed — %v", r))
			resized, unshrinkable = "", true
		}
	}()
	out, err := reencodeDataURL(url, maxDimension)
	if err != nil {
		compressionLog().Warn(fmt.Sprintf("image-shrink recovery: re-encode failed — %v", err))
		return "", true
	}
	if out == "" {
		return "", true
	}
	w, h, decoded := decodePixels(out)
	var accepted bool
	switch {
	case triggeredByBytes:
		accepted = utf8.RuneCountInString(out) < urlLen && (!decoded || max(w, h) <= maxDimension)
	case decoded:
		accepted = max(w, h) <= maxDimension
	default:
		accepted = utf8.RuneCountInString(out) < urlLen
	}
	if !accepted {
		return "", true
	}
	return out, false
}

// Writes the decoded payload to a temp file named for its mime and hands it to the resizer, removing the file after
func reencodeDataURL(url string, maxDimension int) (string, error) {
	header, data, _ := strings.Cut(url, ",")
	mime := dataURLMIME(header, "image/jpeg")
	raw, err := pybase64.B64Decode(data)
	if err != nil {
		return "", err
	}
	suffix, ok := imageSuffixByMIME[mime]
	if !ok {
		suffix = ".jpg"
	}
	tmp, err := os.CreateTemp("", "icebath_shrink_*"+suffix)
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	return imageShrinkResize(tmp.Name(), mime, imageShrinkTargetBytes, maxDimension)
}

// Data URL for an Anthropic base64 image source, empty for any other source
func sourceToDataURL(source any) string {
	src, ok := source.(map[string]any)
	if !ok || src["type"] != "base64" {
		return ""
	}
	data, ok := src["data"].(string)
	if !ok || data == "" {
		return ""
	}
	mediaType := "image/jpeg"
	if pyTruthy(src["media_type"]) {
		mediaType = pystr.Strip(pyrepr.Str(src["media_type"]))
	}
	if !strings.HasPrefix(mediaType, "image/") {
		mediaType = "image/jpeg"
	}
	return "data:" + mediaType + ";base64," + data
}

// New source carrying the re-encoded payload so the stored history keeps its original image
func writeDataURLToSource(source map[string]any, dataURL string) map[string]any {
	header, data, _ := strings.Cut(dataURL, ",")
	out := make(map[string]any, len(source)+3)
	for k, v := range source {
		out[k] = v
	}
	out["type"] = "base64"
	out["media_type"] = dataURLMIME(header, "image/jpeg")
	out["data"] = data
	return out
}

// Copy of a part with one key replaced, leaving the aliased original untouched
func withPartValue(part map[string]any, key string, value any) map[string]any {
	out := make(map[string]any, len(part)+1)
	for k, v := range part {
		out[k] = v
	}
	out[key] = value
	return out
}

// Replacement part for one oversized image part, with whether an oversized image could not be shrunk
func shrinkImagePart(part map[string]any, maxDimension int) (map[string]any, bool, bool) {
	switch ptype := part["type"]; ptype {
	case "image":
		source := part["source"]
		resized, unshrinkable := shrinkDataURL(sourceToDataURL(source), maxDimension)
		if src, ok := source.(map[string]any); ok && resized != "" {
			return withPartValue(part, "source", writeDataURLToSource(src, resized)), unshrinkable, true
		}
		return nil, unshrinkable, true
	case "image_url", "input_image":
		switch image := part["image_url"].(type) {
		case map[string]any:
			url, ok := image["url"]
			if !ok {
				url = ""
			}
			resized, unshrinkable := shrinkDataURL(url, maxDimension)
			if resized != "" {
				return withPartValue(part, "image_url", withPartValue(image, "url", resized)), unshrinkable, true
			}
			return nil, unshrinkable, true
		case string:
			resized, unshrinkable := shrinkDataURL(image, maxDimension)
			if resized != "" {
				return withPartValue(part, "image_url", resized), unshrinkable, true
			}
			return nil, unshrinkable, true
		}
	}
	return nil, false, false
}

// Replaced content list for one message, nil when no part changed, counting changed and unshrinkable parts
func shrinkContentParts[P any](content []P, maxDimension int, changed, unshrinkable *int) []P {
	var out []P
	for i, raw := range content {
		part, ok := any(raw).(map[string]any)
		if !ok {
			continue
		}
		replacement, stuck, considered := shrinkImagePart(part, maxDimension)
		if !considered {
			continue
		}
		if replacement != nil {
			if out == nil {
				out = append([]P(nil), content...)
			}
			out[i] = any(replacement).(P)
			*changed++
		} else if stuck {
			*unshrinkable++
		}
	}
	return out
}

// Re-encodes oversized native image parts in place for an image too large retry, false when a retry would resend a rejected image
func tryShrinkImagePartsInMessages(apiMessages any, maxDimension int) bool {
	var msgs []map[string]any
	switch list := apiMessages.(type) {
	case []any:
		for _, m := range list {
			if msg, ok := m.(map[string]any); ok {
				msgs = append(msgs, msg)
			}
		}
	case []map[string]any:
		msgs = list
	}
	if len(msgs) == 0 {
		return false
	}
	changedCount, unshrinkableOversized := 0, 0
	for _, msg := range msgs {
		switch content := msg["content"].(type) {
		case []any:
			if out := shrinkContentParts(content, maxDimension, &changedCount, &unshrinkableOversized); out != nil {
				msg["content"] = out
			}
		case []map[string]any:
			if out := shrinkContentParts(content, maxDimension, &changedCount, &unshrinkableOversized); out != nil {
				msg["content"] = out
			}
		}
	}
	targetMB := float64(imageShrinkTargetBytes) / (1024 * 1024)
	if changedCount > 0 {
		compressionLog().Info(fmt.Sprintf("image-shrink recovery: re-encoded %d image part(s) to fit under %.0f MB", changedCount, targetMB))
	}
	if unshrinkableOversized > 0 {
		compressionLog().Warn(fmt.Sprintf("image-shrink recovery: %d oversized image part(s) could not be shrunk under %.0f MB — not retrying (would re-send rejected payload)", unshrinkableOversized, targetMB))
		return false
	}
	return changedCount > 0
}
