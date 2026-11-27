// Package redact masks secrets in text a tool hands the model: tokens with a known vendor prefix, JWTs, private keys, credentials in
// URLs, headers and form bodies, and in text that holds secrets, such as a .env read or an environment dump, the values of assignments
// whose names say they are secret
package redact

import (
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Token patterns of known vendors, each starting with a literal prefix
var prefixPatterns = []string{
	`sk-[A-Za-z0-9_-](?:\.?[A-Za-z0-9_-]){9,}`,
	`ctx7sk-[A-Za-z0-9_-]{10,}`,
	`ghp_[A-Za-z0-9]{10,}`, `github_pat_[A-Za-z0-9_]{10,}`, `gho_[A-Za-z0-9]{10,}`, `ghu_[A-Za-z0-9]{10,}`,
	`ghs_[A-Za-z0-9]{10,}`, `ghr_[A-Za-z0-9]{10,}`,
	`xapp-\d+-[A-Za-z0-9-]{10,}`, `xox[baprs]-[A-Za-z0-9-]{10,}`,
	`AIza[A-Za-z0-9_-]{30,}`, `pplx-[A-Za-z0-9]{10,}`, `fal_[A-Za-z0-9_-]{10,}`, `fc-[A-Za-z0-9]{10,}`,
	`bb_live_[A-Za-z0-9_-]{10,}`, `gAAAA[A-Za-z0-9_=-]{20,}`, `AKIA[A-Z0-9]{16}`,
	`sk_live_[A-Za-z0-9]{10,}`, `sk_test_[A-Za-z0-9]{10,}`, `rk_live_[A-Za-z0-9]{10,}`,
	`SG\.[A-Za-z0-9_-]{10,}`, `hf_[A-Za-z0-9]{10,}`, `r8_[A-Za-z0-9]{10,}`, `npm_[A-Za-z0-9]{10,}`,
	`pypi-[A-Za-z0-9_-]{10,}`, `dop_v1_[A-Za-z0-9]{10,}`, `doo_v1_[A-Za-z0-9]{10,}`,
	`am_(?:org_)?[A-Za-z0-9]{20,}`, `sk_[A-Za-z0-9_]{10,}`, `tvly-[A-Za-z0-9]{10,}`, `exa_[A-Za-z0-9]{10,}`,
	`gsk_[A-Za-z0-9]{10,}`, `syt_[A-Za-z0-9]{10,}`, `retaindb_[A-Za-z0-9]{10,}`, `hsk-[A-Za-z0-9]{10,}`,
	`mem0_[A-Za-z0-9]{10,}`, `brv_[A-Za-z0-9]{10,}`, `xai-[A-Za-z0-9]{30,}`, `ntn_[A-Za-z0-9]{10,}`,
	`fw-[A-Za-z0-9]{30,}`, `fw_[A-Za-z0-9]{30,}`, `fpk_[A-Za-z0-9]{30,}`,
	`glpat-[A-Za-z0-9_\-]{10,}`, `gloas-[A-Za-z0-9_\-]{10,}`, `gldt-[A-Za-z0-9_\-]{10,}`, `glrt-[A-Za-z0-9_.\-]{10,}`,
	`glrtr-[A-Za-z0-9_.\-]{10,}`, `glcbt-[A-Za-z0-9_\-]{10,}`, `glptt-[A-Za-z0-9_\-]{10,}`, `glft-[A-Za-z0-9_\-]{10,}`,
	`glimt-[A-Za-z0-9_\-]{10,}`, `glagent-[A-Za-z0-9_\-]{10,}`, `glsoat-[A-Za-z0-9_\-]{10,}`, `glffct-[A-Za-z0-9_\-]{10,}`,
	`glwt-[A-Za-z0-9_\-]{10,}`, `GR1348941[A-Za-z0-9_\-]{10,}`, `pk-lf-[A-Za-z0-9\-]{8,}`,
}

// Leading literal characters of a pattern, up to its first metacharacter
func literal(pattern string) string {
	if i := strings.IndexAny(pattern, `[(\.?*+|{^$`); i >= 0 {
		return pattern[:i]
	}
	return pattern
}

// Literal prefixes of the vendor patterns, the cheap test that any of them may be in a text
var prefixes = func() []string {
	var out []string
	for _, p := range prefixPatterns {
		if lit := literal(p); lit != "" && !slices.Contains(out, lit) {
			out = append(out, lit)
		}
	}
	return out
}()

const (
	secretEnvNames = `(?:API_?KEY|KEY|TOKEN|SECRET|PASSWORD|PASSWD|PASS|PW|CREDENTIAL|AUTH)`
	secretCfgNames = `(?:api[ _.\-]?key|token|secret|passwd|password|credential|auth)`
	yamlCfgNames   = `(?:api[ _.\-]?key|token|secret|passwd|password|credential)`
	gutter         = `(?:[0-9]+(?:[|:\-]|\t)[ \t]*)?`
	jsonKeyNames   = `(?:api_?[Kk]ey|token|secret|password|access_token|refresh_token|auth_token|bearer|secret_value|raw_secret|secret_input|key_material)`
	shellVarRef    = `\$(?:\{[A-Za-z_]\w*[^}]*\}|[A-Za-z_]\w*)`
	tokenBody      = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_-."
)

var (
	vendor          = regexp.MustCompile("(?:" + strings.Join(prefixPatterns, "|") + ")")
	zhipu           = regexp.MustCompile(`[0-9a-f]{32}\.[A-Za-z0-9]{16,}`)
	displayControls = regexp.MustCompile(`[\x00-\x1f\x7f\x{80}-\x{9f}\x{200b}-\x{200f}\x{202a}-\x{202e}\x{2060}-\x{2064}]`)
	envAssign       = regexp.MustCompile(`([A-Z0-9_]{0,50}` + secretEnvNames + `[A-Z0-9_]{0,50})\s*=\s*(['"]?)(\S+)`)
	envAssignLower  = regexp.MustCompile(`(?i)([a-z0-9_]+_(?:key|pass|pw|token|secret|password|passwd|credential|auth))\s*=\s*(['"]?)(\S+)`)
	cfgSecretWord   = regexp.MustCompile(`(?i)` + secretCfgNames)
	envLookup       = regexp.MustCompile(`^(?:os\.(?:getenv|environ)|process\.env|\$ENV\{)`)
	cfgDotted       = regexp.MustCompile(`(?i)([A-Za-z0-9_\-]+\.[A-Za-z0-9_.\-]*` + secretCfgNames + `[A-Za-z0-9_.\-]*|[A-Za-z0-9_.\-]*` + secretCfgNames + `[A-Za-z0-9_.\-]*\.[A-Za-z0-9_.\-]+)=(['"]?)([^\s&]+)`)
	cfgAnchored     = regexp.MustCompile(`(?im)(^[ \t]*` + gutter + `(?:export[ \t]+)?[A-Za-z0-9_\-]*` + secretCfgNames + `[A-Za-z0-9_\-]*)=(['"]?)([^\s&]+)`)
	yamlAssign      = regexp.MustCompile(`(?im)(^[ \t]*` + gutter + `[A-Za-z0-9_.\-]*` + yamlCfgNames + `[A-Za-z0-9_.\-]*)(:[ \t]*)([^\s&]+)`)
	keyKeyword      = regexp.MustCompile(`(?i)(?:api|auth|access|refresh|session|secret)[ _.\\-]?(?:key|token)|token|secret|passwd|password|pass|pw|credential|auth|key`)
	strongKeyword   = regexp.MustCompile(`(?i)(?:api|auth|access|refresh|session|id|bearer)[ _.\\-]?(?:key|token)|key[ _.\\-]?material|secret|passwd|password|pass|pw|credential|auth|bearer`)
	passwordKey     = regexp.MustCompile(`(?i)passwd|password|pass|pw`)
	pathOrVar       = regexp.MustCompile(`^(?:` + shellVarRef + `|\$\(|~|/)(?:[\w./:-]|` + shellVarRef + `)*$`)
	jsonField       = regexp.MustCompile(`(?i)("` + jsonKeyNames + `")\s*:\s*"([^"]+)"`)
	jsonEscapedKey  = regexp.MustCompile(`(?i)(\\+)"` + jsonKeyNames)
	reprField       = regexp.MustCompile(`'([A-Za-z_][A-Za-z0-9_]*)'(\s*:\s*)(?:([bB]?)'((?:\\.|[^'\\])+)'|([bB]?)"((?:\\.|[^"\\])+)")`)
	authHeader      = regexp.MustCompile(`(?i)((?:Proxy-)?Authorization:\s*)([A-Za-z][\w.+-]*\s+)?([^\s"']+)`)
	secretHeader    = regexp.MustCompile(`(?i)((?:x-api-key|x-goog-api-key|api-key|apikey|x-api-token|x-auth-token|x-access-token)\s*:\s*)(\S+)`)
	telegram        = regexp.MustCompile(`(bot)?(\d{8,}):([-A-Za-z0-9_]{30,})`)
	privateKey      = regexp.MustCompile(`-----BEGIN[A-Z ]*PRIVATE KEY-----[\s\S]*?-----END[A-Z ]*PRIVATE KEY-----`)
	dbConnection    = regexp.MustCompile(`(?i)((?:postgres(?:ql)?|mysql|mongodb(?:\+srv)?|redis|amqp)://[^:\s]+:)([^@\s]+)(@)`)
	urlBareToken    = regexp.MustCompile(`(?i)((?:https?|wss?|git|ssh|ftp|ftps|sftp)://)([^\s:@/]{8,})(@[^\s]+)`)
	jwt             = regexp.MustCompile(`eyJ[A-Za-z0-9_-]{10,}(?:\.[A-Za-z0-9_=-]{4,}){0,2}`)
	webappLink      = regexp.MustCompile(`(icebath-session=|/webapp/launch#(?:[^\s#]*?&)?ticket=)[A-Za-z0-9_-]{20,}`)
	phone           = regexp.MustCompile(`\+[1-9]\d{6,14}`)
	formBody        = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]*=[^&\s]*(?:&[A-Za-z_][A-Za-z0-9_.-]*=[^&\s]*)+$`)
	hexText         = regexp.MustCompile(`^[A-Fa-f0-9]+$`)
	opaqueToken     = regexp.MustCompile(`^[A-Za-z0-9_./+=-]+$`)
)

// Query and form parameters whose values are credentials
var sensitiveParams = map[string]bool{
	"access_token": true, "refresh_token": true, "id_token": true, "token": true, "api_key": true, "apikey": true, "client_secret": true,
	"password": true, "auth": true, "jwt": true, "session": true, "secret": true, "key": true, "code": true, "signature": true,
	"x-amz-signature": true, "x-goog-signature": true, "sig": true,
}

// Keys of Python mapping reprs whose values are credentials
var reprSecretKeys = map[string]bool{
	"access_token": true, "refresh_token": true, "id_token": true, "auth_token": true, "token": true, "api_key": true, "apikey": true,
	"client_secret": true, "secret": true, "password": true, "passwd": true, "private_key": true, "credential": true, "credentials": true,
	"authorization": true, "bearer": true, "secret_value": true, "raw_secret": true, "secret_input": true, "key_material": true,
}

var (
	reprEnvSuffixes        = []string{"_API_KEY", "_TOKEN", "_SECRET", "_PASSWORD", "_PASSWD", "_CREDENTIAL", "_CREDENTIALS"}
	reprCredentialSuffixes = []string{"apikey", "api_key", "token", "secret", "password", "passwd", "credential", "credentials"}
)

// Code points that split a token without being seen: controls and zero width characters
func control(r rune) bool {
	return r <= 0x1f || r == 0x7f || r >= 0x200b && r <= 0x200f || r >= 0x2028 && r <= 0x202f || r == 0x2060 || r == 0xfeff
}

// Text of a file the model reads with its secrets masked by a marker that keeps nothing of them; a file that holds secrets, such as a
// .env, also has the values of its secret named assignments masked
func File(text string, secret bool) string {
	return Text(text, secret, nonreusable)
}

// Secret shown by its first six and last four characters, or as *** when it is too short to show any of it safely
func Ends(value string) string {
	return token(value)
}

// Text with its secrets masked by the mask given, Ends or one that keeps nothing; a text that holds secrets, such as a .env read or an
// environment dump, also has the values of its secret named assignments masked
func Text(text string, secret bool, with func(string) string) string {
	if text == "" {
		return text
	}
	code := !secret
	if hasPrefix(text) {
		text = maskSplit(text, with)
		text = replace(text, vendorSpans(text), func(loc []int) string { return with(text[loc[0]:loc[1]]) })
	}
	if strings.Contains(text, ".") {
		text = replace(text, scan(zhipu, text, func(loc []int) bool {
			if r, ok := before(text, loc[0]); ok && (tokenRune(r) || r == '.') {
				return false
			}
			if r, ok := after(text, loc[1]); ok && (tokenRune(r) || r == '.') {
				return false
			}
			return true
		}), func(loc []int) string { return with(text[loc[0]:loc[1]]) })
	}
	if !code {
		text = assignments(text, with)
	}
	if strings.Contains(text, "uthorization") || strings.Contains(text, "UTHORIZATION") {
		text = authHeader.ReplaceAllStringFunc(text, func(m string) string {
			loc := authHeader.FindStringSubmatchIndex(m)
			return group(m, loc, 1) + group(m, loc, 2) + token(group(m, loc, 3))
		})
	}
	if strings.Contains(text, ":") {
		text = secretHeader.ReplaceAllStringFunc(text, func(m string) string {
			loc := secretHeader.FindStringSubmatchIndex(m)
			return group(m, loc, 1) + token(group(m, loc, 2))
		})
		text = replace(text, scan(telegram, text, func(loc []int) bool {
			r, ok := before(text, loc[0])
			return !ok || !unicode.IsDigit(r)
		}), func(loc []int) string { return group(text, loc, 1) + group(text, loc, 2) + ":***" })
	}
	if strings.Contains(text, "BEGIN") && strings.Contains(text, "-----") {
		text = privateKey.ReplaceAllString(text, "[REDACTED PRIVATE KEY]")
	}
	if strings.Contains(text, "://") {
		text = urlCredentials(text, code)
	}
	if strings.Contains(text, "eyJ") {
		text = jwt.ReplaceAllStringFunc(text, token)
	}
	if strings.Contains(text, "icebath-session=") || strings.Contains(text, "/webapp/launch#") {
		text = replace(text, scan(webappLink, text, func(loc []int) bool {
			if !strings.HasPrefix(text[loc[0]:], "icebath-session=") {
				return true
			}
			r, ok := before(text, loc[0])
			return !ok || !tokenRune(r)
		}), func(loc []int) string { return group(text, loc, 1) + "***" })
	}
	if strings.Contains(text, "&") && strings.Contains(text, "=") {
		text = form(text)
	}
	if strings.Contains(text, "+") {
		text = replace(text, scan(phone, text, func(loc []int) bool {
			r, ok := after(text, loc[1])
			return !ok || !(unicode.IsLetter(r) || unicode.IsDigit(r))
		}), func(loc []int) string { return maskPhone(text[loc[0]:loc[1]]) })
	}
	return text
}

var shellRC = map[string]bool{".bashrc": true, ".bash_profile": true, ".bash_login": true, ".profile": true, ".zshrc": true, ".zprofile": true,
	".zlogin": true, ".zshenv": true}

// Project env files that hold secrets, by base name
var EnvFiles = map[string]bool{".env": true, ".env.local": true, ".env.development": true, ".env.production": true, ".env.test": true,
	".env.staging": true, ".envrc": true}

// Whether a path names a file whose reads need the assignment passes: a project env file, a shell startup file, or a config.yaml of
// icebath's own, one in a .icebath folder or under one of the icebath folders given
func SecretFile(path string, icebath []string, under func(path, root string) bool) bool {
	p := strings.ReplaceAll(strings.Trim(path, `"'`), `\`, "/")
	own := false
	for _, prefix := range []string{"$ICEBATH_HOME/", "${ICEBATH_HOME}/"} {
		if rest, ok := strings.CutPrefix(p, prefix); ok {
			p, own = rest, true
			break
		}
	}
	for _, prefix := range []string{"$HOME/", "${HOME}/"} {
		if rest, ok := strings.CutPrefix(p, prefix); ok {
			p = rest
			break
		}
	}
	if strings.Contains(p, "$") {
		return false
	}
	var parts []string
	for _, part := range strings.Split(p, "/") {
		if part != "" {
			parts = append(parts, strings.ToLower(part))
		}
	}
	if len(parts) == 0 {
		return false
	}
	last := parts[len(parts)-1]
	if EnvFiles[last] || shellRC[last] {
		return true
	}
	if last != "config.yaml" && !strings.HasPrefix(last, "config.yaml.good.") && !strings.HasPrefix(last, "config.yaml.corrupt.") {
		return false
	}
	if own || slices.Contains(parts[:len(parts)-1], ".icebath") {
		return true
	}
	for _, root := range icebath {
		if under(path, root) {
			return true
		}
	}
	return false
}

