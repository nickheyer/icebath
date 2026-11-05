package tools

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	// DefaultExtractCharLimit is the per page character budget sent to the model
	DefaultExtractCharLimit = 15000
	// MaxStoredTextChars caps the full text file written to cache/web
	MaxStoredTextChars = 2_000_000
	charLimitFloor     = 2000
	charLimitCeiling   = 500_000
)

// Clamps to the sane range, false for non numeric input
func clampCharLimit(value any) (int, bool) {
	n, ok := pyIntValue(value)
	if !ok {
		return 0, false
	}
	if s, isString := value.(string); isString && !pyIntLiteralRe.MatchString(strings.TrimSpace(s)) {
		return 0, false
	}
	return max(charLimitFloor, min(n, charLimitCeiling)), true
}

// Clamped limit, nil or non numeric input giving the default
func clampOrDefault(value any) int {
	if value == nil {
		return DefaultExtractCharLimit
	}
	if n, ok := clampCharLimit(value); ok {
		return n
	}
	return DefaultExtractCharLimit
}

// web.extract_char_limit clamped, else the default
func extractCharLimit(env *Env) int {
	v, ok := webCfg(env).Lookup("extract_char_limit")
	if !ok {
		return DefaultExtractCharLimit
	}
	return clampOrDefault(v)
}

// Caller's limit clamped, else the configured one
func effectiveCharLimit(env *Env, charLimit any) int {
	if charLimit != nil {
		return clampOrDefault(charLimit)
	}
	return extractCharLimit(env)
}

var (
	base64MarkdownImageRe = regexp.MustCompile(`!\[([^\]]*)\]\(\s*data:image/[^;]+;base64,[A-Za-z0-9+/=\s]+\)`)
	base64ParenImageRe    = regexp.MustCompile(`\(\s*data:image/[^;]+;base64,[A-Za-z0-9+/=\s]+\)`)
	base64BareImageRe     = regexp.MustCompile(`data:image/[^;]+;base64,[A-Za-z0-9+/=]+`)
)

// ConvertBase64ImagesToLinks replaces inline base64 image blobs with [IMAGE: alt] placeholders
func ConvertBase64ImagesToLinks(text string) string {
	out := base64MarkdownImageRe.ReplaceAllStringFunc(text, func(m string) string {
		alt := strings.TrimSpace(base64MarkdownImageRe.FindStringSubmatch(m)[1])
		if alt != "" {
			return "[IMAGE: " + alt + "]"
		}
		return "[IMAGE]"
	})
	out = base64ParenImageRe.ReplaceAllString(out, "[IMAGE]")
	return base64BareImageRe.ReplaceAllString(out, "[IMAGE]")
}

// Writes the full page to cache/web and returns its path, empty when storing fails
func storeFullText(env *Env, u, content string) string {
	dir, err := webCacheDir(env)
	if err != nil {
		envLog(env).Debug("Failed to store full web_extract text", "url", u, "error", err)
		return ""
	}
	sum := sha256.Sum256([]byte(u))
	path := filepath.Join(dir, fmt.Sprintf("%s-%s.md", hostSlug(u), hex.EncodeToString(sum[:])[:10]))
	if n := len([]rune(content)); n > MaxStoredTextChars {
		content = string([]rune(content)[:MaxStoredTextChars]) + fmt.Sprintf("\n\n[... stored copy truncated at %s chars of %s; re-extract a more specific URL for the rest ...]",
			groupThousands(MaxStoredTextChars), groupThousands(n))
	}
	if err := WriteTextExclusive(path, content, false, true); err != nil {
		envLog(env).Debug("Failed to store full web_extract text", "url", u, "error", err)
		return ""
	}
	return path
}

// Decimal with thousands separators the way Python formats with a comma
func groupThousands(n int) string {
	s := fmt.Sprint(n)
	if len(s) <= 3 {
		return s
	}
	var parts []string
	for len(s) > 3 {
		parts = append([]string{s[len(s)-3:]}, parts...)
		s = s[:len(s)-3]
	}
	return s + "," + strings.Join(parts, ",")
}

