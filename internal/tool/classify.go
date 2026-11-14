package tool

import (
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/dlclark/regexp2/v2"

	"github.com/nickheyer/icebath/internal/model/wire"
)

const (
	GuardrailRefusalKey     = "guardrail_refusal"
	CompressionMarkerPrefix = "⟪ICEBATH-CONTEXT-COMPRESSION:"
	errorSuffixMax          = 48
	degradedSuffixMax       = 200
)

var failureMarker = func() *regexp2.Regexp {
	re := regexp2.MustCompile(`"(?:error|failed)"(?!\s*:\s*(?:null|false|0|""|\[\]|\{\})\s*(?:[,}\]]|$))`, regexp2.IgnoreCase)
	re.MatchTimeout = 2 * time.Second
	return re
}()

var (
	rateLimitText      = regexp.MustCompile(`(?i)rate[\s_-]?limit|too many requests|\b429\b`)
	v4aFileHeader      = regexp.MustCompile(`(?m)^\*\*\*\s*(?:Update|Add|Delete)\s+File:\s*(.+)$`)
	v4aMoveHeader      = regexp.MustCompile(`(?m)^\*\*\*\s*Move\s+File:\s*(.+?)\s*->\s*(.+)$`)
	destructivePattern = regexp.MustCompile("(?:^|\\s|&&|\\|\\||;|`)(?:rm\\s|rmdir\\s|cp\\s|install\\s|mv\\s|sed\\s+-i|truncate\\s|dd\\s|shred\\s|git\\s+(?:reset|clean|checkout)\\s)")
	redirectOverwrite  = regexp.MustCompile(`[^>]>[^>]|^>[^>]`)
	compressionMarker  = regexp.MustCompile(regexp.QuoteMeta(CompressionMarkerPrefix) + `[\t\n\v\f\r \x{1c}-\x{1f}\x{85}\p{Z}]+\p{Nd}[\p{Nd},]*`)
)

func parseObject(text string) (wire.Value, bool) {
	v, err := wire.ParseValue([]byte(trimWide(text)))
	if err != nil || !v.IsObject() {
		return wire.Value{}, false
	}
	return v, true
}

func valueDisplay(v wire.Value) string {
	if s, ok := v.Str(); ok {
		return s
	}
	return v.Display()
}

func GuardrailRefusal(text string) bool {
	v, ok := parseObject(text)
	return ok && v.Get(GuardrailRefusalKey).True()
}

func HasFailureMarker(result string) bool {
	if strings.HasPrefix(result, "Error") {
		return true
	}
	head := runePrefix(result, 600)
	m, err := failureMarker.FindStringMatch(head)
	for err == nil && m != nil {
		if m.RuneIndex < 500 {
			return true
		}
		m, err = failureMarker.FindNextMatch(m)
	}
	if err != nil {
		slog.Warn(fmt.Sprintf("failure marker scan timed out; treating the result as failed: %v", err))
		return true
	}
	return false
}

func RateLimited(result string) bool {
	v, err := wire.ParseValue([]byte(trimWide(result)))
	if err != nil {
		return rateLimitText.MatchString(runePrefix(result, 500))
	}
	if !v.IsObject() {
		return false
	}
	for _, k := range []string{"retry_after", "retryAfter", "retry_after_seconds", "retry_after_ms"} {
		if v.Get(k).Truthy() {
			return true
		}
	}
	for _, k := range []string{"status", "status_code", "code"} {
		if f, ok := v.Get(k).Float(); ok && f == 429 && v.Get(k).Kind() == wire.KindNumber {
			return true
		}
	}
	e := v.Get("error")
	text := e
	if e.IsObject() {
		text = e.Get("message")
		if !text.Truthy() {
			text = e.Get("code")
		}
	}
	s, ok := text.Str()
	return ok && rateLimitText.MatchString(s)
}

func ShapeFailure(result string) (string, bool) {
	v, ok := parseObject(result)
	if !ok {
		return "", false
	}
	status := v.Get("status")
	if status.Exists() {
		if s := strings.ToLower(valueDisplay(status)); s == "failed" || s == "error" {
			return valueDisplay(status), true
		}
	}
	items := v.Get("results").Elems()
	if len(items) == 0 {
		return "", false
	}
	for _, item := range items {
		if !item.IsObject() || !item.Get("error").Truthy() {
			return "", false
		}
	}
	first := items[0].Get("error")
	if first.IsObject() {
		if msg := first.Get("message"); msg.Truthy() {
			return valueDisplay(msg), true
		}
		return first.Display(), true
	}
	return valueDisplay(first), true
}