func hasPrefix(text string) bool {
	for _, p := range prefixes {
		if strings.Contains(text, p) {
			return true
		}
	}
	return false
}

// Spans of every match the check accepts, the search resuming one character past a refused start
func scan(re *regexp.Regexp, text string, ok func(loc []int) bool) [][]int {
	var out [][]int
	for pos := 0; pos <= len(text); {
		loc := re.FindStringSubmatchIndex(text[pos:])
		if loc == nil {
			break
		}
		for i := range loc {
			if loc[i] >= 0 {
				loc[i] += pos
			}
		}
		if ok == nil || ok(loc) {
			out = append(out, loc)
			pos = loc[1]
			if loc[1] == loc[0] {
				pos++
			}
			continue
		}
		_, size := utf8.DecodeRuneInString(text[loc[0]:])
		pos = loc[0] + max(size, 1)
	}
	return out
}

// Spans a substitution rewrites: a refused match tried again one character on, a match the rewrite looked at resuming where it ended,
// and for a line anchored pattern only matches at the start of a line
func scanRewrites(re *regexp.Regexp, text string, anchored bool, ok func(loc []int) bool, rewrite func(loc []int) (bool, int)) [][]int {
	var out [][]int
	for pos := 0; pos <= len(text); {
		loc := re.FindStringSubmatchIndex(text[pos:])
		if loc == nil {
			break
		}
		for i := range loc {
			if loc[i] >= 0 {
				loc[i] += pos
			}
		}
		if anchored && loc[0] > 0 && text[loc[0]-1] != '\n' {
			next := strings.IndexByte(text[loc[0]:], '\n')
			if next < 0 {
				break
			}
			pos = loc[0] + next + 1
			continue
		}
		if ok != nil && !ok(loc) {
			_, size := utf8.DecodeRuneInString(text[loc[0]:])
			pos = loc[0] + max(size, 1)
			continue
		}
		rewritten, end := rewrite(loc)
		if rewritten {
			out = append(out, loc)
		}
		pos = max(end, loc[0]+1)
	}
	return out
}

