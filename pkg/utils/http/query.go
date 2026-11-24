package httputil

import (
	"net/url"
	"strings"
)

// URL with the values merged into its query string, each key given replacing every value the URL carried for it, the query encoded with sorted keys and the fragment kept
func MergeQuery(rawURL string, query url.Values) string {
	if len(query) == 0 {
		return rawURL
	}
	base, frag := rawURL, ""
	if i := strings.IndexByte(base, '#'); i >= 0 {
		base, frag = base[:i], base[i:]
	}
	base, existing, _ := strings.Cut(base, "?")
	values := parseQuery(existing, len(query))
	for k, vs := range query {
		values[k] = vs
	}
	return base + "?" + values.Encode() + frag
}

// Values of a query string split at every &, a key or value that does not unescape kept as written
func parseQuery(query string, extra int) url.Values {
	values := make(url.Values, strings.Count(query, "&")+1+extra)
	for part := range strings.SplitSeq(query, "&") {
		if part == "" {
			continue
		}
		k, v, _ := strings.Cut(part, "=")
		if uk, err := url.QueryUnescape(k); err == nil {
			k = uk
		}
		if uv, err := url.QueryUnescape(v); err == nil {
			v = uv
		}
		values[k] = append(values[k], v)
	}
	return values
}