func MutationLanded(tool, result string) bool {
	if tool != "write_file" && tool != "patch" {
		return false
	}
	v, ok := parseObject(result)
	if !ok || v.Get("error").Truthy() {
		return false
	}
	if tool == "write_file" {
		return v.Get("bytes_written").Exists()
	}
	return v.Get("success").True()
}

func englishFailure(key string, args map[string]string) string {
	text := map[string]string{
		"file_not_found":               "File not found: {name}",
		"terminal_backend_unavailable": "terminal backend unavailable",
		"exit_code":                    " [exit {code}]",
		"memory_full":                  " [full]",
		"generic":                      " [error]",
	}[key]
	for k, v := range args {
		text = strings.ReplaceAll(text, "{"+k+"}", v)
	}
	return text
}

func failureMessage(translate func(string, map[string]string) string, key string, args map[string]string) string {
	if translate != nil {
		return translate("display.failure."+key, args)
	}
	return englishFailure(key, args)
}

func tailTrunc(text string, limit int) string {
	if limit <= 0 || utf8.RuneCountInString(text) <= limit {
		return text
	}
	if limit <= 3 {
		return strings.Repeat(".", limit)
	}
	return runePrefix(text, limit-3) + "..."
}

func trimError(msg string, translate func(string, map[string]string) string) string {
	msg = trimWide(msg)
	if _, tail, found := strings.Cut(msg, "File not found:"); found {
		tail = trimWide(tail)
		if i := strings.LastIndex(tail, "/"); i >= 0 {
			msg = failureMessage(translate, "file_not_found", map[string]string{"name": tail[i+1:]})
		}
	}
	return tailTrunc(msg, errorSuffixMax)
}

func DetectFailure(tool string, r Result, translate func(key string, args map[string]string) string) (bool, string) {
	if r.Parts != nil || MutationLanded(tool, r.Text) {
		return false, ""
	}
	v, err := wire.ParseValue([]byte(trimWide(r.Text)))
	object := err == nil && v.IsObject()
	if object && v.Get(GuardrailRefusalKey).True() {
		return false, ""
	}
	if object && v.Get("user_summary").Truthy() {
		return true, " [" + tailTrunc(valueDisplay(v.Get("user_summary")), degradedSuffixMax) + "]"
	}
	if tool == "terminal" {
		if !object {
			return false, ""
		}
		code := v.Get("exit_code")
		zero := !code.Exists()
		switch code.Kind() {
		case wire.KindNull:
			zero = true
		case wire.KindNumber:
			f, _ := code.Float()
			zero = f == 0
		case wire.KindBool:
			zero = code.False()
		}
		if zero {
			return false, ""
		}
		if s, ok := v.Get("status").Str(); ok && s == "degraded" {
			reason := v.Get("reason")
			if !reason.Truthy() {
				reason = v.Get("error")
			}
			text := failureMessage(translate, "terminal_backend_unavailable", nil)
			if reason.Truthy() {
				text = valueDisplay(reason)
			}
			text = trimWide(text)
			if hint := trimWide(valueDisplay(v.Get("retry_hint"))); v.Get("retry_hint").Truthy() && hint != "" {
				text = text + " — " + hint
			}
			return true, " [" + tailTrunc(text, degradedSuffixMax) + "]"
		}
		if e := v.Get("error"); e.Truthy() {
			return true, " [" + trimError(valueDisplay(e), translate) + "]"
		}
		return true, failureMessage(translate, "exit_code", map[string]string{"code": valueDisplay(code)})
	}
	if object {
		failed := v.Get("success").False()
		if tool == "memory" && failed {
			if s, ok := v.Get("error").Str(); ok && strings.Contains(s, "exceed the limit") {
				return true, failureMessage(translate, "memory_full", nil)
			}
		}
		e := v.Get("error")
		if !e.Truthy() {
			e = v.Get("message")
		}
		if e.Truthy() && (failed || v.Get("error").Exists()) {
			return true, " [" + trimError(valueDisplay(e), translate) + "]"
		}
		if shape, ok := ShapeFailure(r.Text); ok {
			return true, " [" + trimError(shape, translate) + "]"
		}
		return false, ""
	}
	if HasFailureMarker(r.Text) {
		return true, failureMessage(translate, "generic", nil)
	}
	return false, ""
}