// Text with each span replaced by what the callback makes of it
func replace(text string, locs [][]int, with func(loc []int) string) string {
	if len(locs) == 0 {
		return text
	}
	var b strings.Builder
	last := 0
	for _, loc := range locs {
		b.WriteString(text[last:loc[0]])
		b.WriteString(with(loc))
		last = loc[1]
	}
	b.WriteString(text[last:])
	return b.String()
}

func group(text string, loc []int, i int) string {
	if loc[2*i] < 0 {
		return ""
	}
	return text[loc[2*i]:loc[2*i+1]]
}

func before(text string, at int) (rune, bool) {
	if at <= 0 {
		return 0, false
	}
	r, _ := utf8.DecodeLastRuneInString(text[:at])
	return r, true
}

func after(text string, at int) (rune, bool) {
	if at >= len(text) {
		return 0, false
	}
	r, _ := utf8.DecodeRuneInString(text[at:])
	return r, true
}

func tokenRune(r rune) bool {
	return r == '_' || r == '-' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9'
}

// Vendor tokens not glued to identifier characters
func vendorSpans(text string) [][]int {
	return scan(vendor, text, func(loc []int) bool {
		if r, ok := before(text, loc[0]); ok && tokenRune(r) {
			return false
		}
		if r, ok := after(text, loc[1]); ok && tokenRune(r) {
			return false
		}
		return true
	})
}

