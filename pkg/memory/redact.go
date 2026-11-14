package memory

import (
	"net/url"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Marker returned when the redactor itself fails so raw text never leaves the process
const RedactionUnavailable = "[redaction-unavailable]"

// Known API key prefixes, every pattern starting with the literal the pre screen gates on
var prefixPatterns = []string{
	`sk-[A-Za-z0-9_-](?:\.?[A-Za-z0-9_-]){9,}`,
	`ctx7sk-[A-Za-z0-9_-]{10,}`,
	`ghp_[A-Za-z0-9]{10,}`,
	`github_pat_[A-Za-z0-9_]{10,}`,
	`gho_[A-Za-z0-9]{10,}`,
	`ghu_[A-Za-z0-9]{10,}`,
	`ghs_[A-Za-z0-9]{10,}`,
	`ghr_[A-Za-z0-9]{10,}`,
	`xapp-\d+-[A-Za-z0-9-]{10,}`,
	`xox[baprs]-[A-Za-z0-9-]{10,}`,
	`AIza[A-Za-z0-9_-]{30,}`,
	`pplx-[A-Za-z0-9]{10,}`,
	`fal_[A-Za-z0-9_-]{10,}`,
	`fc-[A-Za-z0-9]{10,}`,
	`bb_live_[A-Za-z0-9_-]{10,}`,
	`gAAAA[A-Za-z0-9_=-]{20,}`,
	`AKIA[A-Z0-9]{16}`,
	`sk_live_[A-Za-z0-9]{10,}`,
	`sk_test_[A-Za-z0-9]{10,}`,
	`rk_live_[A-Za-z0-9]{10,}`,
	`SG\.[A-Za-z0-9_-]{10,}`,
	`hf_[A-Za-z0-9]{10,}`,
	`r8_[A-Za-z0-9]{10,}`,
	`npm_[A-Za-z0-9]{10,}`,
	`pypi-[A-Za-z0-9_-]{10,}`,
	`dop_v1_[A-Za-z0-9]{10,}`,
	`doo_v1_[A-Za-z0-9]{10,}`,
	`am_(?:org_)?[A-Za-z0-9]{20,}`,
	`sk_[A-Za-z0-9_]{10,}`,
	`tvly-[A-Za-z0-9]{10,}`,
	`exa_[A-Za-z0-9]{10,}`,
	`gsk_[A-Za-z0-9]{10,}`,
	`syt_[A-Za-z0-9]{10,}`,
	`retaindb_[A-Za-z0-9]{10,}`,
	`hsk-[A-Za-z0-9]{10,}`,
	`mem0_[A-Za-z0-9]{10,}`,
	`brv_[A-Za-z0-9]{10,}`,
	`xai-[A-Za-z0-9]{30,}`,
	`ntn_[A-Za-z0-9]{10,}`,
	`fw-[A-Za-z0-9]{30,}`,
	`fw_[A-Za-z0-9]{30,}`,
	`fpk_[A-Za-z0-9]{30,}`,
	`glpat-[A-Za-z0-9_\-]{10,}`,
	`gloas-[A-Za-z0-9_\-]{10,}`,
	`gldt-[A-Za-z0-9_\-]{10,}`,
	`glrt-[A-Za-z0-9_.\-]{10,}`,
	`glrtr-[A-Za-z0-9_.\-]{10,}`,
	`glcbt-[A-Za-z0-9_\-]{10,}`,
	`glptt-[A-Za-z0-9_\-]{10,}`,
	`glft-[A-Za-z0-9_\-]{10,}`,
	`glimt-[A-Za-z0-9_\-]{10,}`,
	`glagent-[A-Za-z0-9_\-]{10,}`,
	`glsoat-[A-Za-z0-9_\-]{10,}`,
	`glffct-[A-Za-z0-9_\-]{10,}`,
	`glwt-[A-Za-z0-9_\-]{10,}`,
	`GR1348941[A-Za-z0-9_\-]{10,}`,
	`pk-lf-[A-Za-z0-9\-]{8,}`,
}

// Every built in secret prefix pattern in registration order, the corpus egress callers test against
func PrefixPatterns() []string { return prefixPatterns }

var prefixSubstrings = literalPrefixes(prefixPatterns)
var prefixAlternation = regexp.MustCompile(`(?:` + strings.Join(prefixPatterns, "|") + `)`)
var prefixWhole = regexp.MustCompile(`^(?:` + strings.Join(prefixPatterns, "|") + `)$`)

// Leading literal characters of a regex up to its first metacharacter
func literalPrefix(pattern string) string {
	for i, ch := range pattern {
		if strings.ContainsRune(`[(\.?*+|{^$`, ch) {
			return pattern[:i]
		}
	}
	return pattern
}

// Leading literal characters of a prefix pattern, the text a synthetic token starts with
func LiteralPrefix(pattern string) string { return literalPrefix(pattern) }

func literalPrefixes(patterns []string) []string {
	out := make([]string, 0, len(patterns))
	for _, p := range patterns {
		out = append(out, literalPrefix(p))
	}
	return out
}

func isTokenEdge(b byte) bool {
	return b == '_' || b == '-' || (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9')
}

// Spans of prefix matched credentials with the Python word edge lookarounds applied
func prefixMatches(text string) [][2]int {
	var out [][2]int
	offset := 0
	for offset < len(text) {
		loc := prefixAlternation.FindStringIndex(text[offset:])
		if loc == nil {
			break
		}
		start, end := offset+loc[0], offset+loc[1]
		if start > 0 && isTokenEdge(text[start-1]) {
			offset = start + 1
			continue
		}
		for end > start && end < len(text) && isTokenEdge(text[end]) {
			end--
			for end > start && !prefixWhole.MatchString(text[start:end]) {
				end--
			}
		}
		if end > start && (end >= len(text) || !isTokenEdge(text[end])) {
			out = append(out, [2]int{start, end})
			offset = end
			continue
		}
		offset = start + 1
	}
	return out
}

func replaceSpans(text string, spans [][2]int, fn func(string) string) string {
	if len(spans) == 0 {
		return text
	}
	var b strings.Builder
	last := 0
	for _, s := range spans {
		b.WriteString(text[last:s[0]])
		b.WriteString(fn(text[s[0]:s[1]]))
		last = s[1]
	}
	b.WriteString(text[last:])
	return b.String()
}

var displayControlRe = regexp.MustCompile("[\x00-\x1f\x7f\u0080-\u009f\u200b-\u200f\u202a-\u202e\u2060-\u2064]")

// Secret masked for display, keeping head and tail once the value clears the floor
func MaskSecret(value string, head, tail, floor int, placeholder, empty string) string {
	if value != "" {
		value = displayControlRe.ReplaceAllString(value, "")
	}
	if value == "" {
		return empty
	}
	runes := []rune(value)
	if len(runes) < floor {
		return placeholder
	}
	return string(runes[:head]) + "..." + string(runes[len(runes)-tail:])
}

func maskToken(token string) string {
	if token == "" {
		return "***"
	}
	return MaskSecret(token, 6, 4, 18, "***", "")
}

func maskTokenNonreusable(token string) string {
	for _, sub := range prefixSubstrings {
		if token != "" && strings.HasPrefix(token, sub) {
			return "\u00abredacted:" + sub + "\u2026\u00bb"
		}
	}
	return "\u00abredacted-secret\u00bb"
}

func hasKnownPrefixSubstring(text string) bool {
	for _, p := range prefixSubstrings {
		if strings.Contains(text, p) {
			return true
		}
	}
	return false
}

func isControlRune(r rune) bool {
	return r <= 0x1f || r == 0x7f || (r >= 0x200b && r <= 0x200f) || (r >= 0x2028 && r <= 0x202f) || r == 0x2060 || r == 0xfeff
}

func isTokenBodyRune(r rune) bool {
	return r == '_' || r == '-' || r == '.' || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
}

// Masks credentials whose body control characters split, matching on a stripped copy and masking the original span
func maskControlSplitTokens(text string, mask func(string) string) string {
	runes := []rune(text)
	var stripped []rune
	var origIdx []int
	for i, r := range runes {
		if !isControlRune(r) {
			stripped = append(stripped, r)
			origIdx = append(origIdx, i)
		}
	}
	if len(stripped) == len(runes) {
		return text
	}
	strippedText := string(stripped)
	byteToRune := map[int]int{}
	pos := 0
	for i, r := range stripped {
		byteToRune[pos] = i
		pos += len(string(r))
	}
	byteToRune[pos] = len(stripped)
	type repl struct {
		start, end int
		text       string
	}
	var repls []repl
	for _, span := range prefixMatches(strippedText) {
		rs, re := byteToRune[span[0]], byteToRune[span[1]]
		startOrig, endOrig := origIdx[rs], origIdx[re-1]+1
		spanText := string(runes[startOrig:endOrig])
		if (strings.ContainsAny(spanText, "\n\r")) && len(prefixMatches(spanText)) > 0 {
			continue
		}
		ok := true
		for _, r := range runes[startOrig:endOrig] {
			if !isTokenBodyRune(r) && !isControlRune(r) {
				ok = false
				break
			}
		}
		if ok && (endOrig >= len(runes) || runes[endOrig] != '=') {
			repls = append(repls, repl{startOrig, endOrig, mask(strippedText[span[0]:span[1]])})
		}
	}
	if len(repls) == 0 {
		return text
	}
	var b strings.Builder
	last := 0
	for _, r := range repls {
		b.WriteString(string(runes[last:r.start]))
		b.WriteString(r.text)
		last = r.end
	}
	b.WriteString(string(runes[last:]))
	return b.String()
}

var zhipuRe = regexp.MustCompile(`([0-9a-f]{32}\.[A-Za-z0-9]{16,})`)

func isDottedTokenEdge(b byte) bool { return b == '.' || isTokenEdge(b) }

func redactZhipu(text string, mask func(string) string) string {
	var spans [][2]int
	offset := 0
	for offset < len(text) {
		loc := zhipuRe.FindStringIndex(text[offset:])
		if loc == nil {
			break
		}
		s, e := offset+loc[0], offset+loc[1]
		if (s == 0 || !isDottedTokenEdge(text[s-1])) && (e >= len(text) || !isDottedTokenEdge(text[e])) {
			spans = append(spans, [2]int{s, e})
			offset = e
			continue
		}
		offset = s + 1
	}
	return replaceSpans(text, spans, mask)
}

// Assignment passes
const secretEnvNames = `(?:API_?KEY|KEY|TOKEN|SECRET|PASSWORD|PASSWD|PASS|PW|CREDENTIAL|AUTH)`
const envValue = `(?:"([^\s"]+)"|'([^\s']+)'|(\S+))`

var envAssignRe = regexp.MustCompile(`([A-Z0-9_]{0,50}` + secretEnvNames + `[A-Z0-9_]{0,50})\s*=\s*` + envValue)
var envAssignLowerRe = regexp.MustCompile(`(?i)(^|[^a-z0-9_])([a-z0-9_]+_(?:key|pass|pw|token|secret|password|passwd|credential|auth))\s*=\s*` + envValue)

const secretCfgNames = `(?:api[ _.\-]?key|token|secret|passwd|password|credential|auth)`
const lineNumberGutter = `(?:[0-9]+(?:[|:\-]|\t)[ \t]*)?`
const cfgValue = `(?:"([^\s&"]+)"|'([^\s&']+)'|([^\s&]+))([\s&]|$)`

var cfgSecretWordRe = regexp.MustCompile(`(?i)` + secretCfgNames)
var envLookupValueRe = regexp.MustCompile(`^(?:os\.(?:getenv|environ)|process\.env|\$ENV\{)`)
var cfgDottedRe = regexp.MustCompile(`(?i)(^|[^A-Za-z0-9_.\-])([A-Za-z0-9_\-]+\.[A-Za-z0-9_.\-]*` + secretCfgNames + `[A-Za-z0-9_.\-]*|[A-Za-z0-9_.\-]*` + secretCfgNames + `[A-Za-z0-9_.\-]*\.[A-Za-z0-9_.\-]+)=` + cfgValue)
var cfgAnchoredRe = regexp.MustCompile(`(?im)(^[ \t]*` + lineNumberGutter + `(?:export[ \t]+)?[A-Za-z0-9_\-]*` + secretCfgNames + `[A-Za-z0-9_\-]*)=` + cfgValue)

const yamlCfgNames = `(?:api[ _.\-]?key|token|secret|passwd|password|credential)`

var yamlAssignRe = regexp.MustCompile(`(?im)(^[ \t]*` + lineNumberGutter + `[A-Za-z0-9_.\-]*` + yamlCfgNames + `[A-Za-z0-9_.\-]*)(:[ \t]*)([^\s&'"][^\s&]*)`)

var keyKeywordRe = regexp.MustCompile(`(?i)(?:api|auth|access|refresh|session|secret)[ _.\\-]?(?:key|token)|token|secret|passwd|password|pass|pw|credential|auth|key`)
var strongKeyKeywordRe = regexp.MustCompile(`(?i)(?:api|auth|access|refresh|session|id|bearer)[ _.\\-]?(?:key|token)|key[ _.\\-]?material|secret|passwd|password|pass|pw|credential|auth|bearer`)
var passwordKeyRe = regexp.MustCompile(`(?i)passwd|password|pass|pw`)

const shellVarRef = `\$(?:\{[A-Za-z_]\w*[^}]*\}|[A-Za-z_]\w*)`

var pathOrVarValueRe = regexp.MustCompile(`^(?:` + shellVarRef + `|\$\(|~|/)(?:[\w./:-]|` + shellVarRef + `)*$`)

func isWordStart(s []rune, i int) bool {
	if i == 0 {
		return true
	}
	prev, cur := s[i-1], s[i]
	if !unicode.IsLetter(prev) || (unicode.IsUpper(cur) && unicode.IsLower(prev)) {
		return true
	}
	return unicode.IsUpper(cur) && unicode.IsUpper(prev) && i+1 < len(s) && unicode.IsLower(s[i+1])
}

func isWordEnd(s []rune, j int, allowPlural bool) bool {
	if j >= len(s) {
		return true
	}
	cur := s[j]
	if !unicode.IsLetter(cur) || (unicode.IsUpper(cur) && unicode.IsLower(s[j-1])) {
		return true
	}
	return allowPlural && (cur == 's' || cur == 'S') && isWordEnd(s, j+1, false)
}

// Reports whether the keyword regex matches the key at a word boundary
func hasWordBoundedKeyword(key string, re *regexp.Regexp) bool {
	runes := []rune(key)
	byteToRune := map[int]int{}
	pos := 0
	for i, r := range runes {
		byteToRune[pos] = i
		pos += len(string(r))
	}
	byteToRune[pos] = len(runes)
	for _, m := range re.FindAllStringIndex(key, -1) {
		if isWordStart(runes, byteToRune[m[0]]) && isWordEnd(runes, byteToRune[m[1]], true) {
			return true
		}
	}
	return false
}

var hexRe = regexp.MustCompile(`^[A-Fa-f0-9]+$`)
var opaqueCharsRe = regexp.MustCompile(`^[A-Za-z0-9_./+=-]+$`)

func looksLikeOpaqueCredential(value string) bool {
	if value == "***" || strings.HasPrefix(value, "\u00abredacted:") {
		return true
	}
	n := len([]rune(value))
	if n >= 16 && hexRe.MatchString(value) {
		return true
	}
	if n >= 20 && opaqueCharsRe.MatchString(value) {
		return true
	}
	if n < 12 {
		return false
	}
	classes := 0
	if strings.IndexFunc(value, unicode.IsLower) >= 0 {
		classes++
	}
	if strings.IndexFunc(value, unicode.IsUpper) >= 0 {
		classes++
	}
	if strings.IndexFunc(value, unicode.IsDigit) >= 0 {
		classes++
	}
	return classes >= 2
}

// Reports whether a dotless segment of 16 or more characters mixes cases and digits like a secret
func opaquePathSegment(seg string) bool {
	if strings.Contains(seg, ".") || len([]rune(seg)) < 16 {
		return false
	}
	return strings.IndexFunc(seg, unicode.IsLower) >= 0 && strings.IndexFunc(seg, unicode.IsUpper) >= 0 && strings.IndexFunc(seg, unicode.IsDigit) >= 0
}

func shouldRedactAssignment(key, value string, checkKeyword bool) bool {
	if envLookupValueRe.MatchString(value) {
		return false
	}
	if value == "***" || strings.HasPrefix(value, "\u00abredacted") {
		return false
	}
	if checkKeyword && !hasWordBoundedKeyword(key, keyKeywordRe) {
		return false
	}
	if pathOrVarValueRe.MatchString(value) && !hasWordBoundedKeyword(key, passwordKeyRe) {
		opaque := false
		for _, seg := range strings.Split(value, "/") {
			if opaquePathSegment(seg) {
				opaque = true
				break
			}
		}
		if value[0] == '$' || !opaque {
			return false
		}
	}
	return hasWordBoundedKeyword(key, strongKeyKeywordRe) || looksLikeOpaqueCredential(value)
}

// First non empty of the quoted and bare value groups with the quote that wrapped it
func pickValue(groups ...string) (string, string) {
	quotes := []string{`"`, `'`, ``}
	for i, g := range groups {
		if g != "" {
			return g, quotes[i]
		}
	}
	return "", ""
}

const jsonKeyNames = `(?:api_?[Kk]ey|token|secret|password|access_token|refresh_token|auth_token|bearer|secret_value|raw_secret|secret_input|key_material)`

var jsonFieldRe = regexp.MustCompile(`(?i)("` + jsonKeyNames + `")\s*:\s*"([^"]+)"`)
var jsonFieldEscaped1Re = regexp.MustCompile(`(?i)(\\"` + jsonKeyNames + `\\")\s*:\s*\\"([^"\\]+)\\"`)
var jsonFieldEscaped2Re = regexp.MustCompile(`(?i)(\\\\"` + jsonKeyNames + `\\\\")\s*:\s*\\\\"([^"\\]+)\\\\"`)

var pythonReprSecretKeys = map[string]bool{
	"access_token": true, "refresh_token": true, "id_token": true, "auth_token": true, "token": true, "api_key": true,
	"apikey": true, "client_secret": true, "secret": true, "password": true, "passwd": true, "private_key": true,
	"credential": true, "credentials": true, "authorization": true, "bearer": true, "secret_value": true,
	"raw_secret": true, "secret_input": true, "key_material": true,
}
var pythonReprEnvSuffixes = []string{"_API_KEY", "_TOKEN", "_SECRET", "_PASSWORD", "_PASSWD", "_CREDENTIAL", "_CREDENTIALS"}
var pythonReprCredentialSuffixes = []string{"apikey", "api_key", "token", "secret", "password", "passwd", "credential", "credentials"}
var pythonReprFieldRe = regexp.MustCompile(`'([A-Za-z_][A-Za-z0-9_]*)'(\s*:\s*)(?:([bB]?)'((?:\\.|[^'\\])+)'|([bB]?)"((?:\\.|[^"\\])+)")`)

// Reports whether a mapping field name holds a credential under the repr field policy
func IsSecretFieldName(key string) bool {
	folded := strings.ToLower(key)
	if pythonReprSecretKeys[folded] {
		return true
	}
	if key == strings.ToUpper(key) && key != strings.ToLower(key) {
		for _, suf := range pythonReprEnvSuffixes {
			if strings.HasSuffix(key, suf) {
				return true
			}
		}
	}
	for _, suf := range pythonReprCredentialSuffixes {
		if strings.HasSuffix(folded, suf) {
			return true
		}
	}
	return false
}

func redactPythonReprFields(text string) string {
	return pythonReprFieldRe.ReplaceAllStringFunc(text, func(m string) string {
		g := pythonReprFieldRe.FindStringSubmatch(m)
		key := g[1]
		if !IsSecretFieldName(key) {
			return m
		}
		prefix, quote, value := g[3], "'", g[4]
		if g[4] == "" && g[6] != "" {
			prefix, quote, value = g[5], `"`, g[6]
		}
		if envLookupValueRe.MatchString(value) || strings.Contains(value, "***") || strings.HasPrefix(value, "\u00abredacted:") {
			return m
		}
		return "'" + key + "'" + g[2] + prefix + quote + "***" + quote
	})
}

func redactAssignments(text string, maskNonreusable bool) string {
	mask := maskToken
	if maskNonreusable {
		mask = maskTokenNonreusable
	}
	if strings.Contains(text, "=") {
		text = envAssignRe.ReplaceAllStringFunc(text, func(m string) string {
			g := envAssignRe.FindStringSubmatch(m)
			value, quote := pickValue(g[2], g[3], g[4])
			if !shouldRedactAssignment(g[1], value, true) {
				return m
			}
			return g[1] + "=" + quote + mask(value) + quote
		})
		if !strings.Contains(text, "://") {
			text = envAssignLowerRe.ReplaceAllStringFunc(text, func(m string) string {
				g := envAssignLowerRe.FindStringSubmatch(m)
				value, quote := pickValue(g[3], g[4], g[5])
				if !shouldRedactAssignment(g[2], value, true) {
					return m
				}
				return g[1] + g[2] + "=" + quote + mask(value) + quote
			})
		}
		if !strings.Contains(text, "://") && cfgSecretWordRe.MatchString(text) {
			text = cfgDottedRe.ReplaceAllStringFunc(text, func(m string) string {
				g := cfgDottedRe.FindStringSubmatch(m)
				value, quote := pickValue(g[3], g[4], g[5])
				if !shouldRedactAssignment(g[2], value, true) {
					return m
				}
				return g[1] + g[2] + "=" + quote + mask(value) + quote + g[6]
			})
			text = cfgAnchoredRe.ReplaceAllStringFunc(text, func(m string) string {
				g := cfgAnchoredRe.FindStringSubmatch(m)
				value, quote := pickValue(g[2], g[3], g[4])
				if !shouldRedactAssignment(g[1], value, true) {
					return m
				}
				return g[1] + "=" + quote + mask(value) + quote + g[5]
			})
		}
	}
	if strings.Contains(text, ":") && strings.Contains(text, `"`) {
		text = jsonFieldRe.ReplaceAllStringFunc(text, func(m string) string {
			g := jsonFieldRe.FindStringSubmatch(m)
			if !shouldRedactAssignment(g[1], g[2], false) {
				return m
			}
			return g[1] + `: "` + mask(g[2]) + `"`
		})
		if strings.Contains(text, `\"`) {
			for _, re := range []*regexp.Regexp{jsonFieldEscaped2Re, jsonFieldEscaped1Re} {
				slashes := `\`
				if re == jsonFieldEscaped2Re {
					slashes = `\\`
				}
				text = re.ReplaceAllStringFunc(text, func(m string) string {
					g := re.FindStringSubmatch(m)
					if !shouldRedactAssignment(g[1], g[2], false) {
						return m
					}
					return g[1] + `: ` + slashes + `"` + mask(g[2]) + slashes + `"`
				})
			}
		}
	}
	if strings.Contains(text, ":") && strings.Contains(text, "'") {
		text = redactPythonReprFields(text)
	}
	if strings.Contains(text, ":") && !strings.Contains(text, "://") {
		text = yamlAssignRe.ReplaceAllStringFunc(text, func(m string) string {
			g := yamlAssignRe.FindStringSubmatch(m)
			if !shouldRedactAssignment(g[1], g[3], true) {
				return m
			}
			return g[1] + g[2] + mask(g[3])
		})
	}
	return text
}

var authHeaderRe = regexp.MustCompile(`(?i)((?:Proxy-)?Authorization:\s*)([A-Za-z][\w.+-]*\s+)?([^\s"']+)`)

// Header names carrying a single opaque credential value
var SecretHeaderNames = []string{"x-api-key", "x-goog-api-key", "api-key", "apikey", "x-api-token", "x-auth-token", "x-access-token"}
var secretHeaderRe = regexp.MustCompile(`(?i)((?:` + strings.Join(SecretHeaderNames, "|") + `)\s*:\s*)(\S+)`)
var telegramRe = regexp.MustCompile(`(^|[^\d])(bot)?(\d{8,}):([-A-Za-z0-9_]{30,})`)
var privateKeyRe = regexp.MustCompile(`-----BEGIN[A-Z ]*PRIVATE KEY-----[\s\S]*?-----END[A-Z ]*PRIVATE KEY-----`)
var dbConnstrRe = regexp.MustCompile(`(?i)((?:postgres(?:ql)?|mysql|mongodb(?:\+srv)?|redis|amqp)://[^:\s]+:)([^@\s]+)(@)`)
var urlBareTokenRe = regexp.MustCompile(`(?i)((?:https?|wss?|git|ssh|ftp|ftps|sftp)://)([^\s:@/]{8,})(@[^\s]+)`)
var jwtRe = regexp.MustCompile(`eyJ[A-Za-z0-9_-]{10,}(?:\.[A-Za-z0-9_=-]{4,}){0,2}`)
var webappLinkCredentialRe = regexp.MustCompile(`(^|[^A-Za-z0-9_-])(icebath-session=|/webapp/launch#(?:[^\s#]*?&)?ticket=)[A-Za-z0-9_-]{20,}`)
var signalPhoneRe = regexp.MustCompile(`(\+[1-9]\d{6,14})([^A-Za-z0-9]|$)`)
var strictURLParamRe = regexp.MustCompile(`([?#&;])([A-Za-z0-9_.~+%\-]+)=([^#&;\s"'<>]*)`)
var strictURLUserinfoRe = regexp.MustCompile(`//[^/\s?#@]+@`)
var formBodyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]*=[^&\s]*(?:&[A-Za-z_][A-Za-z0-9_.-]*=[^&\s]*)+$`)
var bearerResidueRe = regexp.MustCompile(`(?i)\bBearer\s+(?:\[[^\]]+\]|[A-Za-z0-9._~+/-]{20,}=*)`)

var sensitiveQueryParams = map[string]bool{
	"access_token": true, "refresh_token": true, "id_token": true, "token": true, "api_key": true, "apikey": true,
	"client_secret": true, "password": true, "auth": true, "jwt": true, "session": true, "secret": true, "key": true,
	"code": true, "signature": true, "x-amz-signature": true, "x-goog-signature": true, "sig": true,
}

func redactQueryString(query string) string {
	if query == "" {
		return query
	}
	parts := strings.Split(query, "&")
	for i, pair := range parts {
		key, _, found := strings.Cut(pair, "=")
		if found && sensitiveQueryParams[strings.ToLower(key)] {
			parts[i] = key + "=***"
		}
	}
	return strings.Join(parts, "&")
}

func canonicalURLParamName(name string) string {
	decoded := name
	for i := 0; i < 3; i++ {
		next, err := url.QueryUnescape(decoded)
		if err != nil || next == decoded {
			break
		}
		decoded = next
	}
	folded := strings.ToLower(decoded)
	if sensitiveQueryParams[folded] {
		return folded
	}
	return strings.ReplaceAll(folded, "-", "_")
}

func redactStrictURLCredentials(text string) string {
	text = strictURLParamRe.ReplaceAllStringFunc(text, func(m string) string {
		g := strictURLParamRe.FindStringSubmatch(m)
		if sensitiveQueryParams[canonicalURLParamName(g[2])] {
			return g[1] + g[2] + "=***"
		}
		return m
	})
	return strictURLUserinfoRe.ReplaceAllString(text, "//***:***@")
}

func redactFormBody(text string) string {
	trimmed := strings.TrimSpace(text)
	if text == "" || strings.Contains(text, "\n") || !strings.Contains(text, "&") || !formBodyRe.MatchString(trimmed) {
		return text
	}
	return redactQueryString(trimmed)
}

func redactURLCredentials(text string, codeFile bool) string {
	text = dbConnstrRe.ReplaceAllStringFunc(text, func(m string) string {
		g := dbConnstrRe.FindStringSubmatch(m)
		if codeFile && strings.HasPrefix(g[2], "{") && strings.HasSuffix(g[2], "}") {
			return m
		}
		return g[1] + "***" + g[3]
	})
	return urlBareTokenRe.ReplaceAllStringFunc(text, func(m string) string {
		g := urlBareTokenRe.FindStringSubmatch(m)
		return g[1] + maskToken(g[2]) + g[3]
	})
}

// Options of one redaction pass
type RedactOptions struct {
	CodeFile             bool
	FileRead             bool
	SecretFile           bool
	RedactURLCredentials bool
}

// Text with every credential shape masked, the force path every egress boundary runs
func RedactSensitiveText(text string, opts RedactOptions) string {
	return tools.RedactSensitiveTextWith(nil, text, tools.RedactOptions{Force: true, CodeFile: opts.CodeFile,
		FileRead: opts.FileRead, SecretFile: opts.SecretFile, URLCredentials: opts.RedactURLCredentials})
}

// Text scrubbed for a remote reader with the bearer sweep applied, never raw on failure
func RedactForEgress(text string) string { return tools.RedactForEgress(nil, text) }

var tokenRe = regexp.MustCompile(`[A-Za-z0-9_\-./+]{16,}`)

// Values scrubbed for a provider, every token the scrub masked anywhere masked everywhere
func RedactForProvider(values ...any) []any {
	learned := map[string]bool{}
	scrub := func(text string) string {
		out := RedactForEgress(text)
		if out != text {
			after := map[string]bool{}
			for _, t := range tokenRe.FindAllString(out, -1) {
				after[t] = true
			}
			for _, t := range tokenRe.FindAllString(text, -1) {
				if !after[t] {
					learned[t] = true
				}
			}
		}
		return out
	}
	scrubbed := make([]any, len(values))
	for i, v := range values {
		scrubbed[i] = walkStrings(v, scrub)
	}
	if len(learned) == 0 {
		return scrubbed
	}
	tokens := make([]string, 0, len(learned))
	for t := range learned {
		tokens = append(tokens, t)
	}
	sort.Slice(tokens, func(i, j int) bool {
		if len(tokens[i]) != len(tokens[j]) {
			return len(tokens[i]) > len(tokens[j])
		}
		return tokens[i] < tokens[j]
	})
	quoted := make([]string, len(tokens))
	for i, t := range tokens {
		quoted[i] = regexp.QuoteMeta(t)
	}
	pattern := regexp.MustCompile(strings.Join(quoted, "|"))
	for i, v := range scrubbed {
		scrubbed[i] = walkStrings(v, func(t string) string {
			if pattern.MatchString(t) {
				return pattern.ReplaceAllString(t, "[redacted]")
			}
			return t
		})
	}
	return scrubbed
}

// Copy of a value with every string except data URLs passed through fix
func walkStrings(value any, fix func(string) string) any {
	switch v := value.(type) {
	case string:
		if v == "" || strings.HasPrefix(v, "data:") {
			return v
		}
		return fix(v)
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, item := range v {
			out[k] = walkStrings(item, fix)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = walkStrings(item, fix)
		}
		return out
	case []string:
		out := make([]string, len(v))
		for i, item := range v {
			out[i] = walkStrings(item, fix).(string)
		}
		return out
	}
	return value
}

// Scrubbed string for a provider hand off
func redactString(text string) string {
	return RedactForProvider(text)[0].(string)
}
