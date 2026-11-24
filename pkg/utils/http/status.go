package httputil

import (
	"net/http"
	"strconv"
	"strings"
)

// Response whose status reports a failure: the code, the status line text, the URL that answered and where a redirect points
type StatusError struct {
	StatusCode int
	// Status line text such as "404 Not Found", the standard text of the code when empty
	Status string
	URL    string
	// Location header of a redirect response, empty otherwise
	Location string
}

// Kind of status, the status line, the URL and for a redirect its target, such as "client error 404 Not Found for https://example.com/x", a password in either URL written as xxxxx
func (e *StatusError) Error() string {
	status := e.Status
	if status == "" {
		status = strconv.Itoa(e.StatusCode)
		if text := http.StatusText(e.StatusCode); text != "" {
			status += " " + text
		}
	}
	var b strings.Builder
	switch e.StatusCode / 100 {
	case 1:
		b.WriteString("informational response ")
	case 3:
		b.WriteString("redirect ")
	case 4:
		b.WriteString("client error ")
	case 5:
		b.WriteString("server error ")
	default:
		b.WriteString("unexpected status ")
	}
	b.WriteString(status)
	if e.URL != "" {
		b.WriteString(" for ")
		b.WriteString(redactPassword(e.URL))
	}
	if e.Location != "" && e.StatusCode/100 == 3 {
		b.WriteString(", redirecting to ")
		b.WriteString(redactPassword(e.Location))
	}
	return b.String()
}

// URL or proxy setting with the password of its user info written as xxxxx, for error text that may reach a log; the scheme is optional
func redactPassword(raw string) string {
	start := 0
	if i := strings.Index(raw, "://"); i >= 0 && !strings.ContainsAny(raw[:i], "/?#@") {
		start = i + len("://")
	} else if strings.HasPrefix(raw, "//") {
		start = len("//")
	}
	authority := raw[start:]
	if i := strings.IndexAny(authority, "/?#"); i >= 0 {
		authority = authority[:i]
	}
	at := strings.LastIndexByte(authority, '@')
	if at < 0 {
		return raw
	}
	user, _, hasPassword := strings.Cut(authority[:at], ":")
	if !hasPassword {
		return raw
	}
	return raw[:start] + user + ":xxxxx" + raw[start+at:]
}