// A secret shown by its first and last characters, or as *** when it is shorter than floor
func mask(value string, head, tail, floor int) string {
	if value = displayControls.ReplaceAllString(value, ""); value == "" {
		return ""
	}
	runes := []rune(value)
	if len(runes) < floor {
		return "***"
	}
	return string(runes[:head]) + "..." + string(runes[len(runes)-tail:])
}

func token(value string) string {
	if value == "" {
		return "***"
	}
	return mask(value, 6, 4, 18)
}

// Marker that keeps only the vendor prefix of a token, nothing of it that could be used
func nonreusable(value string) string {
	for _, p := range prefixes {
		if value != "" && strings.HasPrefix(value, p) {
			return "«redacted:" + p + "…»"
		}
	}
	return "«redacted-secret»"
}

// Vendor tokens split by control or zero width characters masked whole
func maskSplit(text string, with func(string) string) string {
	if !strings.ContainsFunc(text, control) {
		return text
	}
	var stripped strings.Builder
	var at []int
	for i, r := range text {
		if !control(r) {
			at = append(at, i)
			stripped.WriteRune(r)
		}
	}
	s := stripped.String()
	runeAt := map[int]int{}
	n := 0
	for i := range s {
		runeAt[i] = n
		n++
	}
	type span struct {
		start, end int
		text       string
	}
	var spans []span
	for _, loc := range vendorSpans(s) {
		start := at[runeAt[loc[0]]]
		lastRune, ok := runeAt[loc[1]]
		if !ok {
			lastRune = len(at)
		}
		lastByte := at[lastRune-1]
		_, size := utf8.DecodeRuneInString(text[lastByte:])
		end := lastByte + size
		piece := text[start:end]
		if strings.ContainsAny(piece, "\n\r") && len(vendorSpans(piece)) > 0 {
			continue
		}
		clean := true
		for _, r := range piece {
			if !strings.ContainsRune(tokenBody, r) && !control(r) {
				clean = false
				break
			}
		}
		if clean && (end >= len(text) || text[end] != '=') {
			spans = append(spans, span{start, end, with(s[loc[0]:loc[1]])})
		}
	}
	locs := make([][]int, len(spans))
	for i, s := range spans {
		locs[i] = []int{s.start, s.end}
	}
	i := 0
	return replace(text, locs, func([]int) string {
		out := spans[i].text
		i++
		return out
	})
}

