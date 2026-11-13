package httpclient

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// Headers a strict redirect keeps once it leaves the first origin, everything else possibly carrying credentials
var strictSafeHeaders = map[string]bool{"Accept": true, "User-Agent": true}

// Headers carrying credentials, dropped once a forwarding redirect leaves the first origin
var credentialHeaders = []string{"Authorization", "Proxy-Authorization", "Cookie", "Cookie2"}

// Scheme, host and effective port that bound where credentials may go
type origin struct {
	scheme string
	host   string
	port   int
}

func originOf(u *url.URL) origin {
	host, port := targetHostPort(u)
	return origin{scheme: strings.ToLower(u.Scheme), host: host, port: port}
}

// Reports whether a strict redirect follows a status for the method, GET and HEAD always and POST only to GET
func strictFollows(method string, status int) bool {
	switch method {
	case http.MethodGet, http.MethodHead:
		return true
	case http.MethodPost:
		return status == http.StatusMovedPermanently || status == http.StatusFound || status == http.StatusSeeOther
	}
	return false
}

// Redirect check following at most max hops and stripping headers once any hop leaves the first origin
func redirectPolicy(max int, forward bool) func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if max <= 0 {
			return http.ErrUseLastResponse
		}
		if len(via) > max {
			return fmt.Errorf("%w, stopped after %d", ErrTooManyRedirects, max)
		}
		if !forward && req.Response != nil && !strictFollows(via[len(via)-1].Method, req.Response.StatusCode) {
			return http.ErrUseLastResponse
		}
		if via[0].Header.Get("Referer") == "" {
			req.Header.Del("Referer")
		}
		if !forward {
			req.Header.Del("Content-Type")
		}
		first := originOf(via[0].URL)
		left := originOf(req.URL) != first
		for _, hop := range via[1:] {
			left = left || originOf(hop.URL) != first
		}
		if !left {
			return nil
		}
		if forward {
			for _, name := range credentialHeaders {
				req.Header.Del(name)
			}
			return nil
		}
		for name := range req.Header {
			if !strictSafeHeaders[http.CanonicalHeaderKey(name)] {
				delete(req.Header, name)
			}
		}
		return nil
	}
}
