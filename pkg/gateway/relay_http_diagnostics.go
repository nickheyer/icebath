package gateway

import (
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

var relayOriginHeaders = []string{"Server", "Via", "Proxy-Status", "X-Squid-Error"}

var relayProxyOnlyHeaders = []string{"Via", "Proxy-Status", "X-Squid-Error"}

var relayProxyServerNames = []string{"squid", "envoy", "cloudflare", "zscaler", "bluecoat", "proxysg", "netskope", "forcepoint"}

// RelayBodyExcerptChars caps the characters of an error body quoted in a description
const RelayBodyExcerptChars = 200

const relayBodyReadLimit = 4096

// Bounded body of an HTTP error response, empty when there is none
func RelayReadErrorBody(exc *RelayHTTPError) []byte {
	if exc == nil {
		return nil
	}
	if len(exc.Body) > relayBodyReadLimit {
		return exc.Body[:relayBodyReadLimit]
	}
	return exc.Body
}

// Connector refusal reason from a JSON error object body, empty for anything else
func RelayConnectorReason(body []byte) string {
	text, err := pystr.DecodeUTF8(body)
	if err != nil {
		return ""
	}
	parsed, err := pyjson.Loads(text)
	if err != nil {
		return ""
	}
	obj, ok := pyjson.AsObject(parsed)
	if !ok {
		return ""
	}
	reason, ok := obj.Vals["error"].(string)
	if !ok {
		return ""
	}
	return pystr.Strip(reason)
}

// True when a response header names a proxy hop rather than the connector
func RelayAnsweredByProxy(exc *RelayHTTPError) bool {
	if exc == nil || exc.Header == nil {
		return false
	}
	for _, name := range relayProxyOnlyHeaders {
		if exc.Header.Get(name) != "" {
			return true
		}
	}
	server := pystr.Lower(exc.Header.Get("Server"))
	for _, name := range relayProxyServerNames {
		if strings.Contains(server, name) {
			return true
		}
	}
	return false
}

// Text with every exact secret replaced then generically redacted
func RelayRedactExcerpt(text string, secrets ...string) string {
	return tools.RedactSensitiveTextWith(nil, relayScrub(text, secrets), tools.RedactOptions{Force: true})
}

// Text with each non empty secret replaced by three asterisks
func relayScrub(text string, secrets []string) string {
	for _, secret := range secrets {
		if secret != "" {
			text = strings.ReplaceAll(text, secret, "***")
		}
	}
	return text
}

// HTTP status with the redacted origin headers and a one line redacted body excerpt
func RelayDescribeHTTPError(exc *RelayHTTPError, body []byte, secrets ...string) string {
	var parts []string
	for _, name := range relayOriginHeaders {
		value := ""
		if exc.Header != nil {
			value = exc.Header.Get(name)
		}
		if value != "" {
			collapsed := strings.Join(pystr.SplitWhitespace(value, -1), " ")
			parts = append(parts, strings.ToLower(name)+"="+pystr.Slice(RelayRedactExcerpt(collapsed, secrets...), 0, 120))
		}
	}
	text := relayScrub(strings.Join(pystr.SplitWhitespace(pystr.DecodeUTF8Replace(body), -1), " "), secrets)
	if text != "" {
		excerpt := pystr.Slice(text, 0, RelayBodyExcerptChars)
		if len(pystr.Runes(text)) > RelayBodyExcerptChars {
			excerpt += "…"
		}
		parts = append(parts, "body="+pyrepr.Repr(RelayRedactExcerpt(excerpt, secrets...)))
	}
	out := "HTTP " + strconv.Itoa(exc.Code)
	if len(parts) > 0 {
		out += " (" + strings.Join(parts, " ") + ")"
	}
	return out
}