func wordStart(s []rune, i int) bool {
	if i == 0 {
		return true
	}
	prev, cur := s[i-1], s[i]
	if !unicode.IsLetter(prev) || unicode.IsUpper(cur) && unicode.IsLower(prev) {
		return true
	}
	return unicode.IsUpper(cur) && unicode.IsUpper(prev) && i+1 < len(s) && unicode.IsLower(s[i+1])
}

func wordEnd(s []rune, j int, plural bool) bool {
	if j >= len(s) {
		return true
	}
	cur := s[j]
	if !unicode.IsLetter(cur) || unicode.IsUpper(cur) && unicode.IsLower(s[j-1]) {
		return true
	}
	return plural && (cur == 's' || cur == 'S') && wordEnd(s, j+1, false)
}

// Whether the keyword pattern matches the key as a word of it, camel case and separators splitting words
func keyword(key string, re *regexp.Regexp) bool {
	runes := []rune(key)
	runeAt := map[int]int{}
	n := 0
	for i := range key {
		runeAt[i] = n
		n++
	}
	runeAt[len(key)] = n
	for _, loc := range re.FindAllStringIndex(key, -1) {
		if wordStart(runes, runeAt[loc[0]]) && wordEnd(runes, runeAt[loc[1]], true) {
			return true
		}
	}
	return false
}

