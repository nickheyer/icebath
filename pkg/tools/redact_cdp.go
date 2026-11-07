package tools

import "regexp"

// Python str pattern whitespace, the code points str.isspace accepts
const pyRegexSpace = `\t\n\x0b\x0c\r\x1c-\x20\x{85}\x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}`

var (
	cdpURLWithQueryRe = regexp.MustCompile(`(https?|wss?|ftp)://([^` + pyRegexSpace + `/?#]+)([^` + pyRegexSpace + `?#]*)\?([^` + pyRegexSpace + `#]+)(#[^` + pyRegexSpace + `]*)?`)
	cdpURLUserinfoRe  = regexp.MustCompile(`(https?|wss?|ftp)://([^/` + pyRegexSpace + `:@]+):([^/` + pyRegexSpace + `@]+)@`)
)

// Endpoint url with the default redaction plus every sensitive query value and userinfo password masked
func redactCDPURL(value string) string {
	text := RedactSensitiveText(value)
	if text == "" {
		return text
	}
	text = replaceAllSubmatchFunc(cdpURLWithQueryRe, text, func(g []string) string {
		return g[1] + "://" + g[2] + g[3] + "?" + redactQueryString(g[4]) + g[5]
	})
	return replaceAllSubmatchFunc(cdpURLUserinfoRe, text, func(g []string) string {
		return g[1] + "://" + g[2] + ":***@"
	})
}

// Cdp endpoint url safe to log, query credentials and userinfo passwords masked
func RedactCDPURL(value string) string { return redactCDPURL(value) }
