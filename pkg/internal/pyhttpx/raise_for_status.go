package pyhttpx

import (
	"fmt"
	"net/http"
	"strings"
)

// Text of the HTTPStatusError httpx raise_for_status raises for a non 2xx response
func RaiseForStatusMessage(status int, rawURL, location string) string {
	var header *string
	if location != "" {
		header = &location
	}
	return statusErrorText(status, http.StatusText(status), rawURL, header)
}

// HTTPStatusError text of this response with the reason phrase its status line carried
func (r *Response) StatusErrorText(rawURL string) string {
	reason := http.StatusText(r.StatusCode)
	if r.Status != "" {
		_, reason, _ = strings.Cut(r.Status, " ")
	}
	var header *string
	if values := r.Header.Values("Location"); len(values) > 0 {
		header = &values[0]
	}
	return statusErrorText(r.StatusCode, reason, rawURL, header)
}

// HTTPStatusError message, the redirect location added only for the five codes httpx follows
func statusErrorText(status int, reason, rawURL string, location *string) string {
	kind := map[int]string{1: "Informational response", 3: "Redirect response", 4: "Client error", 5: "Server error"}[status/100]
	if kind == "" {
		kind = "Invalid status code"
	}
	msg := fmt.Sprintf("%s '%d %s' for url '%s'", kind, status, reason, rawURL)
	switch status {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		if location != nil {
			msg += fmt.Sprintf("\nRedirect location: '%s'", *location)
		}
	}
	return msg + fmt.Sprintf("\nFor more information check: https://developer.mozilla.org/en-US/docs/Web/HTTP/Status/%d", status)
}
