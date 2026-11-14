package wire

import (
	"bytes"
	"encoding/json"
	"net/url"
	"regexp"
	"strings"
	"unicode"
)

// Most characters of an unreadable stream frame kept for its diagnostic
const streamErrorTextLimit = 4096

// Member names whose string value is a credential wherever the member appears
const secretFieldNames = `(?:api_?key|token|secret|password|access_token|refresh_token|id_token|auth_token|bearer|` +
	`authorization|proxy-authorization|x-api-key|x-goog-api-key|api-key|client_secret|secret_value|raw_secret|` +
	`secret_input|key_material)`

// Vendor key shapes, each a literal prefix followed by its token body
var tokenShapes = [...]string{
	`sk-[A-Za-z0-9_-](?:\.?[A-Za-z0-9_-]){9,}`, `ctx7sk-[A-Za-z0-9_-]{10,}`, `ghp_[A-Za-z0-9]{10,}`,
	`github_pat_[A-Za-z0-9_]{10,}`, `gho_[A-Za-z0-9]{10,}`, `ghu_[A-Za-z0-9]{10,}`, `ghs_[A-Za-z0-9]{10,}`,
	`ghr_[A-Za-z0-9]{10,}`, `xapp-\d+-[A-Za-z0-9-]{10,}`, `xox[baprs]-[A-Za-z0-9-]{10,}`, `AIza[A-Za-z0-9_-]{30,}`,
	`pplx-[A-Za-z0-9]{10,}`, `fal_[A-Za-z0-9_-]{10,}`, `fc-[A-Za-z0-9]{10,}`, `bb_live_[A-Za-z0-9_-]{10,}`,
	`gAAAA[A-Za-z0-9_=-]{20,}`, `AKIA[A-Z0-9]{16}`, `sk_live_[A-Za-z0-9]{10,}`, `sk_test_[A-Za-z0-9]{10,}`,
	`rk_live_[A-Za-z0-9]{10,}`, `SG\.[A-Za-z0-9_-]{10,}`, `hf_[A-Za-z0-9]{10,}`, `r8_[A-Za-z0-9]{10,}`,
	`npm_[A-Za-z0-9]{10,}`, `pypi-[A-Za-z0-9_-]{10,}`, `dop_v1_[A-Za-z0-9]{10,}`, `doo_v1_[A-Za-z0-9]{10,}`,
	`am_(?:org_)?[A-Za-z0-9]{20,}`, `sk_[A-Za-z0-9_]{10,}`, `tvly-[A-Za-z0-9]{10,}`, `exa_[A-Za-z0-9]{10,}`,
	`gsk_[A-Za-z0-9]{10,}`, `syt_[A-Za-z0-9]{10,}`, `retaindb_[A-Za-z0-9]{10,}`, `hsk-[A-Za-z0-9]{10,}`,
	`mem0_[A-Za-z0-9]{10,}`, `brv_[A-Za-z0-9]{10,}`, `xai-[A-Za-z0-9]{30,}`, `ntn_[A-Za-z0-9]{10,}`,
	`fw-[A-Za-z0-9]{30,}`, `fw_[A-Za-z0-9]{30,}`, `fpk_[A-Za-z0-9]{30,}`, `glpat-[A-Za-z0-9_\-]{10,}`,
	`gloas-[A-Za-z0-9_\-]{10,}`, `gldt-[A-Za-z0-9_\-]{10,}`, `glrt-[A-Za-z0-9_.\-]{10,}`, `glrtr-[A-Za-z0-9_.\-]{10,}`,
	`glcbt-[A-Za-z0-9_\-]{10,}`, `glptt-[A-Za-z0-9_\-]{10,}`, `glft-[A-Za-z0-9_\-]{10,}`, `glimt-[A-Za-z0-9_\-]{10,}`,
	`glagent-[A-Za-z0-9_\-]{10,}`, `glsoat-[A-Za-z0-9_\-]{10,}`, `glffct-[A-Za-z0-9_\-]{10,}`, `glwt-[A-Za-z0-9_\-]{10,}`,
	`GR1348941[A-Za-z0-9_\-]{10,}`, `pk-lf-[A-Za-z0-9\-]{8,}`,
}

