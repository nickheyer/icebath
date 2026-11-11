package dashboard

import (
	"crypto/sha256"
	"encoding/base64"
	"regexp"
	"strings"
)

var (
	htmlStartTagRE  = regexp.MustCompile(`^<([a-zA-Z][^\t\n\r\f />\x00]*)`)
	htmlAttrRE      = regexp.MustCompile(`^((?:[\s/]*)(?:(?:[^\s/>][^\s/=>]*)(?:\s*=+\s*(?:'[^']*'|"[^"]*"|(?:['"][^\s>]*|[^'"\s>][^\s>]*)))?(?:\s|/(?:[^>]|$))*))`)
	htmlAttrNameRE  = regexp.MustCompile(`^[\s/]*([^\s/>][^\s/=>]*)`)
	htmlScriptEndRE = regexp.MustCompile(`(?i)</script[\t\n\r\f />]`)
)

// CSP hashes of the inline scripts of a document, the html.parser reading of script data verbatim
func inlineScriptHashes(html string) []string {
	var hashes []string
	i := 0
	for i < len(html) {
		lt := strings.IndexByte(html[i:], '<')
		if lt < 0 {
			break
		}
		i += lt
		rest := html[i:]
		if strings.HasPrefix(rest, "<!--") {
			end := strings.Index(rest[4:], "-->")
			if end < 0 {
				break
			}
			i += 4 + end + 3
			continue
		}
		m := htmlStartTagRE.FindStringSubmatch(rest)
		if m == nil {
			i++
			continue
		}
		pos := len(m[0])
		hasSrc := false
		for {
			am := htmlAttrRE.FindString(rest[pos:])
			if am == "" {
				break
			}
			if nm := htmlAttrNameRE.FindStringSubmatch(am); nm != nil && strings.ToLower(nm[1]) == "src" {
				hasSrc = true
			}
			pos += len(am)
		}
		gt := strings.IndexByte(rest[pos:], '>')
		if gt < 0 {
			break
		}
		i += pos + gt + 1
		if strings.ToLower(m[1]) != "script" || hasSrc {
			continue
		}
		end := htmlScriptEndRE.FindStringIndex(html[i:])
		if end == nil {
			break
		}
		digest := sha256.Sum256([]byte(html[i : i+end[0]]))
		hashes = append(hashes, "'sha256-"+base64.StdEncoding.EncodeToString(digest[:])+"'")
		i += end[0]
	}
	return hashes
}

// Response headers of a served page, the Webapp's content security policy added on its surface
func pageHeaders(req *Request, html string, headers []string) []string {
	if surface, _ := req.App().State.Get("ui_surface").(string); surface != "webapp" {
		return headers
	}
	hashes := strings.Join(inlineScriptHashes(html), " ")
	policy := strings.Join([]string{
		"default-src 'none'",
		"script-src 'self' blob: 'wasm-unsafe-eval' " + hashes,
		"script-src-attr 'none'",
		"style-src 'self' 'unsafe-inline' https://fonts.googleapis.com",
		"font-src 'self' data: https://fonts.gstatic.com",
		"connect-src * ws: wss: data: blob:",
		"img-src 'self' data: blob: https: http:",
		"media-src * data: blob:",
		"worker-src 'self' blob:",
		"frame-src 'self' blob: data: https: http:",
		"manifest-src 'self'",
		"object-src 'none'",
		"base-uri 'none'",
		"form-action 'none'",
		"frame-ancestors 'none'",
	}, "; ")
	return append(append([]string{}, headers...), "Content-Security-Policy", policy, "X-Content-Type-Options", "nosniff")
}
