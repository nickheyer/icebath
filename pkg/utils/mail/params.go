package mailutil

import (
	"cmp"
	"slices"
	"strconv"
	"strings"
)

// Value and parameters of a structured field such as Content-Type or Content-Disposition, read leniently: value and parameter names lower cased, a comment after the value dropped, quotes removed, RFC 2231 continuations joined and decoded from their character set, an extended value preferred to a plain one of the same name and the first of repeated plain ones kept
func ParseParams(value string) (string, map[string]string) {
	segments := splitParams(value)
	main, _, _ := strings.Cut(segments[0], "(")
	main = strings.ToLower(strings.TrimSpace(main))
	params := make(map[string]string, len(segments)-1)
	if len(segments) == 1 {
		return main, params
	}
	type piece struct {
		n       int
		value   string
		encoded bool
	}
	extended := map[string]string{}
	pieces := map[string][]piece{}
	for _, segment := range segments[1:] {
		name, val, hasValue := strings.Cut(segment, "=")
		name = strings.ToLower(strings.TrimSpace(name))
		if name == "" {
			continue
		}
		val = unquote(strings.TrimSpace(val))
		base, star, isExtended := strings.Cut(name, "*")
		switch {
		case !isExtended:
			if _, seen := params[name]; !seen {
				if !hasValue {
					val = ""
				}
				params[name] = plainText(val)
			}
		case star == "":
			if _, seen := extended[base]; !seen {
				extended[base] = val
			}
		default:
			encoded := strings.HasSuffix(star, "*")
			n, err := strconv.Atoi(strings.TrimSuffix(star, "*"))
			if err != nil || n < 0 {
				continue
			}
			pieces[base] = append(pieces[base], piece{n: n, value: val, encoded: encoded})
		}
	}
	for name, raw := range extended {
		charset, text := splitExtended(raw)
		if decoded := decodeText(percentDecode(text), charset); decoded != "" {
			params[name] = decoded
		}
	}
	for name, list := range pieces {
		slices.SortStableFunc(list, func(a, b piece) int { return cmp.Compare(a.n, b.n) })
		charset := ""
		var joined []byte
		anyEncoded := false
		for i, p := range list {
			if !p.encoded {
				joined = append(joined, p.value...)
				continue
			}
			text := p.value
			if i == 0 {
				charset, text = splitExtended(text)
			}
			joined = append(joined, percentDecode(text)...)
			anyEncoded = true
		}
		var decoded string
		if anyEncoded {
			decoded = decodeText(joined, charset)
		} else {
			decoded = plainText(string(joined))
		}
		if decoded != "" {
			params[name] = decoded
		}
	}
	return main, params
}

// Segments of a field value split at semicolons outside quoted strings
func splitParams(s string) []string {
	var out []string
	start, quoted := 0, false
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\':
			if quoted {
				i++
			}
		case '"':
			quoted = !quoted
		case ';':
			if !quoted {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	return append(out, s[start:])
}

// Value without its surrounding double quotes, an unterminated opening quote dropped too, and the \\ and \" escapes inside undone while other backslashes stay, as sloppy senders leave them in Windows paths
func unquote(v string) string {
	if v == "" || v[0] != '"' {
		return v
	}
	inner := v[1:]
	if n := len(inner); n > 0 && inner[n-1] == '"' {
		inner = inner[:n-1]
	}
	if !strings.Contains(inner, `\`) {
		return inner
	}
	return strings.NewReplacer(`\\`, `\`, `\"`, `"`).Replace(inner)
}

// Character set and text of an RFC 2231 extended value charset'language'text, no character set when the quotes are missing
func splitExtended(v string) (string, string) {
	charset, rest, ok := strings.Cut(v, "'")
	if !ok {
		return "", v
	}
	if _, text, ok := strings.Cut(rest, "'"); ok {
		return charset, text
	}
	return "", v
}

// Bytes of text with %XX escapes decoded, a % that starts none kept
func percentDecode(s string) []byte {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) && isHex(s[i+1]) && isHex(s[i+2]) {
			out = append(out, unhex(s[i+1])<<4|unhex(s[i+2]))
			i += 2
			continue
		}
		out = append(out, s[i])
	}
	return out
}
