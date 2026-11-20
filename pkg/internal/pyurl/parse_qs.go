package pyurl

import "strings"

// Name value pairs of a query string as urllib.parse.parse_qsl splits and unquotes them, non strict
func ParseQSL(qs string, keepBlankValues bool) [][2]string {
	var out [][2]string
	for _, nameValue := range strings.Split(qs, "&") {
		if nameValue == "" {
			continue
		}
		nv := strings.SplitN(nameValue, "=", 2)
		if len(nv) != 2 {
			if !keepBlankValues {
				continue
			}
			nv = append(nv, "")
		}
		if len(nv[1]) > 0 || keepBlankValues {
			out = append(out, [2]string{Unquote(strings.ReplaceAll(nv[0], "+", " ")), Unquote(strings.ReplaceAll(nv[1], "+", " "))})
		}
	}
	return out
}

// Values per name in first seen order as urllib.parse.parse_qs groups them
func ParseQS(qs string, keepBlankValues bool) map[string][]string {
	out := map[string][]string{}
	for _, kv := range ParseQSL(qs, keepBlankValues) {
		out[kv[0]] = append(out[kv[0]], kv[1])
	}
	return out
}