func argText(v any) (string, bool) {
	switch t := v.(type) {
	case nil:
		return "", false
	case string:
		return t, t != ""
	case bool:
		if !t {
			return "", false
		}
	}
	n := nodeOf(v)
	if !n.truthy() {
		return "", false
	}
	if s, ok := n.text(); ok {
		return s, true
	}
	return string(n.compact()), true
}

func MutationTargets(tool string, args Args) []string {
	if tool != "write_file" && tool != "patch" {
		return nil
	}
	mode := "replace"
	if tool == "patch" {
		if m, ok := argText(args["mode"]); ok {
			mode = m
		}
	}
	if mode == "replace" {
		if p, ok := argText(args["path"]); ok {
			return []string{p}
		}
		return nil
	}
	if mode != "patch" {
		return nil
	}
	body, ok := args["patch"].(string)
	if !ok || body == "" {
		return nil
	}
	var paths []string
	for _, m := range v4aFileHeader.FindAllStringSubmatch(body, -1) {
		paths = append(paths, trimWide(m[1]))
	}
	for _, m := range v4aMoveHeader.FindAllStringSubmatch(body, -1) {
		paths = append(paths, trimWide(m[1]), trimWide(m[2]))
	}
	return slices.DeleteFunc(paths, func(p string) bool { return p == "" })
}

func LandedMutationPaths(tool string, args Args, result string) []string {
	targets := MutationTargets(tool, args)
	if tool != "write_file" && tool != "patch" {
		return targets
	}
	v, ok := parseObject(result)
	if !ok {
		return targets
	}
	var landed []string
	for _, p := range v.Get("files_modified").Elems() {
		if p.Truthy() {
			landed = append(landed, valueDisplay(p))
		}
	}
	if len(landed) > 0 {
		return landed
	}
	if resolved := v.Get("resolved_path"); resolved.Truthy() {
		return []string{valueDisplay(resolved)}
	}
	return targets
}

func ErrorPreview(r Result, maxLen int) string {
	text := r.Plain()
	if stripped := trimWide(text); strings.HasPrefix(stripped, "{") {
		if v, err := wire.ParseValue([]byte(stripped)); err == nil && v.IsObject() {
			if s, ok := v.Get("error").Str(); ok {
				text = s
			}
		}
	}
	text = strings.Join(wideFields(text), " ")
	if utf8.RuneCountInString(text) > maxLen {
		text = runePrefix(text, maxLen-1) + "…"
	}
	return text
}

func AppendHint(r Result, hint string) Result {
	if r.Parts == nil {
		return r
	}
	parts := slices.Clone(r.Parts)
	appended := false
	for i := range parts {
		if parts[i].Kind == wire.PartText {
			parts[i].Text += hint
			appended = true
			break
		}
	}
	if !appended {
		parts = append([]wire.Part{{Kind: wire.PartText, Text: hint}}, parts...)
	}
	r.Parts = parts
	if r.Summary != "" {
		r.Summary += hint
	}
	return r
}

func DestructiveCommand(cmd string) bool {
	return cmd != "" && (destructivePattern.MatchString(cmd) || redirectOverwrite.MatchString(cmd))
}

func identifierKey(key string) bool {
	for i, r := range key {
		if r == '_' || unicode.IsLetter(r) || unicode.Is(unicode.Nl, r) {
			continue
		}
		if i > 0 && (unicode.IsDigit(r) || unicode.In(r, unicode.Mn, unicode.Mc, unicode.Pc)) {
			continue
		}
		return false
	}
	return key != ""
}

func PrunedArgumentPaths(effects Effects, args Args) []string {
	if effects&NoEffect != 0 {
		return nil
	}
	var found []string
	var walk func(v any, path string)
	walk = func(v any, path string) {
		switch t := v.(type) {
		case string:
			if compressionMarker.MatchString(t) {
				found = append(found, path)
			}
		case map[string]any:
			keys := make([]string, 0, len(t))
			for k := range t {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				if identifierKey(k) {
					walk(t[k], path+"."+k)
				} else {
					walk(t[k], path+"["+string(wire.Quote(k))+"]")
				}
			}
		case Args:
			walk(map[string]any(t), path)
		case []any:
			for i, item := range t {
				walk(item, fmt.Sprintf("%s[%d]", path, i))
			}
		}
	}
	walk(map[string]any(args), "$")
	return found
}