// Head and tail window with a footer naming the stored copy, false when the page fits
func truncateWithFooter(env *Env, content, u string, charLimit int) (string, bool) {
	runes := []rune(content)
	if len(runes) <= charLimit {
		return content, false
	}
	headBudget := int(float64(charLimit) * 0.75)
	tailBudget := charLimit - headBudget
	head := string(runes[:headBudget])
	tail := string(runes[len(runes)-tailBudget:])
	if nl := strings.LastIndex(head, "\n"); float64(nl) > float64(headBudget)*0.5 {
		head = head[:nl]
	}
	if nl := strings.Index(tail, "\n"); nl >= 0 && float64(nl) < float64(tailBudget)*0.5 {
		tail = tail[nl+1:]
	}
	stored := storeFullText(env, u, content)
	if stored != "" {
		stored = ToAgentVisibleCachePath(env, stored)
	}
	footer := []string{"", strings.Repeat("─", 8) + " [TRUNCATED] " + strings.Repeat("─", 8),
		fmt.Sprintf("Showing %s chars (head) + %s chars (tail) of %s total clean characters.",
			groupThousands(len([]rune(head))), groupThousands(len([]rune(tail))), groupThousands(len(runes)))}
	if stored != "" {
		middleStart := strings.Count(head, "\n") + 2
		footer = append(footer, "Full text saved to: "+stored,
			fmt.Sprintf("To read the omitted middle: read_file path=\"%s\" offset=%d limit=200  (the file is the complete page; raise/lower offset to page through it).", stored, middleStart))
	} else {
		footer = append(footer, "Full text could not be stored; re-run web_extract on a more specific URL or open it in the browser for the complete page.")
	}
	footer = append(footer, strings.Repeat("─", 29))
	modelText := head + "\n\n[... middle omitted — see footer ...]\n\n" + tail
	return modelText + "\n" + strings.Join(footer, "\n"), true
}

var unambiguousBinaryKinds = []string{"SQLite", "ZIP", "gzip", "bzip2", "xz", "7-Zip", "ELF", "Mach-O", "PNG", "JPEG", "GIF", "TIFF", "FLAC", "Ogg"}

// Magic byte type name when a fetched body is a raw binary file, compared NUL stripped on both sides
func binaryPayloadKind(text string) string {
	runes := []rune(text)
	if len(runes) > 32 {
		runes = runes[:32]
	}
	var head []byte
	for _, r := range runes {
		if r == 0 {
			continue
		}
		if r <= 0xFF {
			head = append(head, byte(r))
		}
	}
	for _, sig := range FileMagicSignatures() {
		stripped := strings.ReplaceAll(string(sig.Prefix), "\x00", "")
		if stripped == "" || !strings.HasPrefix(string(head), stripped) {
			continue
		}
		for _, kind := range unambiguousBinaryKinds {
			if strings.HasPrefix(sig.Name, kind) {
				return sig.Name
			}
		}
	}
	return ""
}

// Replaces each successful entry's content with its cleaned, budgeted text in place, recording metrics
func truncateResults(env *Env, results []*WebPage, charLimit int, debug map[string]any) {
	log := envLog(env)
	for _, r := range results {
		raw := r.body()
		if r.Error != "" {
			if r.Content != "" {
				r.Content, _ = truncateWithFooter(env, ConvertBase64ImagesToLinks(r.Content), r.URL, charLimit)
			}
			continue
		}
		if raw == "" {
			continue
		}
		if kind := binaryPayloadKind(raw); kind != "" {
			r.Content = ""
			r.Error = fmt.Sprintf("URL returned binary content (%s), not a page. Download it with the terminal (curl -L -o) and use read_file (SQLite/Office/PDF auto-extract) or terminal utilities on the file.", kind)
			log.Info("binary payload dropped", "url", r.URL, "kind", kind, "chars", len([]rune(raw)))
			continue
		}
		clean := ConvertBase64ImagesToLinks(raw)
		modelText, truncated := truncateWithFooter(env, clean, r.URL, charLimit)
		r.Content = modelText
		if truncated {
			debug["pages_truncated"] = asIntValue0(debug["pages_truncated"]) + 1
			metrics, _ := debug["truncation_metrics"].([]any)
			debug["truncation_metrics"] = append(metrics, map[string]any{"url": r.URL, "original_size": len([]rune(clean)), "sent_size": len([]rune(modelText))})
			log.Info("page truncated", "url", r.URL, "from", len([]rune(clean)), "to", len([]rune(modelText)))
		} else {
			log.Info("page whole", "url", r.URL, "chars", len([]rune(clean)))
		}
	}
}

// Integer of a debug counter, zero when unset
func asIntValue0(v any) int {
	n, _ := pyIntValue(v)
	return n
}

// Trimmed result rows keeping url, title, content, error and blocked_by_policy
func trimResults(results []*WebPage) []any {
	out := make([]any, 0, len(results))
	for _, r := range results {
		row := map[string]any{"url": r.URL, "title": r.Title, "content": r.Content, "error": emptyAsNull(r.Error)}
		if r.BlockedByPolicy != nil {
			row["blocked_by_policy"] = r.BlockedByPolicy
		}
		out = append(out, row)
	}
	return out
}
