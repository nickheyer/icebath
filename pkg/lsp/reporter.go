package lsp

import (
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Reporter caps on diagnostics per file, block size and attacker controlled fields
const (
	MaxPerFile      = 20
	MaxTotalChars   = 4000
	maxMessageChars = 300
	maxCodeChars    = 80
	maxSourceChars  = 80
)

var severityNames = map[float64]string{1: "ERROR", 2: "WARN", 3: "INFO", 4: "HINT"}

// Python html.escape
func htmlEscape(s string, quote bool) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	if quote {
		s = strings.ReplaceAll(s, `"`, "&quot;")
		s = strings.ReplaceAll(s, "'", "&#x27;")
	}
	return s
}

// Language server field made safe to embed, newlines collapsed, controls dropped, capped and escaped
func sanitizeField(v any, limit int, isPrintable func(rune) bool) string {
	if v == nil {
		return ""
	}
	raw := strings.ReplaceAll(strings.ReplaceAll(pyrepr.Str(v), "\r", " "), "\n", " ")
	var b strings.Builder
	for _, r := range raw {
		if r == ' ' || isPrintable(r) {
			b.WriteRune(r)
		}
	}
	return htmlEscape(runePrefix(pystr.Strip(b.String()), limit), false)
}

// Severity name of a value the way a dict lookup by number finds it, the TypeError for unhashable values
func severityName(v any) (string, error) {
	switch v.(type) {
	case []any, map[string]any:
		return "", &pyExc{typ: "TypeError", text: fmt.Sprintf("unhashable type: '%s'", pyrepr.TypeName(v))}
	}
	if n, ok := pyNumber(v); ok {
		if name, ok := severityNames[n]; ok {
			return name, nil
		}
	}
	return "ERROR", nil
}

// FormatDiagnostic renders one diagnostic on one line with sanitized fields and 1 based positions
func FormatDiagnostic(d map[string]any, isPrintable func(rune) bool) (string, error) {
	if d == nil {
		return "", attributeError(nil)
	}
	sev, err := severityName(pyOr(d["severity"], int64(1)))
	if err != nil {
		return "", err
	}
	rng, err := dictOr(d["range"])
	if err != nil {
		return "", err
	}
	start, err := dictOr(rng["start"])
	if err != nil {
		return "", err
	}
	line, err := pyInt(dgetOr(start, "line", int64(0)))
	if err != nil {
		return "", err
	}
	col, err := pyInt(dgetOr(start, "character", int64(0)))
	if err != nil {
		return "", err
	}
	msg := sanitizeField(d["message"], maxMessageChars, isPrintable)
	code := sanitizeField(d["code"], maxCodeChars, isPrintable)
	source := sanitizeField(d["source"], maxSourceChars, isPrintable)
	out := fmt.Sprintf("%s [%d:%d] %s", sev, line+1, col+1, msg)
	if code != "" {
		out += " [" + code + "]"
	}
	if source != "" {
		out += " (" + source + ")"
	}
	return out, nil
}

// ReportForFile builds the diagnostics block of a file, empty when no error severity diagnostic remains
func ReportForFile(filePath string, diagnostics []map[string]any, isPrintable func(rune) bool) (string, error) {
	var filtered []map[string]any
	for _, d := range diagnostics {
		if d == nil {
			return "", attributeError(nil)
		}
		sev := pyOr(d["severity"], int64(1))
		switch sev.(type) {
		case []any, map[string]any:
			return "", &pyExc{typ: "TypeError", text: fmt.Sprintf("unhashable type: '%s'", pyrepr.TypeName(sev))}
		}
		if n, ok := pyNumber(sev); ok && n == 1 {
			filtered = append(filtered, d)
		}
	}
	if len(filtered) == 0 {
		return "", nil
	}
	shown := filtered
	if len(shown) > MaxPerFile {
		shown = shown[:MaxPerFile]
	}
	lines := make([]string, len(shown))
	for i, d := range shown {
		line, err := FormatDiagnostic(d, isPrintable)
		if err != nil {
			return "", err
		}
		lines[i] = line
	}
	body := strings.Join(lines, "\n")
	if len(filtered) > MaxPerFile {
		body += fmt.Sprintf("\n... and %d more", len(filtered)-MaxPerFile)
	}
	return "<diagnostics file=\"" + htmlEscape(filePath, true) + "\">\n" + body + "\n</diagnostics>", nil
}
