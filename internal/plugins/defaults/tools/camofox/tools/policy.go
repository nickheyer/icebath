package tools

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/dlclark/regexp2/v2"
	"github.com/spf13/cast"

	browserconfig "github.com/nickheyer/icebath/internal/browser/config"
	"github.com/nickheyer/icebath/internal/tools/redact"
	"github.com/nickheyer/icebath/internal/web"
	"github.com/nickheyer/icebath/pkg/strs"
)

const (
	defaultThreshold = 15000
	minThreshold     = 1000
	maxStored        = 2_000_000
	walkDepth        = 320
	walkMarker       = "[redacted: nesting limit]"
)

func policyErr(message string, extra ...any) map[string]any {
	out := map[string]any{"success": false, "error": message}
	for i := 0; i+1 < len(extra); i += 2 {
		out[extra[i].(string)] = extra[i+1]
	}
	return out
}

// Text the browser sent with its secrets masked, tool output being where the model reads it
func redactText(s string) string { return redact.Text(s, false, redact.Ends) }

func redactAny(v any) any {
	switch t := v.(type) {
	case nil:
		return nil
	case string:
		return redactText(t)
	}
	return redactText(text(v))
}

// Nested browser data with its text masked, a tree past the walk limit replaced by the marker
func redactOutput(value any, depth int) any {
	if depth >= walkDepth {
		return walkMarker
	}
	switch t := value.(type) {
	case string:
		return redactText(t)
	case []any:
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = redactOutput(item, depth+1)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, v := range t {
			out[redactText(k)] = redactOutput(v, depth+1)
		}
		return out
	}
	return value
}

// Every place the typed text shows up masked as the masking would mask it alone
func redactTyped(value any, typed string) any {
	if typed == "" {
		return value
	}
	masked := redactText(typed)
	if masked == typed {
		return value
	}
	return replaceTyped(value, typed, masked)
}

func replaceTyped(value any, needle, masked string) any {
	switch t := value.(type) {
	case string:
		return strings.ReplaceAll(t, needle, masked)
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, v := range t {
			out[k] = replaceTyped(v, needle, masked)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = replaceTyped(item, needle, masked)
		}
		return out
	}
	return value
}

// Refusal of an address that carries an API key or token, plain or percent decoded
func secretURL(raw string) map[string]any {
	decoded, err := url.PathUnescape(raw)
	if err != nil {
		decoded = raw
	}
	if web.CarriesSecret(raw) || web.CarriesSecret(decoded) {
		return policyErr("Blocked: URL contains what appears to be an API key or token. Secrets must not be sent in URLs.")
	}
	return nil
}

// Navigation target normalized, and the refusal of one that carries a secret, reaches cloud metadata or the website policy blocks
func (s *scope) navigationPolicy(raw string) (string, map[string]any) {
	if refused := secretURL(raw); refused != nil {
		return raw, refused
	}
	raw = web.NormalizeURL(raw)
	if refused := secretURL(raw); refused != nil {
		return raw, refused
	}
	if web.AlwaysBlocked(raw) {
		return raw, policyErr("Blocked: URL targets a cloud metadata endpoint")
	}
	if blocked := web.Blocked(s.cfg, raw); blocked != nil {
		return raw, policyErr(blocked.Message, "blocked_by_policy", map[string]any{"host": blocked.Host, "rule": blocked.Rule, "source": blocked.Source})
	}
	return raw, nil
}

type risky struct {
	re     *regexp2.Regexp
	reason string
}

func compile(pattern string) *regexp2.Regexp { return regexp2.MustCompile(pattern, regexp2.RE2) }

var riskyPatterns = []risky{
	{compile(`(?i)\bdocument\s*\.\s*cookie\b`), "document.cookie"},
	{compile(`(?i)\b(?:localStorage|sessionStorage)\b`), "web storage"},
	{compile(`(?i)\bindexedDB\b`), "IndexedDB"},
	{compile(`(?i)\bcaches\s*\.\s*(?:open|match|keys)\b`), "Cache Storage"},
	{compile(`(?i)\bnavigator\s*\.\s*(?:clipboard|credentials|serviceWorker)\b`), "navigator sensitive API"},
	{compile(`(?i)\b(?:fetch|XMLHttpRequest|WebSocket|EventSource)\s*\(`), "network request"},
	{compile(`(?i)\bnavigator\s*\.\s*sendBeacon\s*\(`), "network beacon"},
	{compile(`(?is)\bdocument\s*\.\s*forms\b.*\bvalue\b`), "form value extraction"},
	{compile(`(?is)\bquerySelector(?:All)?\s*\([^)]*(?:input|textarea|password)[^)]*\).*\bvalue\b`), "form value extraction"},
}

var stringLiteral = compile("(?s)'(?:\\\\.|[^'\\\\])*'|\"(?:\\\\.|[^\"\\\\])*\"|`(?:\\\\.|[^`\\\\])*`")

type sensitive struct {
	token  string
	word   *regexp2.Regexp
	reason string
}

func token(t, reason string) sensitive { return sensitive{t, compile(`(?i)\b` + t + `\b`), reason} }

var sensitiveTokens = []sensitive{
	token("cookie", "document.cookie"), token("localStorage", "web storage"), token("sessionStorage", "web storage"), token("indexedDB", "IndexedDB"),
	token("caches", "Cache Storage"), token("clipboard", "navigator sensitive API"), token("credentials", "navigator sensitive API"),
	token("serviceWorker", "navigator sensitive API"), token("fetch", "network request"), token("XMLHttpRequest", "network request"),
	token("WebSocket", "network request"), token("EventSource", "network request"), token("sendBeacon", "network beacon"),
}