// Credential shapes a provider can echo back inside error text
var (
	tokenAny      = regexp.MustCompile(strings.Join(tokenShapes[:], "|"))
	tokenEach     = anchoredEach(tokenShapes[:])
	dottedKey     = regexp.MustCompile(`[0-9a-f]{32}\.[A-Za-z0-9]{16,}`)
	dottedEach    = anchoredEach([]string{dottedKey.String()})
	jwtToken      = regexp.MustCompile(`eyJ[A-Za-z0-9_-]{10,}(?:\.[A-Za-z0-9_=-]{4,}){0,2}`)
	authHeader    = regexp.MustCompile(`(?i)((?:Proxy-)?Authorization:\s*)([A-Za-z][\w.+-]*\s+)?([^\s"']+)`)
	keyHeader     = regexp.MustCompile(`(?i)((?:x-api-key|x-goog-api-key|api-key|apikey|x-api-token|x-auth-token|x-access-token)\s*:\s*)(\S+)`)
	bearerValue   = regexp.MustCompile(`(?i)(\bBearer\s+)([A-Za-z0-9._~+/-]{20,}=*)`)
	privateKey    = regexp.MustCompile(`-----BEGIN[A-Z ]*PRIVATE KEY-----[\s\S]*?-----END[A-Z ]*PRIVATE KEY-----`)
	connPassword  = regexp.MustCompile(`(?i)((?:postgres(?:ql)?|mysql|mongodb(?:\+srv)?|redis|amqp)://[^:\s]+:)([^@\s]+)(@)`)
	urlBareToken  = regexp.MustCompile(`(?i)((?:https?|wss?|git|ssh|ftp|ftps|sftp)://)([^\s:@/]{8,})(@[^\s]+)`)
	urlParam      = regexp.MustCompile(`([?#&;])([A-Za-z0-9_.~+%\-]+)=([^#&;\s"'<>\\]*)`)
	urlUserinfo   = regexp.MustCompile(`//[^/\s?#@]+@`)
	secretName    = regexp.MustCompile(`(?i)^` + secretFieldNames + `$`)
	secretField   = regexp.MustCompile(`(?i)("` + secretFieldNames + `")\s*:\s*"([^"]+)"`)
	escapedField  = regexp.MustCompile(`(?i)(\\+)"` + secretFieldNames + `(\\+)"\s*:\s*(\\+)"([^"\\]+)(\\+)"`)
	envAssignment = regexp.MustCompile(`([A-Z0-9_]{0,50}(?:API_?KEY|KEY|TOKEN|SECRET|PASSWORD|PASSWD|PASS|PW|CREDENTIAL|AUTH)[A-Z0-9_]{0,50})\s*=\s*(\S+)`)
	secretWord    = regexp.MustCompile(`(?i)(?:api|auth|access|refresh|session|secret)[ _.\\-]?(?:key|token)|token|secret|passwd|password|pass|pw|credential|auth|key`)
	strongWord    = regexp.MustCompile(`(?i)(?:api|auth|access|refresh|session|id|bearer)[ _.\\-]?(?:key|token)|key[ _.\\-]?material|secret|passwd|password|pass|pw|credential|auth|bearer`)
	envLookup     = regexp.MustCompile(`^(?:os\.(?:getenv|environ)|process\.env|\$ENV\{)`)
)

// Compiles each pattern to match only at the start of its input
func anchoredEach(patterns []string) []*regexp.Regexp {
	out := make([]*regexp.Regexp, len(patterns))
	for i, p := range patterns {
		out[i] = regexp.MustCompile(`^(?:` + p + `)`)
	}
	return out
}

// Reports whether c can continue a vendor token, so a token touching it is part of a longer word
func tokenByte(c byte) bool {
	return c == '_' || c == '-' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// Reports whether c can continue a dotted id and secret pair
func dottedByte(c byte) bool {
	return c == '.' || tokenByte(c)
}

// Shows the first six and last four characters of a credential, three stars for one under eighteen characters
func maskToken(token string) string {
	clean := []rune(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || (r >= 0x200b && r <= 0x200f) || (r >= 0x202a && r <= 0x202e) || (r >= 0x2060 && r <= 0x2064) {
			return -1
		}
		return r
	}, token))
	if len(clean) < 18 {
		return "***"
	}
	return string(clean[:6]) + "..." + string(clean[len(clean)-4:])
}

// Masks every match of a shape that no token character touches, trying each alternative at a candidate start in order
func maskShapes(text string, any *regexp.Regexp, each []*regexp.Regexp, inside func(byte) bool) string {
	var b strings.Builder
	pos, last := 0, 0
	for pos < len(text) {
		loc := any.FindStringIndex(text[pos:])
		if loc == nil {
			break
		}
		start := pos + loc[0]
		pos = start + 1
		if start > 0 && inside(text[start-1]) {
			continue
		}
		for _, re := range each {
			end := start + len(re.FindString(text[start:]))
			if end == start || (end < len(text) && inside(text[end])) {
				continue
			}
			b.WriteString(text[last:start])
			b.WriteString(maskToken(text[start:end]))
			pos, last = end, end
			break
		}
	}
	if last == 0 {
		return text
	}
	b.WriteString(text[last:])
	return b.String()
}

