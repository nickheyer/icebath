package redact

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	gutter    = `(?:[0-9]+(?:[|:\-]|\t)[ \t]*)?`
	foldExtra = `\x{130}\x{131}`
)

var (
	cfgNames  = `(?:` + fi("api") + `[ _.\-]?key|token|secret|passwd|password|` + fi("credential") + `|auth)`
	yamlNames = `(?:` + fi("api") + `[ _.\-]?key|token|secret|passwd|password|` + fi("credential") + `)`

	reEnvUpper = regexp.MustCompile(`([A-Z0-9_]{0,50}(?:API_?KEY|KEY|TOKEN|SECRET|PASSWORD|PASSWD|PASS|PW|CREDENTIAL|AUTH)[A-Z0-9_]{0,50})` +
		sp + `*=` + sp + `*(` + nsp + `+)`)
	reEnvLower = regexp.MustCompile(`(?i)([a-z0-9_` + foldExtra + `]+_(?:key|pass|pw|token|secret|password|passwd|` + fi("credential") + `|auth))` +
		sp + `*=` + sp + `*(` + nsp + `+)`)
	reCfgSecretWord = regexp.MustCompile(`(?i)` + cfgNames)
	reCfgAnchored   = regexp.MustCompile(`(?i)^([ \t]*` + gutter + `(?:export[ \t]+)?[A-Za-z0-9_\-` + foldExtra + `]*` + cfgNames +
		`[A-Za-z0-9_\-` + foldExtra + `]*)=`)
	reYAMLAssign = regexp.MustCompile(`(?i)^([ \t]*` + gutter + `[A-Za-z0-9_.\-` + foldExtra + `]*` + yamlNames +
		`[A-Za-z0-9_.\-` + foldExtra + `]*)(:[ \t]*)`)
	reJSONField = regexp.MustCompile(`(?i)("(?:` + fi("api") + `_?[Kk]ey|token|secret|password|access_token|refresh_token|auth_token|bearer|secret_value|raw_secret|` +
		fi("secret_input") + `|` + fi("key_material") + `)")` + sp + `*:` + sp + `*"([^"]+)"`)
	reReprField     = regexp.MustCompile(`'([A-Za-z_][A-Za-z0-9_]*)'(` + sp + `*:` + sp + `*)(?:([bB]?)'((?:\\.|[^'\\])+)'|([bB]?)"((?:\\.|[^"\\])+)")`)
	rePytestLine    = regexp.MustCompile(`^([ \t]*E[ \t]{2,})(.*)$`)
	reExceptionLine = regexp.MustCompile(`^((?:[A-Za-z_][\p{L}\p{N}_]*\.)*[A-Za-z_][\p{L}\p{N}_]*(?:Error|Exception|Warning):[ \t]*)(.*)$`)

	reKeyKeyword = regexp.MustCompile(`(?i)(?:` + fi("api") + `|auth|access|refresh|` + fi("session") + `|secret)[ _.\\-]?(?:key|token)` +
		`|token|secret|passwd|password|pass|pw|` + fi("credential") + `|auth|key`)
	reStrongKeyword = regexp.MustCompile(`(?i)(?:` + fi("api") + `|auth|access|refresh|` + fi("session") + `|` + fi("id") + `|bearer)[ _.\\-]?(?:key|token)` +
		`|key[ _.\\-]?` + fi("material") + `|secret|passwd|password|pass|pw|` + fi("credential") + `|auth|bearer`)
	rePasswordKeyword = regexp.MustCompile(`(?i)passwd|password|pass|pw`)

	rePathOrVar = regexp.MustCompile(`^(?:` + shellVarRef + `|\$\(|~|/)(?:[\p{L}\p{N}_./:-]|` + shellVarRef + `)*\n?\z`)
)

const shellVarRef = `\$(?:\{[A-Za-z_][\p{L}\p{N}_]*[^}]*\}|[A-Za-z_][\p{L}\p{N}_]*)`

var reprSecretKeys = map[string]bool{
	"access_token": true, "refresh_token": true, "id_token": true, "auth_token": true, "token": true,
	"api_key": true, "apikey": true, "client_secret": true, "secret": true, "password": true, "passwd": true,
	"private_key": true, "credential": true, "credentials": true, "authorization": true, "bearer": true,
	"secret_value": true, "raw_secret": true, "secret_input": true, "key_material": true,
}

