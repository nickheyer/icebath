package camofox

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/dlclark/regexp2/v2"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/tools"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	regexutil "github.com/nickheyer/icebath/pkg/utils/regex"
	textutil "github.com/nickheyer/icebath/pkg/utils/text"
	urlutil "github.com/nickheyer/icebath/pkg/utils/url"
)

const (
	defaultSnapshotThreshold = 15000
	minSnapshotThreshold     = 1000
	maxStoredSnapshotChars   = 2_000_000
	redactWalkMaxDepth       = 320
	redactWalkDepthMarker    = "[redacted: nesting limit]"
)

// Error result dict with success false, the extra keys after the error
func policyErr(message string, extra ...any) *jsonutil.Object {
	return jsonutil.ObjectOf(append([]any{"success", false, "error", message}, extra...)...)
}

// browser key of the profile config, def on a miss or an unreadable config
func (s *scope) browserCfg(key string, def any) any {
	raw, err := s.rawConfig()
	if err != nil {
		pkgLog().Debug(fmt.Sprintf("Could not read browser.%s: %v", key, err))
		return def
	}
	return cfgGet(raw, def, "browser", key)
}

// Force redacted browser originated text, tool output being a model boundary
func (s *scope) redactText(text string) string {
	return tools.RedactSensitiveTextWith(s.env, text, tools.RedactOptions{Force: true})
}

// Force redacted value of any type, nil kept and other values redacted as their text
func (s *scope) redactAny(v any) any {
	switch t := v.(type) {
	case nil:
		return nil
	case string:
		return s.redactText(t)
	}
	return s.redactText(anyutil.String(v))
}

// Force redacted nested browser data, a tree past the walk limit replaced by the marker
func (s *scope) redactOutput(value any, depth int) any {
	if depth >= redactWalkMaxDepth {
		return redactWalkDepthMarker
	}
	switch t := value.(type) {
	case string:
		return s.redactText(t)
	case []any:
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = s.redactOutput(item, depth+1)
		}
		return out
	case *jsonutil.Object:
		if t == nil {
			return value
		}
		out := jsonutil.NewObject()
		for _, k := range t.Keys() {
			out.Set(anyutil.String(s.redactOutput(k, depth+1)), s.redactOutput(t.Value(k), depth+1))
		}
		return out
	}
	return value
}

// Every occurrence of a secret looking typed value replaced by its redacted form
func (s *scope) redactTypedText(value any, typed string) any {
	needle := typed
	redacted := needle
	if needle != "" {
		redacted = s.redactText(needle)
	}
	if redacted == needle {
		return value
	}
	return replaceTyped(value, needle, redacted)
}

// Value with every string inside it carrying needle replaced
func replaceTyped(value any, needle, redacted string) any {
	switch t := value.(type) {
	case string:
		return strings.ReplaceAll(t, needle, redacted)
	case *jsonutil.Object:
		if t == nil {
			return value
		}
		out := jsonutil.NewObject()
		for _, k := range t.Keys() {
			out.Set(k, replaceTyped(t.Value(k), needle, redacted))
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = replaceTyped(item, needle, redacted)
		}
		return out
	}
	return value
}

// Refusal of a URL embedding an API key or token in raw or percent decoded form
func secretURLError(raw string) *jsonutil.Object {
	if tools.RedactPrefixSearch(raw) || tools.RedactPrefixSearch(urlutil.Unescape(raw)) {
		return policyErr("Blocked: URL contains what appears to be an API key or token. Secrets must not be sent in URLs.")
	}
	return nil
}

// Normalized navigate target and the refusal of a secret bearing, metadata or blocklisted URL
func (s *scope) navigationPolicyError(raw string) (string, *jsonutil.Object) {
	if err := secretURLError(raw); err != nil {
		return raw, err
	}
	raw = tools.NormalizeURLForRequest(raw)
	if err := secretURLError(raw); err != nil {
		return raw, err
	}
	if tools.IsAlwaysBlockedURL(s.env, raw) {
		return raw, policyErr("Blocked: URL targets a cloud metadata endpoint")
	}
	if blocked, _ := tools.CheckWebsiteAccess(s.env, raw, ""); blocked != nil {
		return raw, policyErr(blocked.Message, "blocked_by_policy",
			jsonutil.ObjectOf("host", blocked.Host, "rule", blocked.Rule, "source", blocked.Source))
	}
	return raw, nil
}