// Reports whether a value is already masked or names a variable instead of holding a secret
func maskExempt(value string) bool {
	return value == "***" || strings.HasPrefix(value, "[REDACTED") || envLookup.MatchString(value)
}

// Reports whether a value has the shape of an opaque credential
func opaqueValue(value string) bool {
	hex, body := true, true
	var lower, upper, digit bool
	for i := 0; i < len(value); i++ {
		c := value[i]
		switch {
		case c >= '0' && c <= '9':
			digit = true
		case c >= 'a' && c <= 'z':
			lower, hex = true, hex && c <= 'f'
		case c >= 'A' && c <= 'Z':
			upper, hex = true, hex && c <= 'F'
		default:
			hex, body = false, body && strings.IndexByte("_./+=-", c) >= 0
		}
	}
	switch {
	case len(value) >= 16 && hex, len(value) >= 20 && body:
		return true
	case len(value) < 12:
		return false
	}
	classes := 0
	for _, seen := range [...]bool{lower, upper, digit} {
		if seen {
			classes++
		}
	}
	return classes >= 2
}

// Reports whether position i of a name begins a word, at an edge, a non-letter or a case change
func wordStart(s string, i int) bool {
	if i == 0 {
		return true
	}
	prev, cur := s[i-1], s[i]
	if !asciiLetter(prev) || (asciiUpper(cur) && !asciiUpper(prev)) {
		return true
	}
	return asciiUpper(cur) && asciiUpper(prev) && i+1 < len(s) && asciiLetter(s[i+1]) && !asciiUpper(s[i+1])
}

// Reports whether position j of a name ends a word, one trailing plural s absorbed when allowed
func wordEnd(s string, j int, plural bool) bool {
	if j >= len(s) {
		return true
	}
	cur := s[j]
	if !asciiLetter(cur) || (asciiUpper(cur) && asciiLetter(s[j-1]) && !asciiUpper(s[j-1])) {
		return true
	}
	return plural && cur|0x20 == 's' && wordEnd(s, j+1, false)
}

// Reports whether c is an ASCII letter
func asciiLetter(c byte) bool {
	return c|0x20 >= 'a' && c|0x20 <= 'z'
}

// Reports whether c is an ASCII capital
func asciiUpper(c byte) bool {
	return c >= 'A' && c <= 'Z'
}

// Reports whether a pattern matches inside a name as a whole word
func wordIn(name string, re *regexp.Regexp) bool {
	for _, loc := range re.FindAllStringIndex(name, -1) {
		if wordStart(name, loc[0]) && wordEnd(name, loc[1], true) {
			return true
		}
	}
	return false
}

// Masks the value of a NAME=value assignment whose name carries a secret word
func maskAssignment(match string) string {
	groups := envAssignment.FindStringSubmatch(match)
	name, value := groups[1], groups[2]
	quote := ""
	if value[0] == '"' || value[0] == '\'' {
		if end := strings.LastIndexByte(value[1:], value[0]); end > 0 {
			quote, value = value[:1], value[1:1+end]
		}
	}
	if maskExempt(value) || !wordIn(name, secretWord) || (!wordIn(name, strongWord) && !opaqueValue(value)) {
		return match
	}
	return name + "=" + quote + maskToken(value) + quote
}

// Name of a URL parameter with percent escapes resolved, folded for comparison
func paramName(name string) string {
	for range 3 {
		decoded, err := url.QueryUnescape(name)
		if err != nil || decoded == name {
			break
		}
		name = decoded
	}
	name = strings.ToLower(name)
	switch name {
	case "x-amz-signature", "x-goog-signature":
		return name
	}
	return strings.ReplaceAll(name, "-", "_")
}

// Reports whether a URL parameter carries a credential
func secretParam(name string) bool {
	switch paramName(name) {
	case "access_token", "refresh_token", "id_token", "token", "api_key", "apikey", "client_secret", "password", "auth",
		"jwt", "session", "secret", "key", "signature", "x-amz-signature", "x-goog-signature", "sig":
		return true
	}
	return false
}

