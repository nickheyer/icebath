package pyhtml

import "strings"

var escapeQuote = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "\"", "&quot;", "'", "&#x27;")

var escapePlain = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

// HTML escape of s with the Python html.escape replacements, quotes included when quote is set
func Escape(s string, quote bool) string {
	if quote {
		return escapeQuote.Replace(s)
	}
	return escapePlain.Replace(s)
}
