package gateway

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
)

// Image types the API server inlines as data URLs
var apiMediaMIME = map[string]string{".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".gif": "image/gif",
	".webp": "image/webp", ".bmp": "image/bmp"}

// Largest image the API server inlines
const apiMediaDataURLMaxBytes = 5 * 1024 * 1024

// Inline data URL markdown of a deliverable image path, empty when refused, too large or unreadable
func apiMediaToDataURL(ctx context.Context, pathStr string) string {
	safe, err := ValidateMediaDeliveryPath(ctx, pathStr, "")
	if err != nil || safe == "" {
		return ""
	}
	suffix := strings.ToLower(filepath.Ext(safe))
	mime, ok := apiMediaMIME[suffix]
	if !ok {
		return ""
	}
	info, err := os.Stat(safe)
	if err != nil || info.Size() > apiMediaDataURLMaxBytes {
		return ""
	}
	data, err := os.ReadFile(safe)
	if err != nil {
		return ""
	}
	return "![image](data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data) + ")"
}

// Media tags replaced by inline base64 data URLs, a trailing end of sequence token dropped only when a tag resolved
func APIResolveMediaToDataURLs(ctx context.Context, text string) (out string) {
	if text == "" || !strings.Contains(text, "MEDIA:") {
		return text
	}
	defer func() {
		if recover() != nil {
			out = text
		}
	}()
	runes := []rune(text)
	scan := runes
	if start := terminalSentinelStart(runes); start >= 0 {
		scan = runes[:start]
	}
	var b strings.Builder
	last := 0
	for _, m := range mediaTagCleanupRE.finditer(scan) {
		whole := m.GroupByNumber(0)
		start, end := whole.RuneIndex, whole.RuneIndex+whole.RuneLength
		b.WriteString(string(scan[last:start]))
		if replaced := apiMediaToDataURL(ctx, mediaTagCleanupRE.group(m, "path")); replaced != "" {
			b.WriteString(replaced)
		} else {
			b.WriteString(string(scan[start:end]))
		}
		last = end
	}
	b.WriteString(string(scan[last:]))
	resolved := b.String()
	if resolved == string(scan) {
		return text
	}
	return resolved
}
