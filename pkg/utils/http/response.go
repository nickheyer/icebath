package httputil

import (
	"mime"
	"net/http"
	"strings"

	charsetutil "github.com/nickheyer/icebath/pkg/utils/charset"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Response read whole: the status, the headers, the URL that answered and the body bytes
type Response struct {
	StatusCode int
	// Status line text after the protocol, such as "404 Not Found", empty when the response was not read from the wire
	Status string
	Header http.Header
	// URL of the request that produced the response, the last hop when redirects were followed
	URL string
	// Body bytes, the content codings the transport undid already undone
	Content []byte
}

// Body as text in the charset the Content-Type names, UTF-8 otherwise, undecodable bytes as U+FFFD
func (r *Response) Text() string { return DecodeText(r.Header, r.Content) }

// Body decoded as JSON: objects as *jsonutil.Object, arrays as []any, integers that fit as int64, other numbers as float64
func (r *Response) JSON() (any, error) { return jsonutil.Decode(r.Content) }

// Status error of the response, nil when the status is in the 2xx range
func (r *Response) CheckStatus() error {
	if r.StatusCode >= 200 && r.StatusCode <= 299 {
		return nil
	}
	return &StatusError{StatusCode: r.StatusCode, Status: r.Status, URL: r.URL, Location: r.Header.Get("Location")}
}

// Text of a body in the charset its Content-Type names, UTF-8 when it names none or one that is unknown, undecodable bytes as U+FFFD
func DecodeText(h http.Header, body []byte) string {
	if name := contentCharset(h.Get("Content-Type")); name != "" && name != "utf-8" && name != "utf8" {
		if text, err := charsetutil.DecodeReplace(body, name); err == nil {
			return text
		}
	}
	return strings.ToValidUTF8(string(body), "�")
}

// Lowercased charset parameter of a Content-Type value, read leniently when the value does not parse as a media type
func contentCharset(contentType string) string {
	if contentType == "" {
		return ""
	}
	if _, params, err := mime.ParseMediaType(contentType); err == nil {
		return strings.ToLower(strings.Trim(params["charset"], `"' `))
	}
	lower := strings.ToLower(contentType)
	i := strings.Index(lower, "charset=")
	if i < 0 {
		return ""
	}
	value, _, _ := strings.Cut(lower[i+len("charset="):], ";")
	return strings.Trim(value, `"' `)
}