var reprEnvSuffixes = []string{"_API_KEY", "_TOKEN", "_SECRET", "_PASSWORD", "_PASSWD", "_CREDENTIAL", "_CREDENTIALS"}

var reprCredentialSuffixes = []string{"apikey", "api_key", "token", "secret", "password", "passwd", "credential", "credentials"}

var jsonEscapedNames = []string{
	"api_key", "apikey", "token", "secret", "password", "access_token", "refresh_token", "auth_token",
	"bearer", "secret_value", "raw_secret", "secret_input", "key_material",
}

var cfgNameWords = []string{"token", "secret", "passwd", "password", "credential", "auth"}

type assignRender func(key, quote, value string) string

func hasAnySuffix(s string, suffixes []string) bool {
	for _, suffix := range suffixes {
		if strings.HasSuffix(s, suffix) {
			return true
		}
	}
	return false
}

func isWordStart(s string, i int) bool {
	if i == 0 {
		return true
	}
	prev := prevRune(s, i)
	cur, w := utf8.DecodeRuneInString(s[i:])
	if !isAlphaRune(prev) || (pyIsUpper(cur) && pyIsLower(prev)) {
		return true
	}
	if i+w >= len(s) {
		return false
	}
	next, _ := utf8.DecodeRuneInString(s[i+w:])
	return pyIsUpper(cur) && pyIsUpper(prev) && pyIsLower(next)
}

func isWordEnd(s string, j int, allowPlural bool) bool {
	if j >= len(s) {
		return true
	}
	cur, w := utf8.DecodeRuneInString(s[j:])
	if !isAlphaRune(cur) || (pyIsUpper(cur) && pyIsLower(prevRune(s, j))) {
		return true
	}
	return allowPlural && (cur == 's' || cur == 'S') && isWordEnd(s, j+w, false)
}

func wordBounded(key string, re *regexp.Regexp) bool {
	for _, loc := range re.FindAllStringIndex(key, -1) {
		if isWordStart(key, loc[0]) && isWordEnd(key, loc[1], true) {
			return true
		}
	}
	return false
}

func KeyHasSecretKeyword(key string) bool {
	return wordBounded(key, reKeyKeyword)
}

func isEnvLookup(value string) bool {
	for _, p := range []string{"os.getenv", "os.environ", "process.env", "$ENV{"} {
		if strings.HasPrefix(value, p) {
			return true
		}
	}
	return false
}

func isOpaqueSegment(seg string) bool {
	if strings.Contains(seg, ".") || utf8.RuneCountInString(seg) < 16 {
		return false
	}
	var lower, upper, digit bool
	for i := 0; i < len(seg); i++ {
		c := seg[i]
		lower = lower || (c >= 'a' && c <= 'z')
		upper = upper || (c >= 'A' && c <= 'Z')
		digit = digit || (c >= '0' && c <= '9')
	}
	return lower && upper && digit
}

func allBytes(s string, ok func(byte) bool) bool {
	for i := 0; i < len(s); i++ {
		if !ok(s[i]) {
			return false
		}
	}
	return true
}

func isOpaqueChar(c byte) bool {
	return isAlnum(c) || strings.IndexByte("_./+=-", c) >= 0
}

func looksOpaque(value string) bool {
	if value == "***" || strings.HasPrefix(value, "«redacted:") {
		return true
	}
	n := utf8.RuneCountInString(value)
	if n >= 16 && allBytes(value, isHexDigit) {
		return true
	}
	if n >= 20 && allBytes(value, isOpaqueChar) {
		return true
	}
	if n < 12 {
		return false
	}
	var lower, upper, digit bool
	for i := 0; i < len(value); i++ {
		c := value[i]
		lower = lower || (c >= 'a' && c <= 'z')
		upper = upper || (c >= 'A' && c <= 'Z')
		digit = digit || (c >= '0' && c <= '9')
	}
	kinds := 0
	for _, k := range []bool{lower, upper, digit} {
		if k {
			kinds++
		}
	}
	return kinds >= 2
}

func shouldRedact(key, value string, checkKeyword bool) bool {
	if isEnvLookup(value) {
		return false
	}
	if value == "***" || strings.HasPrefix(value, "«redacted") {
		return false
	}
	if checkKeyword && !KeyHasSecretKeyword(key) {
		return false
	}
	if rePathOrVar.MatchString(value) && !wordBounded(key, rePasswordKeyword) {
		opaque := false
		for _, seg := range strings.Split(value, "/") {
			if isOpaqueSegment(seg) {
				opaque = true
				break
			}
		}
		if value[0] == '$' || !opaque {
			return false
		}
	}
	return wordBounded(key, reStrongKeyword) || looksOpaque(value)
}

