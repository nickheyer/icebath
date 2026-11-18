package camofox

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/dlclark/regexp2/v2"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyre"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	defaultSnapshotThreshold = 15000
	minSnapshotThreshold     = 1000
	maxStoredSnapshotChars   = 2_000_000
	redactWalkMaxDepth       = 320
	redactWalkDepthMarker    = "[redacted: nesting limit]"
)

// Error result dict with success false, the extra keys after the error
func policyErr(message string, extra ...any) *pyjson.Object {
	return pyjson.ObjectOf(append([]any{"success", false, "error", message}, extra...)...)
}

// browser key of the profile config, def on a miss or an unreadable config
func (s *scope) browserCfg(key string, def any) any {
	raw, err := s.rawConfig()
	if err != nil {
		policyLog().Debug(fmt.Sprintf("Could not read browser.%s: %v", key, err))
		return def
	}
	return cfgGet(raw, def, "browser", key)
}

// Force redacted browser originated text, tool output being a model boundary
func (s *scope) redactText(text string) string {
	return tools.RedactSensitiveTextWith(s.env, text, tools.RedactOptions{Force: true})
}

// Force redacted value of any type, None kept and other values redacted as their str
func (s *scope) redactAny(v any) any {
	switch t := v.(type) {
	case nil:
		return nil
	case string:
		return s.redactText(t)
	}
	return s.redactText(pyrepr.Str(v))
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
	case *pyjson.Object:
		if t == nil {
			return value
		}
		out := pyjson.NewObject()
		for _, k := range t.Keys {
			out.Set(pyrepr.Str(s.redactOutput(k, depth+1)), s.redactOutput(t.Vals[k], depth+1))
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
	case *pyjson.Object:
		if t == nil {
			return value
		}
		out := pyjson.NewObject()
		for _, k := range t.Keys {
			out.Set(k, replaceTyped(t.Vals[k], needle, redacted))
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
func secretURLError(raw string) *pyjson.Object {
	if tools.RedactPrefixSearch(raw) || tools.RedactPrefixSearch(pyurl.Unquote(raw)) {
		return policyErr("Blocked: URL contains what appears to be an API key or token. Secrets must not be sent in URLs.")
	}
	return nil
}

// Normalized navigate target and the refusal of a secret bearing, metadata or blocklisted URL
func (s *scope) navigationPolicyError(raw string) (string, *pyjson.Object) {
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
			pyjson.ObjectOf("host", blocked.Host, "rule", blocked.Rule, "source", blocked.Source))
	}
	return raw, nil
}

// One sensitive JavaScript primitive pattern with the reason it names
type riskyPattern struct {
	re     *regexp2.Regexp
	reason string
}

var riskyEvalPatterns = []riskyPattern{
	{pyre.MustCompile(`(?i)\bdocument\s*\.\s*cookie\b`), "document.cookie"},
	{pyre.MustCompile(`(?i)\b(?:localStorage|sessionStorage)\b`), "web storage"},
	{pyre.MustCompile(`(?i)\bindexedDB\b`), "IndexedDB"},
	{pyre.MustCompile(`(?i)\bcaches\s*\.\s*(?:open|match|keys)\b`), "Cache Storage"},
	{pyre.MustCompile(`(?i)\bnavigator\s*\.\s*(?:clipboard|credentials|serviceWorker)\b`), "navigator sensitive API"},
	{pyre.MustCompile(`(?i)\b(?:fetch|XMLHttpRequest|WebSocket|EventSource)\s*\(`), "network request"},
	{pyre.MustCompile(`(?i)\bnavigator\s*\.\s*sendBeacon\s*\(`), "network beacon"},
	{pyre.MustCompile(`(?is)\bdocument\s*\.\s*forms\b.*\bvalue\b`), "form value extraction"},
	{pyre.MustCompile(`(?is)\bquerySelector(?:All)?\s*\([^)]*(?:input|textarea|password)[^)]*\).*\bvalue\b`), "form value extraction"},
}

var jsStringLiteralRE = pyre.MustCompile("(?s)'(?:\\\\.|[^'\\\\])*'|\"(?:\\\\.|[^\"\\\\])*\"|`(?:\\\\.|[^`\\\\])*`")

// Token a decoded string literal or a bare word may spell, with its pattern and reason
type sensitiveToken struct {
	token  string
	word   *regexp2.Regexp
	reason string
}

// Sensitive token with its case insensitive whole word pattern
func newSensitiveToken(token, reason string) sensitiveToken {
	return sensitiveToken{token: token, word: pyre.MustCompile(`(?i)\b` + token + `\b`), reason: reason}
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

// Python truthy spelling of a config value, None false
func truthy(v any) bool {
	if v == nil {
		return false
	}
	switch pystr.Lower(pystr.Strip(pyrepr.Str(v))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// Decoded body of a JS string literal with escapes normalized, the raw body when the escapes do not decode
func decodeJSStringLiteral(literal string) string {
	if pystr.Len(literal) < 2 {
		return literal
	}
	body := pystr.Slice(literal, 1, pystr.Len(literal)-1)
	if pystr.StrictUTF8EncodeError(body) != nil {
		return body
	}
	if decoded, err := state.UnicodeEscapeDecode([]byte(body)); err == nil {
		return decoded
	}
	return body
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
	lowered := pystr.Lower(literals.String())
	for _, t := range sensitiveEvalTokens {
		if searches(t.word, expression) || strings.Contains(lowered, pystr.Lower(t.token)) {
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

// browser.snapshot_threshold floored at the minimum, the default for a missing or unparsable value
func (s *scope) snapshotThreshold() (int64, error) {
	val := s.browserCfg("snapshot_threshold", nil)
	if val == nil {
		return defaultSnapshotThreshold, nil
	}
	n, err := pyint.Of(val)
	if err != nil {
		if pe, ok := err.(*pyint.Error); ok && pe.Kind == "OverflowError" {
			return 0, err
		}
		return defaultSnapshotThreshold, nil
	}
	return max(saturateInt64(n), minSnapshotThreshold), nil
}

// Replaces path with content without following a planted symlink
func writeCacheFile(path, content string) error {
	if st, err := os.Lstat(path); err == nil && st != nil {
		if err := pypath.Unlink(path); err != nil && !os.IsNotExist(err) {
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
		if n := pystr.Len(content); n > maxStoredSnapshotChars {
			content = pystr.Slice(content, 0, maxStoredSnapshotChars) + fmt.Sprintf("\n\n[... stored copy truncated at %s chars of %s ...]",
				pystr.GroupThousands(maxStoredSnapshotChars), pystr.GroupThousands(int64(n)))
		}
		cacheDir := home.HermesDir(s.home, "cache/web", "web_cache")
		if st, err := os.Lstat(cacheDir); err == nil && st.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("refusing to write through a symlinked %s", cacheDir)
		}
		if err := os.MkdirAll(cacheDir, 0o777); err != nil {
			return "", err
		}
		if err := pystr.StrictUTF8EncodeError(content); err != nil {
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
		policyLog().Debug(fmt.Sprintf("Failed to store full browser snapshot: %v", err))
		return ""
	}
	return path
}

// Snapshot cut at a line boundary to maxChars, the note naming the stored full tree for refs past the cut
func (s *scope) truncateSnapshot(snapshotText string, maxChars int64) string {
	if int64(pystr.Len(snapshotText)) <= maxChars {
		return snapshotText
	}
	storedPath := s.storeFullSnapshot(snapshotText)
	lines := strings.Split(snapshotText, "\n")
	var result []string
	var chars int64
	reserve := min(110+int64(pystr.Len(storedPath)), floorDiv(maxChars, 2))
	for _, line := range lines {
		n := int64(pystr.Len(line))
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

// Python floor division of an int by a positive divisor
func floorDiv(a, b int64) int64 {
	q := a / b
	if (a%b != 0) && (a < 0) {
		q--
	}
	return q
}

// Logger of the reference camofox_policy module
func policyLog() *slog.Logger {
	return slog.Default().With("logger", "hermes_plugins.camofox.camofox_policy")
}
