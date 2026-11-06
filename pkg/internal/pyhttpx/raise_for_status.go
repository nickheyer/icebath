package pyhttpx

import (
	"fmt"
	"net/http"
)

// Text of the HTTPStatusError httpx raise_for_status raises for a non 2xx response
func RaiseForStatusMessage(status int, rawURL, location string) string {
	kind := map[int]string{1: "Informational response", 3: "Redirect response", 4: "Client error", 5: "Server error"}[status/100]
	if kind == "" {
		kind = "Invalid status code"
	}
	msg := fmt.Sprintf("%s '%d %s' for url '%s'", kind, status, http.StatusText(status), rawURL)
	if status/100 == 3 && location != "" {
		msg += fmt.Sprintf("\nRedirect location: '%s'", location)
	}
	return msg + fmt.Sprintf("\nFor more information check: https://developer.mozilla.org/en-US/docs/Web/HTTP/Status/%d", status)
}