// One sensitive JavaScript primitive pattern with the reason it names
type riskyPattern struct {
	re     *regexp2.Regexp
	reason string
}

var riskyEvalPatterns = []riskyPattern{
	{regexutil.MustCompile(`(?i)\bdocument\s*\.\s*cookie\b`), "document.cookie"},
	{regexutil.MustCompile(`(?i)\b(?:localStorage|sessionStorage)\b`), "web storage"},
	{regexutil.MustCompile(`(?i)\bindexedDB\b`), "IndexedDB"},
	{regexutil.MustCompile(`(?i)\bcaches\s*\.\s*(?:open|match|keys)\b`), "Cache Storage"},
	{regexutil.MustCompile(`(?i)\bnavigator\s*\.\s*(?:clipboard|credentials|serviceWorker)\b`), "navigator sensitive API"},
	{regexutil.MustCompile(`(?i)\b(?:fetch|XMLHttpRequest|WebSocket|EventSource)\s*\(`), "network request"},
	{regexutil.MustCompile(`(?i)\bnavigator\s*\.\s*sendBeacon\s*\(`), "network beacon"},
	{regexutil.MustCompile(`(?is)\bdocument\s*\.\s*forms\b.*\bvalue\b`), "form value extraction"},
	{regexutil.MustCompile(`(?is)\bquerySelector(?:All)?\s*\([^)]*(?:input|textarea|password)[^)]*\).*\bvalue\b`), "form value extraction"},
}

var jsStringLiteralRE = regexutil.MustCompile("(?s)'(?:\\\\.|[^'\\\\])*'|\"(?:\\\\.|[^\"\\\\])*\"|`(?:\\\\.|[^`\\\\])*`")

// Token a decoded string literal or a bare word may spell, with its pattern and reason
type sensitiveToken struct {
	token  string
	word   *regexp2.Regexp
	reason string
}

// Sensitive token with its case insensitive whole word pattern
func newSensitiveToken(token, reason string) sensitiveToken {
	return sensitiveToken{token: token, word: regexutil.MustCompile(`(?i)\b` + token + `\b`), reason: reason}
}

var sensitiveEvalTokens = []sensitiveToken{
	newSensitiveToken("cookie", "document.cookie"),
	newSensitiveToken("localStorage", "web storage"), newSensitiveToken("sessionStorage", "web storage"),
	newSensitiveToken("indexedDB", "IndexedDB"), newSensitiveToken("caches", "Cache Storage"),
	newSensitiveToken("clipboard", "navigator sensitive API"), newSensitiveToken("credentials", "navigator sensitive API"),
	newSensitiveToken("serviceWorker", "navigator sensitive API"),
	newSensitiveToken("fetch", "network request"), newSensitiveToken("XMLHttpRequest", "network request"),
	newSensitiveToken("WebSocket", "network request"), newSensitiveToken("EventSource", "network request"),
	newSensitiveToken("sendBeacon", "network beacon"),
}