// Whether a value has the shape of a credential: a marker, long hex, a long opaque token, or mixed case and digits
func opaque(value string) bool {
	if value == "***" || strings.HasPrefix(value, "«redacted:") {
		return true
	}
	n := utf8.RuneCountInString(value)
	if n >= 16 && hexText.MatchString(value) || n >= 20 && opaqueToken.MatchString(value) {
		return true
	}
	if n < 12 {
		return false
	}
	classes := 0
	for _, set := range []string{"abcdefghijklmnopqrstuvwxyz", "ABCDEFGHIJKLMNOPQRSTUVWXYZ", "0123456789"} {
		if strings.ContainsAny(value, set) {
			classes++
		}
	}
	return classes >= 2
}

// Whether a path segment without dots is long and mixes case and digits the way a secret does
func opaqueSegment(segment string) bool {
	if strings.Contains(segment, ".") || utf8.RuneCountInString(segment) < 16 {
		return false
	}
	return strings.ContainsAny(segment, "abcdefghijklmnopqrstuvwxyz") && strings.ContainsAny(segment, "ABCDEFGHIJKLMNOPQRSTUVWXYZ") &&
		strings.ContainsAny(segment, "0123456789")
}

// Whether an assignment of the value to the key is a secret to mask
func secretAssignment(key, value string, checkKeyword bool) bool {
	if envLookup.MatchString(value) || value == "***" || strings.HasPrefix(value, "«redacted") {
		return false
	}
	if checkKeyword && !keyword(key, keyKeyword) {
		return false
	}
	if pathOrVar.MatchString(value) && !keyword(key, passwordKey) {
		opaquePath := false
		for _, segment := range strings.Split(value, "/") {
			if opaqueSegment(segment) {
				opaquePath = true
				break
			}
		}
		if value[0] == '$' || !opaquePath {
			return false
		}
	}
	return keyword(key, strongKeyword) || opaque(value)
}