func redactAssignments(text string, mask func(string) string) string {
	env := func(key, quote, value string) string { return key + "=" + quote + mask(value) + quote }
	if strings.Contains(text, "=") {
		text = redactEnvAssign(text, reEnvUpper, nil, env)
		if !strings.Contains(text, "://") {
			text = redactEnvAssign(text, reEnvLower, isFoldWord, env)
		}
		if !strings.Contains(text, "://") && reCfgSecretWord.MatchString(text) {
			text = redactCfgDotted(text, env)
			text = mapLines(text, func(line string) string { return redactCfgAnchoredLine(line, env) })
		}
	}
	if strings.Contains(text, ":") && strings.Contains(text, `"`) {
		text = replaceSubmatch(reJSONField, text, func(m []int) string {
			key, value := group(text, m, 1), group(text, m, 2)
			if !shouldRedact(key, value, false) {
				return text[m[0]:m[1]]
			}
			return key + `: "` + mask(value) + `"`
		})
		if strings.Contains(text, `\"`) {
			text = redactJSONEscaped(text, mask)
		}
	}
	if strings.Contains(text, ":") && strings.Contains(text, "'") {
		text = redactReprFields(text)
	}
	if strings.Contains(text, ":") && !strings.Contains(text, "://") {
		text = mapLines(text, func(line string) string { return redactYAMLLine(line, mask) })
	}
	return text
}

func quotedValue(text string, vs, ve int) (quote, value string, end int) {
	run := text[vs:ve]
	if len(run) >= 3 && (run[0] == '\'' || run[0] == '"') {
		if j := strings.LastIndexByte(run, run[0]); j >= 2 {
			return run[:1], run[1:j], vs + j + 1
		}
	}
	return "", run, ve
}

func redactEnvAssign(text string, re *regexp.Regexp, runClass func(rune) bool, render assignRender) string {
	var b strings.Builder
	last, from := 0, 0
	for from < len(text) {
		loc := re.FindStringSubmatchIndex(text[from:])
		if loc == nil {
			break
		}
		start := from + loc[0]
		if runClass != nil && start > 0 && runClass(prevRune(text, start)) {
			next := start
			for next < len(text) {
				r, w := utf8.DecodeRuneInString(text[next:])
				if !runClass(r) {
					break
				}
				next += w
			}
			from = next
			continue
		}
		key := text[from+loc[2] : from+loc[3]]
		quote, value, end := quotedValue(text, from+loc[4], from+loc[5])
		if shouldRedact(key, value, true) {
			b.WriteString(text[last:start])
			b.WriteString(render(key, quote, value))
			last = end
		}
		from = end
	}
	if last == 0 {
		return text
	}
	b.WriteString(text[last:])
	return b.String()
}

func valueRunEnd(text string, v0 int) int {
	i := v0
	for i < len(text) {
		r, w := utf8.DecodeRuneInString(text[i:])
		if isPySpace(r) || r == '&' {
			break
		}
		i += w
	}
	return i
}

func cfgQuote(run string) (quote, value string) {
	if len(run) >= 3 && (run[0] == '\'' || run[0] == '"') && run[len(run)-1] == run[0] {
		return run[:1], run[1 : len(run)-1]
	}
	return "", run
}

func cfgRunEnd(text string, p int, withDot bool) int {
	for p < len(text) {
		r, w := utf8.DecodeRuneInString(text[p:])
		if !isCfgKey(r) || (!withDot && r == '.') {
			break
		}
		p += w
	}
	return p
}

func cfgNameAt(text string, t, limit int) int {
	s := text[t:limit]
	if n := matchFold(s, "api"); n >= 0 {
		if n < len(s) && strings.IndexByte(" _.-", s[n]) >= 0 {
			if k := matchFold(s[n+1:], "key"); k >= 0 {
				return t + n + 1 + k
			}
		}
		if k := matchFold(s[n:], "key"); k >= 0 {
			return t + n + k
		}
	}
	for _, word := range cfgNameWords {
		if n := matchFold(s, word); n >= 0 {
			return t + n
		}
	}
	return -1
}

