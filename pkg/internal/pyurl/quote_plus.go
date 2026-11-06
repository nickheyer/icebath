package pyurl

import "strings"

// Percent encodes like urllib.parse.quote_plus, spaces becoming plus signs
func QuotePlus(s, safe string) string {
	if !strings.Contains(s, " ") {
		return Quote(s, safe)
	}
	return strings.ReplaceAll(Quote(s, safe+" "), " ", "+")
}

// Query string of ordered string pairs like urllib.parse.urlencode with its quote_plus default
func URLEncode(pairs [][2]string) string {
	parts := make([]string, 0, len(pairs))
	for _, p := range pairs {
		parts = append(parts, QuotePlus(p[0], "")+"="+QuotePlus(p[1], ""))
	}
	return strings.Join(parts, "&")
}
