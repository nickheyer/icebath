package redact

import (
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

var secretHeaderNames = []string{"x-api-key", "x-goog-api-key", "api-key", "apikey", "x-api-token", "x-auth-token", "x-access-token"}

var sensitiveQueryParams = map[string]bool{
	"access_token": true, "refresh_token": true, "id_token": true, "token": true, "api_key": true, "apikey": true,
	"client_secret": true, "password": true, "auth": true, "jwt": true, "session": true, "secret": true, "key": true,
	"code": true, "signature": true, "x-amz-signature": true, "x-goog-signature": true, "sig": true,
}

var (
	reAuthHeader   = regexp.MustCompile(`(?i)((?:Proxy-)?` + fi("Authorization") + `:` + sp + `*)([A-Za-z` + foldExtra + `][\p{L}\p{N}_.+-]*` + sp + `+)?([^` + pyWS + `"']+)`)
	reSecretHeader = regexp.MustCompile(`(?i)((?:` + fi(strings.Join(secretHeaderNames, "|")) + `)` + sp + `*:` + sp + `*)(` + nsp + `+)`)
	rePrivateKey   = regexp.MustCompile(`-----BEGIN[A-Z ]*PRIVATE KEY-----(?s:.)*?-----END[A-Z ]*PRIVATE KEY-----`)
	reDBConn       = regexp.MustCompile(`(?i)((?:postgres(?:ql)?|mysql|mongodb(?:\+srv)?|` + fi("redis") + `|amqp)://[^:` + pyWS + `]+:)([^@` + pyWS + `]+)(@)`)
	reBareToken    = regexp.MustCompile(`(?i)((?:https?|wss?|` + fi("git") + `|ssh|ftp|ftps|sftp)://)([^` + pyWS + `:@/]{8,})(@` + nsp + `+)`)
	reJWT          = regexp.MustCompile(`eyJ[A-Za-z0-9_-]{10,}(?:\.[A-Za-z0-9_=-]{4,}){0,2}`)
	reWebapp       = regexp.MustCompile(`(icebath-session=|/webapp/launch#(?:[^` + pyWS + `#]*?&)?ticket=)[A-Za-z0-9_-]{20,}`)
	reURLWithQuery = regexp.MustCompile(`(https?|wss?|ftp)://([^` + pyWS + `/?#]+)([^` + pyWS + `?#]*)\?([^` + pyWS + `#]+)(#` + nsp + `*)?`)
	reURLUserinfo  = regexp.MustCompile(`(https?|wss?|ftp)://([^/` + pyWS + `:@]+):([^/` + pyWS + `@]+)@`)
	reStrictParam  = regexp.MustCompile(`([?#&;])([A-Za-z0-9_.~+%\-]+)=([^#&;` + pyWS + `"'<>]*)`)
	reStrictUser   = regexp.MustCompile(`//[^/` + pyWS + `?#@]+@`)
	reFormBody     = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]*=[^&` + pyWS + `]*(?:&[A-Za-z_][A-Za-z0-9_.-]*=[^&` + pyWS + `]*)+\z`)
	reBearer       = regexp.MustCompile(`(?i)bearer` + sp + `+(?:\[[^\]]+\]|[A-Za-z0-9._~+/` + foldExtra + `-]{20,}=*)`)
)

func SecretHeaderNames() []string {
	return slices.Clone(secretHeaderNames)
}

func SensitiveQueryParams() []string {
	out := make([]string, 0, len(sensitiveQueryParams))
	for name := range sensitiveQueryParams {
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

func maskToken(token string) string {
	if token == "" {
		return "***"
	}
	return maskSecret(token, 6, 4, 18, "***", "")
}

func redactAuthHeader(text string) string {
	return replaceSubmatch(reAuthHeader, text, func(m []int) string {
		return group(text, m, 1) + group(text, m, 2) + maskToken(group(text, m, 3))
	})
}

func redactSecretHeader(text string) string {
	return replaceSubmatch(reSecretHeader, text, func(m []int) string {
		return group(text, m, 1) + maskToken(group(text, m, 2))
	})
}

func telegramDigits(text string, q int) (end, colon int, ok bool) {
	i, n := q, 0
	for i < len(text) {
		r, w := utf8.DecodeRuneInString(text[i:])
		if !unicode.Is(unicode.Nd, r) {
			break
		}
		i += w
		n++
	}
	if n < 8 || i >= len(text) || text[i] != ':' {
		return 0, 0, false
	}
	j := runEnd(text, i+1, isTokenChar)
	if j-(i+1) < 30 {
		return 0, 0, false
	}
	return j, i, true
}

func redactTelegram(text string) string {
	var b strings.Builder
	last := 0
	for p := 0; p < len(text); {
		_, w := utf8.DecodeRuneInString(text[p:])
		if p > 0 && unicode.Is(unicode.Nd, prevRune(text, p)) {
			p += w
			continue
		}
		end, colon, ok := 0, 0, false
		if strings.HasPrefix(text[p:], "bot") {
			end, colon, ok = telegramDigits(text, p+3)
		}
		if !ok {
			end, colon, ok = telegramDigits(text, p)
		}
		if !ok {
			p += w
			continue
		}
		b.WriteString(text[last:p])
		b.WriteString(text[p:colon] + ":***")
		last, p = end, end
	}
	if last == 0 {
		return text
	}
	b.WriteString(text[last:])
	return b.String()
}

func redactURLCredentials(text string, codeFile bool) string {
	text = replaceSubmatch(reDBConn, text, func(m []int) string {
		pw := group(text, m, 2)
		if codeFile && strings.HasPrefix(pw, "{") && strings.HasSuffix(pw, "}") {
			return text[m[0]:m[1]]
		}
		return group(text, m, 1) + "***" + group(text, m, 3)
	})
	return replaceSubmatch(reBareToken, text, func(m []int) string {
		return group(text, m, 1) + maskToken(group(text, m, 2)) + group(text, m, 3)
	})
}

func redactWebappLink(text string) string {
	var b strings.Builder
	last, from := 0, 0
	for from < len(text) {
		m := reWebapp.FindStringSubmatchIndex(text[from:])
		if m == nil {
			break
		}
		start := from + m[0]
		if text[start] == 'i' && start > 0 && isTokenChar(text[start-1]) {
			from = start + 1
			continue
		}
		b.WriteString(text[last:start])
		b.WriteString(text[from+m[2]:from+m[3]] + "***")
		last = from + m[1]
		from = last
	}
	if last == 0 {
		return text
	}
	b.WriteString(text[last:])
	return b.String()
}

func unquotePlus(s string) string {
	s = strings.ReplaceAll(s, "+", " ")
	if !strings.Contains(s, "%") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		j := i
		if s[i] < utf8.RuneSelf {
			for j < len(s) && s[j] < utf8.RuneSelf {
				j++
			}
			b.WriteString(strings.ToValidUTF8(percentDecode(s[i:j]), "�"))
		} else {
			for j < len(s) && s[j] >= utf8.RuneSelf {
				j++
			}
			b.WriteString(s[i:j])
		}
		i = j
	}
	return b.String()
}

func unhex(c byte) byte {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10
	}
	return c - 'A' + 10
}

func percentDecode(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) && isHexDigit(s[i+1]) && isHexDigit(s[i+2]) {
			out = append(out, unhex(s[i+1])<<4|unhex(s[i+2]))
			i += 2
			continue
		}
		out = append(out, s[i])
	}
	return string(out)
}

func CanonicalParamName(name string) string {
	decoded := name
	for range 3 {
		next := unquotePlus(decoded)
		if next == decoded {
			break
		}
		decoded = next
	}
	folded := casefold(decoded)
	if sensitiveQueryParams[folded] {
		return folded
	}
	return strings.ReplaceAll(folded, "-", "_")
}

func redactStrictURLCredentials(text string) string {
	text = replaceSubmatch(reStrictParam, text, func(m []int) string {
		if !sensitiveQueryParams[CanonicalParamName(group(text, m, 2))] {
			return text[m[0]:m[1]]
		}
		return group(text, m, 1) + group(text, m, 2) + "=***"
	})
	return reStrictUser.ReplaceAllLiteralString(text, "//***:***@")
}

func redactQueryString(query string) string {
	if query == "" {
		return query
	}
	pairs := strings.Split(query, "&")
	for i, pair := range pairs {
		if key, _, found := strings.Cut(pair, "="); found && sensitiveQueryParams[pyLower(key)] {
			pairs[i] = key + "=***"
		}
	}
	return strings.Join(pairs, "&")
}

func redactFormBody(text string) string {
	if text == "" || strings.Contains(text, "\n") || !strings.Contains(text, "&") {
		return text
	}
	stripped := pyStrip(text)
	if !reFormBody.MatchString(stripped) {
		return text
	}
	return redactQueryString(stripped)
}

func redactPhones(text string) string {
	var b strings.Builder
	last := 0
	for p := 0; p < len(text); p++ {
		if text[p] != '+' || p+1 >= len(text) || text[p+1] < '1' || text[p+1] > '9' {
			continue
		}
		var ends [14]int
		n, i := 0, p+2
		for n < 14 && i < len(text) {
			r, w := utf8.DecodeRuneInString(text[i:])
			if !unicode.Is(unicode.Nd, r) {
				break
			}
			i += w
			ends[n] = i
			n++
		}
		for k := n; k >= 6; k-- {
			e := ends[k-1]
			if e < len(text) && isAlnum(text[e]) {
				continue
			}
			phone := text[p:e]
			count := utf8.RuneCountInString(phone)
			keep := 4
			if count <= 8 {
				keep = 2
			}
			b.WriteString(text[last:p])
			b.WriteString(phone[:runeOffset(phone, keep)] + "****" + phone[runeOffset(phone, count-keep):])
			last = e
			p = e - 1
			break
		}
	}
	if last == 0 {
		return text
	}
	b.WriteString(text[last:])
	return b.String()
}

func redactZhipu(text string, mask func(string) string) string {
	isKeyChar := func(c byte) bool { return isTokenChar(c) || c == '.' }
	var b strings.Builder
	last := 0
	for p := 0; p+49 <= len(text); p++ {
		if !isLowerHex(text[p]) || (p > 0 && isKeyChar(text[p-1])) {
			continue
		}
		q := runEnd(text[:p+32], p, isLowerHex)
		if q < p+32 || text[q] != '.' {
			continue
		}
		e := runEnd(text, q+1, isAlnum)
		if e-(q+1) < 16 || (e < len(text) && isKeyChar(text[e])) {
			continue
		}
		b.WriteString(text[last:p])
		b.WriteString(mask(text[p:e]))
		last = e
		p = e - 1
	}
	if last == 0 {
		return text
	}
	b.WriteString(text[last:])
	return b.String()
}

func redactBearerResidue(text string) string {
	var b strings.Builder
	last, from := 0, 0
	for from < len(text) {
		m := reBearer.FindStringIndex(text[from:])
		if m == nil {
			break
		}
		start := from + m[0]
		if start > 0 && pyIsWord(prevRune(text, start)) {
			from = start + 1
			continue
		}
		b.WriteString(text[last:start])
		b.WriteString("Bearer [redacted]")
		last = from + m[1]
		from = last
	}
	if last == 0 {
		return text
	}
	b.WriteString(text[last:])
	return b.String()
}