func firstNameEnd(text string, from, limit int) int {
	best := -1
	for t := from; t < limit; {
		if u := cfgNameAt(text, t, limit); u >= 0 && (best < 0 || u < best) {
			best = u
		}
		_, w := utf8.DecodeRuneInString(text[t:])
		t += w
	}
	return best
}

func dotWithTail(text string, from, limit int) bool {
	for d := from; d < limit; d++ {
		if text[d] == '.' && d+1 < limit {
			return true
		}
	}
	return false
}

func apiBefore(text string, p, q0 int) int {
	t := q0
	for k := 0; k < 3; k++ {
		if t <= p {
			return -1
		}
		_, w := utf8.DecodeLastRuneInString(text[:t])
		t -= w
	}
	if matchFold(text[t:q0], "api") != q0-t {
		return -1
	}
	return t
}

func dottedKeyEnd(text string, p, q0 int) (int, bool) {
	if q0 >= len(text) {
		return 0, false
	}
	r1 := cfgRunEnd(text, p, false)
	switch text[q0] {
	case '=':
		if r1 > p && r1 < q0 && text[r1] == '.' && firstNameEnd(text, r1+1, q0) >= 0 {
			return q0, true
		}
		if u := firstNameEnd(text, p, q0); u >= 0 && dotWithTail(text, u, q0) {
			return q0, true
		}
	case ' ':
		t := apiBefore(text, p, q0)
		if t < 0 {
			return 0, false
		}
		k := matchFold(text[q0+1:], "key")
		if k < 0 {
			return 0, false
		}
		u := q0 + 1 + k
		q1 := cfgRunEnd(text, u, true)
		if q1 >= len(text) || text[q1] != '=' {
			return 0, false
		}
		if (r1 > p && r1 < q0 && text[r1] == '.' && r1+1 <= t) || dotWithTail(text, u, q1) {
			return q1, true
		}
	}
	return 0, false
}

func redactCfgDotted(text string, render assignRender) string {
	var b strings.Builder
	last := 0
	for p := 0; p < len(text); {
		r, w := utf8.DecodeRuneInString(text[p:])
		if !isCfgKey(r) {
			p += w
			continue
		}
		q0 := cfgRunEnd(text, p, true)
		if p > 0 && isCfgKey(prevRune(text, p)) {
			p = q0
			continue
		}
		if keyEnd, ok := dottedKeyEnd(text, p, q0); ok {
			v0 := keyEnd + 1
			if ve := valueRunEnd(text, v0); ve > v0 {
				key := text[p:keyEnd]
				quote, value := cfgQuote(text[v0:ve])
				if shouldRedact(key, value, true) {
					b.WriteString(text[last:p])
					b.WriteString(render(key, quote, value))
					last = ve
				}
				p = ve
				continue
			}
		}
		p = q0
	}
	if last == 0 {
		return text
	}
	b.WriteString(text[last:])
	return b.String()
}

func redactCfgAnchoredLine(line string, render assignRender) string {
	loc := reCfgAnchored.FindStringSubmatchIndex(line)
	if loc == nil {
		return line
	}
	v0 := loc[1]
	ve := valueRunEnd(line, v0)
	if ve == v0 {
		return line
	}
	key := line[loc[2]:loc[3]]
	quote, value := cfgQuote(line[v0:ve])
	if !shouldRedact(key, value, true) {
		return line
	}
	return render(key, quote, value) + line[ve:]
}

func redactYAMLLine(line string, mask func(string) string) string {
	loc := reYAMLAssign.FindStringSubmatchIndex(line)
	if loc == nil {
		return line
	}
	v0 := loc[1]
	if v0 < len(line) && (line[v0] == '\'' || line[v0] == '"') {
		return line
	}
	ve := valueRunEnd(line, v0)
	if ve == v0 {
		return line
	}
	key, sep, value := line[loc[2]:loc[3]], line[loc[4]:loc[5]], line[v0:ve]
	if !shouldRedact(key, value, true) {
		return line
	}
	return key + sep + mask(value) + line[ve:]
}

func backslashRun(text string, i int) int {
	for i < len(text) && text[i] == '\\' {
		i++
	}
	return i
}

func skipSpace(text string, i int) int {
	for i < len(text) {
		r, w := utf8.DecodeRuneInString(text[i:])
		if !isPySpace(r) {
			break
		}
		i += w
	}
	return i
}