// Quote and value of a greedy quoted assignment, cut at its last closing quote, and where the match really ends
func greedyValue(text string, loc []int) (string, string, int) {
	quote, run := group(text, loc, 2), group(text, loc, 3)
	if quote == "" {
		return "", run, loc[1]
	}
	if i := strings.LastIndex(run, quote); i >= 1 {
		return quote, run[:i], loc[6] + i + 1
	}
	return "", quote + run, loc[1]
}

// Quote and value of a config assignment whose closing quote must end its value
func lazyValue(text string, loc []int) (string, string) {
	quote, run := group(text, loc, 2), group(text, loc, 3)
	if quote == "" {
		return "", run
	}
	if len(run) >= 2 && strings.HasSuffix(run, quote) {
		return quote, run[:len(run)-1]
	}
	return "", quote + run
}

func cfgKeyRune(r rune) bool { return r == '.' || tokenRune(r) }

// Values of env, config, JSON and YAML assignments to secret names masked with the mask given
func assignments(text string, with func(string) string) string {
	if strings.Contains(text, "=") {
		env := func(re *regexp.Regexp, ok func(loc []int) bool) {
			locs := scanRewrites(re, text, false, ok, func(loc []int) (bool, int) {
				_, value, end := greedyValue(text, loc)
				return secretAssignment(group(text, loc, 1), value, true), end
			})
			if len(locs) == 0 {
				return
			}
			var b strings.Builder
			last := 0
			for _, loc := range locs {
				quote, value, end := greedyValue(text, loc)
				b.WriteString(text[last:loc[0]])
				b.WriteString(group(text, loc, 1) + "=" + quote + with(value) + quote)
				last = end
			}
			b.WriteString(text[last:])
			text = b.String()
		}
		env(envAssign, nil)
		if !strings.Contains(text, "://") {
			env(envAssignLower, func(loc []int) bool {
				r, ok := before(text, loc[0])
				return !ok || !(r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r))
			})
		}
		if !strings.Contains(text, "://") && cfgSecretWord.MatchString(text) {
			cfg := func(re *regexp.Regexp, anchored bool, ok func(loc []int) bool) {
				locs := scanRewrites(re, text, anchored, ok, func(loc []int) (bool, int) {
					_, value := lazyValue(text, loc)
					return secretAssignment(group(text, loc, 1), value, true), loc[1]
				})
				text = replace(text, locs, func(loc []int) string {
					quote, value := lazyValue(text, loc)
					return group(text, loc, 1) + "=" + quote + with(value) + quote
				})
			}
			cfg(cfgDotted, false, func(loc []int) bool {
				r, ok := before(text, loc[0])
				return !ok || !cfgKeyRune(r)
			})
			cfg(cfgAnchored, true, nil)
		}
	}
	if strings.Contains(text, ":") && strings.Contains(text, `"`) {
		locs := scanRewrites(jsonField, text, false, nil, func(loc []int) (bool, int) {
			return secretAssignment(group(text, loc, 1), group(text, loc, 2), false), loc[1]
		})
		text = replace(text, locs, func(loc []int) string { return group(text, loc, 1) + `: "` + with(group(text, loc, 2)) + `"` })
		if strings.Contains(text, `\"`) {
			text = escapedJSON(text, with)
		}
	}
	if strings.Contains(text, ":") && strings.Contains(text, "'") {
		text = reprFields(text)
	}
	if strings.Contains(text, ":") && !strings.Contains(text, "://") {
		locs := scanRewrites(yamlAssign, text, true, nil, func(loc []int) (bool, int) {
			value := group(text, loc, 3)
			if strings.HasPrefix(value, "'") || strings.HasPrefix(value, `"`) {
				return false, loc[1]
			}
			return secretAssignment(group(text, loc, 1), value, true), loc[1]
		})
		text = replace(text, locs, func(loc []int) string {
			return group(text, loc, 1) + group(text, loc, 2) + with(group(text, loc, 3))
		})
	}
	return text
}