// Body of a JavaScript string literal with its escapes decoded, the raw body when they don't decode
func literalBody(literal string) string {
	r := []rune(literal)
	if len(r) < 2 {
		return literal
	}
	body := string(r[1 : len(r)-1])
	if decoded, ok := unescapeJS(body); ok {
		return decoded
	}
	return body
}

// A JavaScript string body with its escapes decoded, false for a malformed hex or unicode escape
func unescapeJS(body string) (string, bool) {
	if !strings.Contains(body, `\`) {
		return body, true
	}
	var b strings.Builder
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
			r, n, ok := unicodeEscape(body[i+1:])
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

// Code point of the hex after a backslash-u, four digits or one to six in braces, and the bytes they take; a surrogate pair joined
func unicodeEscape(s string) (rune, int, bool) {
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

func searches(re *regexp2.Regexp, subject string) bool {
	ok, err := re.MatchString(subject)
	return err == nil && ok
}

// Why a script is risky, by how it spells a sensitive primitive or by the tokens its string literals hold; empty when it is not
func riskyReason(expression string) string {
	if expression == "" {
		return ""
	}
	for _, p := range riskyPatterns {
		if searches(p.re, expression) {
			return p.reason
		}
	}
	var literals strings.Builder
	m, _ := stringLiteral.FindStringMatch(expression)
	for m != nil {
		literals.WriteString(literalBody(m.String()))
		m, _ = stringLiteral.FindNextMatch(m)
	}
	lowered := strings.ToLower(literals.String())
	for _, t := range sensitiveTokens {
		if searches(t.word, expression) || strings.Contains(lowered, strings.ToLower(t.token)) {
			return t.reason
		}
	}
	return ""
}

// Refusal of a risky script while browser.restrict_evaluate is on and browser.allow_unsafe_evaluate off
func (s *scope) evalPolicy(expression string) string {
	if !s.cfg.GetBool(browserconfig.RestrictEvaluate) || s.cfg.GetBool(browserconfig.AllowUnsafeEvaluate) {
		return ""
	}
	reason := riskyReason(expression)
	if reason == "" {
		return ""
	}
	return "Blocked: browser_console(expression=...) tried to use sensitive browser JavaScript primitive (" + reason + ") while " +
		"browser.restrict_evaluate is enabled. Use browser_snapshot/browser_get_images/browser_console without expression for normal inspection, or " +
		"set browser.restrict_evaluate: false in config.yaml to allow programmatic evaluation."
}

// browser.snapshot_threshold, never under the minimum
func (s *scope) threshold() int64 {
	n, err := cast.ToInt64E(s.cfg.Get(browserconfig.SnapshotThreshold))
	if err != nil {
		return defaultThreshold
	}
	return max(n, minThreshold)
}

// Writes the file anew, never through a planted link
func writeCache(path, content string) error {
	if _, err := os.Lstat(path); err == nil {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|noFollow, 0o644)
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

// The whole snapshot, masked, kept in the web cache at the path the agent's file tools see; empty on failure
func (s *scope) storeSnapshot(snapshot string) string {
	path, err := func() (string, error) {
		content := redactText(snapshot)
		if n := utf8.RuneCountInString(content); n > maxStored {
			content = strs.Head(content, maxStored) + fmt.Sprintf("\n\n[... stored copy truncated at %s chars of %s ...]", strs.Thousands(maxStored), strs.Thousands(n))
		}
		dir := filepath.Join(s.cfg.ProfileDir(s.cfg.Profile()), "cache", "web")
		if st, err := os.Lstat(dir); err == nil && st.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("refusing to write through a symlinked %s", dir)
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return "", err
		}
		digest := sha256.Sum256([]byte(content))
		path := filepath.Join(dir, "browser-snapshot-"+hex.EncodeToString(digest[:])[:10]+".txt")
		if err := writeCache(path, content); err != nil {
			return "", err
		}
		if s.env != nil && s.env.Visible != nil {
			return s.env.Visible(path), nil
		}
		return path, nil
	}()
	if err != nil {
		slog.Debug(fmt.Sprintf("Failed to store full browser snapshot: %v", err))
		return ""
	}
	return path
}

// Snapshot cut at a line to the limit, the note naming the stored whole tree for refs past the cut
func (s *scope) truncate(snapshot string, limit int64) string {
	if int64(utf8.RuneCountInString(snapshot)) <= limit {
		return snapshot
	}
	stored := s.storeSnapshot(snapshot)
	lines := strings.Split(snapshot, "\n")
	var kept []string
	var chars int64
	half := limit / 2
	reserve := min(110+int64(utf8.RuneCountInString(stored)), half)
	for _, line := range lines {
		n := int64(utf8.RuneCountInString(line))
		if chars+n+1 > limit-reserve {
			break
		}
		kept = append(kept, line)
		chars += n + 1
	}
	if remaining := len(lines) - len(kept); remaining > 0 {
		if stored != "" {
			kept = append(kept, fmt.Sprintf("\n[... %d more lines truncated — full snapshot: read_file path=\"%s\" offset=%d limit=200]", remaining, stored,
				len(kept)+1))
		} else {
			kept = append(kept, fmt.Sprintf("\n[... %d more lines truncated, use browser_snapshot for full content]", remaining))
		}
	}
	return strings.Join(kept, "\n")
}
