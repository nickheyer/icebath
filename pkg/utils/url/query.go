package urlutil

import (
	"net/url"
	"strings"
)

// Query string of name value pairs in the order given, names and values escaped as url.QueryEscape does
func EncodeQuery(pairs [][2]string) string {
	if len(pairs) == 0 {
		return ""
	}
	var b strings.Builder
	for i, p := range pairs {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(url.QueryEscape(p[0]))
		b.WriteByte('=')
		b.WriteString(url.QueryEscape(p[1]))
	}
	return b.String()
}

// Name value pairs of a query string in their order, a plus sign read as a space, a malformed escape kept as text and a name without = given an empty value
func QueryPairs(query string) [][2]string {
	if query == "" {
		return nil
	}
	pairs := make([][2]string, 0, strings.Count(query, "&")+1)
	for part := range strings.SplitSeq(query, "&") {
		if part == "" {
			continue
		}
		name, value, _ := strings.Cut(part, "=")
		pairs = append(pairs, [2]string{unescape(name, true), unescape(value, true)})
	}
	return pairs
}