// Credential fields masked inside a JSON document carried as an escaped string
func escapedJSON(text string, with func(string) string) string {
	type span struct {
		start, end int
		text       string
	}
	var spans []span
	blank := func(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }
	for pos := 0; pos < len(text); {
		loc := jsonEscapedKey.FindStringSubmatchIndex(text[pos:])
		if loc == nil {
			break
		}
		start, keyEnd := pos+loc[0], pos+loc[1]
		closer := text[pos+loc[2]:pos+loc[3]] + `"`
		rest := text[keyEnd:]
		pos = start + 1
		if !strings.HasPrefix(rest, closer) {
			continue
		}
		i := len(closer)
		for i < len(rest) && blank(rest[i]) {
			i++
		}
		if i >= len(rest) || rest[i] != ':' {
			continue
		}
		for i++; i < len(rest) && blank(rest[i]); i++ {
		}
		if !strings.HasPrefix(rest[i:], closer) {
			continue
		}
		valueStart := i + len(closer)
		j := valueStart
		for j < len(rest) && rest[j] != '"' && rest[j] != '\\' {
			j++
		}
		if j == valueStart || !strings.HasPrefix(rest[j:], closer) {
			continue
		}
		value := rest[valueStart:j]
		end := keyEnd + j + len(closer)
		key := text[start:keyEnd] + closer
		if secretAssignment(key, value, false) {
			spans = append(spans, span{start, end, key + ": " + closer + with(value) + closer})
		}
		pos = end
	}
	locs := make([][]int, len(spans))
	for i, s := range spans {
		locs[i] = []int{s.start, s.end}
	}
	i := 0
	return replace(text, locs, func([]int) string {
		out := spans[i].text
		i++
		return out
	})
}

func reprSecret(key string) bool {
	folded := strings.ToLower(key)
	if reprSecretKeys[folded] {
		return true
	}
	if key == strings.ToUpper(key) && key != folded {
		for _, suffix := range reprEnvSuffixes {
			if strings.HasSuffix(key, suffix) {
				return true
			}
		}
	}
	for _, suffix := range reprCredentialSuffixes {
		if strings.HasSuffix(folded, suffix) {
			return true
		}
	}
	return false
}

// Credential fields of Python mapping reprs masked whole
func reprFields(text string) string {
	return reprField.ReplaceAllStringFunc(text, func(m string) string {
		loc := reprField.FindStringSubmatchIndex(m)
		key := group(m, loc, 1)
		if !reprSecret(key) {
			return m
		}
		prefix, quote, value := group(m, loc, 5), `"`, group(m, loc, 6)
		if loc[8] >= 0 {
			prefix, quote, value = group(m, loc, 3), "'", group(m, loc, 4)
		}
		if envLookup.MatchString(value) || strings.Contains(value, "***") || strings.HasPrefix(value, "«redacted:") {
			return m
		}
		return "'" + key + "'" + group(m, loc, 2) + prefix + quote + "***" + quote
	})
}

// Text that is nothing but a form body with its sensitive values masked
func form(text string) string {
	trimmed := strings.TrimSpace(text)
	if strings.Contains(text, "\n") || !strings.Contains(text, "&") || !formBody.MatchString(trimmed) {
		return text
	}
	pairs := strings.Split(trimmed, "&")
	for i, pair := range pairs {
		if key, _, found := strings.Cut(pair, "="); found && sensitiveParams[strings.ToLower(key)] {
			pairs[i] = key + "=***"
		}
	}
	return strings.Join(pairs, "&")
}

// Database connection passwords and bare tokens in URL user info masked; a code file's {placeholder} password kept
func urlCredentials(text string, code bool) string {
	text = dbConnection.ReplaceAllStringFunc(text, func(m string) string {
		loc := dbConnection.FindStringSubmatchIndex(m)
		password := group(m, loc, 2)
		if code && strings.HasPrefix(password, "{") && strings.HasSuffix(password, "}") {
			return m
		}
		return group(m, loc, 1) + "***" + group(m, loc, 3)
	})
	return urlBareToken.ReplaceAllStringFunc(text, func(m string) string {
		loc := urlBareToken.FindStringSubmatchIndex(m)
		return group(m, loc, 1) + token(group(m, loc, 2)) + group(m, loc, 3)
	})
}

func maskPhone(number string) string {
	keep := 4
	if len(number) <= 8 {
		keep = 2
	}
	return number[:keep] + "****" + number[len(number)-keep:]
}