// Whether a config value spells true (1, true, yes or on in any case), nil false
func truthy(v any) bool {
	if v == nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(anyutil.String(v))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// Decoded body of a JS string literal with escapes normalized, the raw body when the escapes do not decode
func decodeJSStringLiteral(literal string) string {
	if utf8.RuneCountInString(literal) < 2 {
		return literal
	}
	body := textutil.DropLast(textutil.DropFirst(literal, 1), 1)
	if !utf8.ValidString(body) {
		return body
	}
	if decoded, ok := unescapeJS(body); ok {
		return decoded
	}
	return body
}

// Body of a JavaScript string literal with its escapes decoded, false for a malformed hex or unicode escape
func unescapeJS(body string) (string, bool) {
	if !strings.Contains(body, `\`) {
		return body, true
	}
	var b strings.Builder
	b.Grow(len(body))
	for i := 0; i < len(body); i++ {
		if body[i] != '\\' {
			b.WriteByte(body[i])
			continue
		}
		if i++; i == len(body) {
			return "", false
		}
		switch c := body[i]; c {
		case 'n':
			b.WriteByte('\n')
		case 't':
			b.WriteByte('\t')
		case 'r':
			b.WriteByte('\r')
		case 'b':
			b.WriteByte('\b')
		case 'f':
			b.WriteByte('\f')
		case 'v':
			b.WriteByte('\v')
		case '\n':
		case '\r':
			if i+1 < len(body) && body[i+1] == '\n' {
				i++
			}
		case 'x':
			if i+2 >= len(body) {
				return "", false
			}
			v, err := strconv.ParseUint(body[i+1:i+3], 16, 8)
			if err != nil {
				return "", false
			}
			b.WriteRune(rune(v))
			i += 2
		case 'u':
			r, n, ok := jsUnicodeEscape(body[i+1:])
			if !ok {
				return "", false
			}
			b.WriteRune(r)
			i += n
		default:
			if c >= '0' && c <= '7' {
				v, n := int(c-'0'), 1
				for n < 3 && i+n < len(body) && body[i+n] >= '0' && body[i+n] <= '7' && v*8+int(body[i+n]-'0') <= 0o377 {
					v, n = v*8+int(body[i+n]-'0'), n+1
				}
				b.WriteRune(rune(v))
				i += n - 1
				continue
			}
			r, size := utf8.DecodeRuneInString(body[i:])
			if r != 0x2028 && r != 0x2029 {
				b.WriteString(body[i : i+size])
			}
			i += size - 1
		}
	}
	return b.String(), true
}

// Code point of the hex digits after a backslash-u, four of them or one to six in braces, the bytes they take, a surrogate pair of escapes joined
func jsUnicodeEscape(s string) (rune, int, bool) {
	if strings.HasPrefix(s, "{") {
		end := strings.IndexByte(s, '}')
		if end < 2 || end > 7 {
			return 0, 0, false
		}
		v, err := strconv.ParseUint(s[1:end], 16, 32)
		if err != nil || v > utf8.MaxRune {
			return 0, 0, false
		}
		return rune(v), end + 1, true
	}
	if len(s) < 4 {
		return 0, 0, false
	}
	v, err := strconv.ParseUint(s[:4], 16, 16)
	if err != nil {
		return 0, 0, false
	}
	r := rune(v)
	if r >= 0xd800 && r < 0xdc00 && len(s) >= 10 && s[4] == '\\' && s[5] == 'u' {
		if low, err := strconv.ParseUint(s[6:10], 16, 16); err == nil && low >= 0xdc00 && low < 0xe000 {
			return utf16.DecodeRune(r, rune(low)), 10, true
		}
	}
	return r, 4, true
}

// Whether a pattern matches anywhere in the subject
func searches(re *regexp2.Regexp, subject string) bool {
	ok, err := re.MatchString(subject)
	return err == nil && ok
}

// Why an expression is risky, direct spellings or tokens inside decoded string literals, empty when it is not
func riskyEvalReason(expression string) string {
	if expression == "" {
		return ""
	}
	for _, p := range riskyEvalPatterns {
		if searches(p.re, expression) {
			return p.reason
		}
	}
	var literals strings.Builder
	m, _ := jsStringLiteralRE.FindStringMatch(expression)
	for m != nil {
		literals.WriteString(decodeJSStringLiteral(m.String()))
		m, _ = jsStringLiteralRE.FindNextMatch(m)
	}
	lowered := strings.ToLower(literals.String())
	for _, t := range sensitiveEvalTokens {
		if searches(t.word, expression) || strings.Contains(lowered, strings.ToLower(t.token)) {
			return t.reason
		}
	}
	return ""
}

// Refusal text when browser.restrict_evaluate is on without allow_unsafe_evaluate and the expression is risky
func (s *scope) evalPolicyError(expression string) string {
	if !truthy(s.browserCfg("restrict_evaluate", nil)) || truthy(s.browserCfg("allow_unsafe_evaluate", nil)) {
		return ""
	}
	reason := riskyEvalReason(expression)
	if reason == "" {
		return ""
	}
	return "Blocked: browser_console(expression=...) tried to use sensitive browser " +
		"JavaScript primitive (" + reason + ") while browser.restrict_evaluate is " +
		"enabled. Use browser_snapshot/browser_get_images/browser_console " +
		"without expression for normal inspection, or set " +
		"browser.restrict_evaluate: false in config.yaml to allow programmatic evaluation."
}

// browser.snapshot_threshold floored at the minimum, the default for a missing or unparsable value, a number past the int64 range held at its bound
func (s *scope) snapshotThreshold() int64 {
	val := s.browserCfg("snapshot_threshold", nil)
	if val == nil {
		return defaultSnapshotThreshold
	}
	n, err := anyutil.ToInt(val)
	if err != nil && !errors.Is(err, anyutil.ErrRange) {
		return defaultSnapshotThreshold
	}
	return max(n, minSnapshotThreshold)
}

// Replaces path with content without following a planted symlink
func writeCacheFile(path, content string) error {
	if st, err := os.Lstat(path); err == nil && st != nil {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|openNoFollow, 0o644)
	if err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		content = strings.ReplaceAll(content, "\n", "\r\n")
	}
	if _, err := f.WriteString(content); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// Force redacted full snapshot stored under cache/web, the path the agent file tools see, empty on failure
func (s *scope) storeFullSnapshot(snapshotText string) string {
	path, err := func() (string, error) {
		content := s.redactText(snapshotText)
		if n := utf8.RuneCountInString(content); n > maxStoredSnapshotChars {
			content = textutil.First(content, maxStoredSnapshotChars) + fmt.Sprintf("\n\n[... stored copy truncated at %s chars of %s ...]",
				textutil.Thousands(maxStoredSnapshotChars), textutil.Thousands(int64(n)))
		}
		cacheDir := home.IcebathDir(s.home, "cache/web", "web_cache")
		if st, err := os.Lstat(cacheDir); err == nil && st.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("refusing to write through a symlinked %s", cacheDir)
		}
		if err := os.MkdirAll(cacheDir, 0o777); err != nil {
			return "", err
		}
		digest := sha256.Sum256([]byte(content))
		path := filepath.Join(cacheDir, "browser-snapshot-"+hex.EncodeToString(digest[:])[:10]+".txt")
		if err := writeCacheFile(path, content); err != nil {
			return "", err
		}
		return tools.ToAgentVisibleCachePath(s.env, path), nil
	}()
	if err != nil {
		pkgLog().Debug(fmt.Sprintf("Failed to store full browser snapshot: %v", err))
		return ""
	}
	return path
}

// Snapshot cut at a line boundary to maxChars, the note naming the stored full tree for refs past the cut
func (s *scope) truncateSnapshot(snapshotText string, maxChars int64) string {
	if int64(utf8.RuneCountInString(snapshotText)) <= maxChars {
		return snapshotText
	}
	storedPath := s.storeFullSnapshot(snapshotText)
	lines := strings.Split(snapshotText, "\n")
	var result []string
	var chars int64
	reserve := min(110+int64(utf8.RuneCountInString(storedPath)), floorDiv(maxChars, 2))
	for _, line := range lines {
		n := int64(utf8.RuneCountInString(line))
		if chars+n+1 > maxChars-reserve {
			break
		}
		result = append(result, line)
		chars += n + 1
	}
	if remaining := len(lines) - len(result); remaining > 0 {
		if storedPath != "" {
			result = append(result, fmt.Sprintf("\n[... %d more lines truncated — full snapshot: read_file path=\"%s\" offset=%d limit=200]",
				remaining, storedPath, len(result)+1))
		} else {
			result = append(result, fmt.Sprintf("\n[... %d more lines truncated, use browser_snapshot for full content]", remaining))
		}
	}
	return strings.Join(result, "\n")
}

// Quotient of a by a positive divisor rounded toward negative infinity
func floorDiv(a, b int64) int64 {
	q := a / b
	if (a%b != 0) && (a < 0) {
		q--
	}
	return q
}
