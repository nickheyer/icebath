package gatewayapi

import (
	"net/http"
	"strings"
)

// Request headers sent under icebath names copied over the hermes names the handlers read, the icebath value winning
func icebathRequestHeaders(h http.Header) {
	var keys []string
	for key := range h {
		if strings.HasPrefix(key, "X-Icebath-") {
			keys = append(keys, key)
		}
	}
	for _, key := range keys {
		h["X-Hermes-"+strings.TrimPrefix(key, "X-Icebath-")] = append([]string(nil), h[key]...)
	}
}

// Every X-Hermes response header also sent under its X-Icebath name so clients of either product read it
func icebathResponseHeaders(h http.Header) {
	var keys []string
	for key := range h {
		if strings.HasPrefix(key, "X-Hermes-") {
			keys = append(keys, key)
		}
	}
	for _, key := range keys {
		h["X-Icebath-"+strings.TrimPrefix(key, "X-Hermes-")] = append([]string(nil), h[key]...)
	}
}

// Header map with an X-Icebath twin beside each X-Hermes entry, for responses that send headers before returning
func withIcebathTwins(headers map[string]string) map[string]string {
	for key, value := range headers {
		if strings.HasPrefix(key, "X-Hermes-") {
			headers["X-Icebath-"+strings.TrimPrefix(key, "X-Hermes-")] = value
		}
	}
	return headers
}