// Masks every credential-shaped run of provider text, vendor keys, tokens, auth headers, secret fields and URL credentials
func MaskCredentials(text string) string {
	if text == "" {
		return text
	}
	text = maskShapes(text, tokenAny, tokenEach, tokenByte)
	if strings.Contains(text, ".") {
		text = maskShapes(text, dottedKey, dottedEach, dottedByte)
	}
	if strings.Contains(text, "=") {
		text = envAssignment.ReplaceAllStringFunc(text, maskAssignment)
	}
	if strings.Contains(text, `"`) && strings.Contains(text, ":") {
		text = secretField.ReplaceAllStringFunc(text, func(m string) string {
			g := secretField.FindStringSubmatch(m)
			if maskExempt(g[2]) {
				return m
			}
			return g[1] + `: "` + maskToken(g[2]) + `"`
		})
		text = escapedField.ReplaceAllStringFunc(text, func(m string) string {
			g := escapedField.FindStringSubmatch(m)
			if maskExempt(g[4]) || g[1] != g[2] || g[1] != g[3] || g[1] != g[5] {
				return m
			}
			return m[:len(m)-len(g[4])-len(g[5])-1] + maskToken(g[4]) + g[5] + `"`
		})
	}
	text = authHeader.ReplaceAllStringFunc(text, func(m string) string {
		g := authHeader.FindStringSubmatch(m)
		return g[1] + g[2] + maskToken(g[3])
	})
	text = keyHeader.ReplaceAllStringFunc(text, func(m string) string {
		g := keyHeader.FindStringSubmatch(m)
		return g[1] + maskToken(g[2])
	})
	if strings.Contains(text, "-----") {
		text = privateKey.ReplaceAllString(text, "[REDACTED PRIVATE KEY]")
	}
	if strings.Contains(text, "://") {
		text = connPassword.ReplaceAllString(text, "${1}***${3}")
		text = urlBareToken.ReplaceAllStringFunc(text, func(m string) string {
			g := urlBareToken.FindStringSubmatch(m)
			return g[1] + maskToken(g[2]) + g[3]
		})
	}
	if strings.Contains(text, "=") {
		text = urlParam.ReplaceAllStringFunc(text, func(m string) string {
			g := urlParam.FindStringSubmatch(m)
			if !secretParam(g[2]) {
				return m
			}
			return g[1] + g[2] + "=***"
		})
	}
	if strings.Contains(text, "//") {
		text = urlUserinfo.ReplaceAllString(text, "//***:***@")
	}
	if strings.Contains(text, "eyJ") {
		text = jwtToken.ReplaceAllStringFunc(text, maskToken)
	}
	return bearerValue.ReplaceAllStringFunc(text, func(m string) string {
		g := bearerValue.FindStringSubmatch(m)
		return g[1] + maskToken(g[2])
	})
}

// Masks an unreadable stream frame and cuts it to the diagnostic bound
func maskFrame(text string) string {
	return preview(MaskCredentials(text), streamErrorTextLimit)
}

// One open object or array while JSON text is walked, with the member name last read
type maskScope struct {
	object  bool
	wantKey bool
	key     string
}

// Masks every string of valid JSON text and the whole value of any member named as a credential
func maskJSON(raw []byte) json.RawMessage {
	out := make([]byte, 0, len(raw))
	var stack []maskScope
	secret := false
	for i := 0; i < len(raw); {
		c := raw[i]
		top := len(stack) - 1
		if c == '"' {
			end := skipString(raw, i)
			text := decodeString(raw[i:end])
			switch {
			case top >= 0 && stack[top].object && stack[top].wantKey:
				stack[top].key, stack[top].wantKey = text, false
				out = append(out, raw[i:end]...)
			case secret && !maskExempt(text):
				out = appendString(out, maskToken(text), 0)
			default:
				if masked := MaskCredentials(text); masked != text {
					out = appendString(out, masked, 0)
				} else {
					out = append(out, raw[i:end]...)
				}
			}
			secret, i = false, end
			continue
		}
		switch c {
		case '{':
			stack, secret = append(stack, maskScope{object: true, wantKey: true}), false
		case '[':
			stack, secret = append(stack, maskScope{}), false
		case '}', ']':
			if top >= 0 {
				stack = stack[:top]
			}
			secret = false
		case ':':
			secret = top >= 0 && secretName.MatchString(stack[top].key)
		case ',':
			secret = false
			if top >= 0 {
				stack[top].wantKey = stack[top].object
			}
		}
		out = append(out, c)
		i++
	}
	if bytes.Equal(out, raw) {
		return raw
	}
	return out
}