func jsonEscapedAt(text string, b0, b1 int) (start, end int, key, escapes, value string, ok bool) {
	open := b1 + 1
	for _, word := range jsonEscapedNames {
		n := matchFold(text[open:], word)
		if n < 0 {
			continue
		}
		c0 := open + n
		c1 := backslashRun(text, c0)
		m := c1 - c0
		if m == 0 || m > b1-b0 || c1 >= len(text) || text[c1] != '"' {
			continue
		}
		start = b1 - m
		key, escapes = text[start:c1+1], text[c0:c1]
		i := skipSpace(text, c1+1)
		if i >= len(text) || text[i] != ':' {
			return 0, 0, "", "", "", false
		}
		i = skipSpace(text, i+1)
		if !strings.HasPrefix(text[i:], escapes+`"`) {
			return 0, 0, "", "", "", false
		}
		vs := i + m + 1
		ve := vs
		for ve < len(text) && text[ve] != '"' && text[ve] != '\\' {
			ve++
		}
		if ve == vs || !strings.HasPrefix(text[ve:], escapes+`"`) {
			return 0, 0, "", "", "", false
		}
		return start, ve + m + 1, key, escapes, text[vs:ve], true
	}
	return 0, 0, "", "", "", false
}

func redactJSONEscaped(text string, mask func(string) string) string {
	var b strings.Builder
	last := 0
	for i := 0; i < len(text); {
		j := strings.IndexByte(text[i:], '\\')
		if j < 0 {
			break
		}
		b0 := i + j
		b1 := backslashRun(text, b0)
		if b1 >= len(text) || text[b1] != '"' {
			i = b1
			continue
		}
		start, end, key, escapes, value, ok := jsonEscapedAt(text, b0, b1)
		if !ok {
			i = b1 + 1
			continue
		}
		if shouldRedact(key, value, false) {
			b.WriteString(text[last:start])
			b.WriteString(key + ": " + escapes + `"` + mask(value) + escapes + `"`)
			last = end
		}
		i = end
	}
	if last == 0 {
		return text
	}
	b.WriteString(text[last:])
	return b.String()
}

func isReprSecretKey(key string) bool {
	folded := casefold(key)
	if reprSecretKeys[folded] {
		return true
	}
	if pyIsUpperString(key) && hasAnySuffix(key, reprEnvSuffixes) {
		return true
	}
	return hasAnySuffix(folded, reprCredentialSuffixes)
}

func IsSecretFieldName(key string) bool {
	return isReprSecretKey(key)
}

func redactReprFields(text string) string {
	return replaceSubmatch(reReprField, text, func(m []int) string {
		key := group(text, m, 1)
		if !isReprSecretKey(key) {
			return text[m[0]:m[1]]
		}
		prefix, quote, value := group(text, m, 5), `"`, group(text, m, 6)
		if m[8] >= 0 {
			prefix, quote, value = group(text, m, 3), "'", group(text, m, 4)
		}
		if isEnvLookup(value) || strings.Contains(value, "***") || strings.HasPrefix(value, "«redacted:") {
			return text[m[0]:m[1]]
		}
		return "'" + key + "'" + group(text, m, 2) + prefix + quote + "***" + quote
	})
}

func pyLineEnd(text string, i int) (end, next int) {
	for j := i; j < len(text); {
		r, w := utf8.DecodeRuneInString(text[j:])
		switch r {
		case '\r':
			if j+1 < len(text) && text[j+1] == '\n' {
				return j, j + 2
			}
			return j, j + 1
		case '\n', '\v', '\f', 0x1c, 0x1d, 0x1e, 0x85, 0x2028, 0x2029:
			return j, j + w
		}
		j += w
	}
	return len(text), len(text)
}

func redactDiagnosticReprFields(text string) string {
	var b strings.Builder
	for i := 0; i < len(text); {
		end, next := pyLineEnd(text, i)
		ending := text[end:next]
		body := text[i:end]
		if ending != "\n" && ending != "\r" && ending != "\r\n" {
			body, ending = text[i:next], ""
		}
		m := rePytestLine.FindStringSubmatchIndex(body)
		if m == nil {
			m = reExceptionLine.FindStringSubmatchIndex(body)
		}
		if m != nil {
			b.WriteString(group(body, m, 1) + redactReprFields(group(body, m, 2)) + ending)
		} else {
			b.WriteString(body + ending)
		}
		i = next
	}
	return b.String()
}

func isAlphaRune(r rune) bool {
	return unicode.IsLetter(r)
}
