package gatewayapi

import (
	"net/http"
	"testing"
)

// An icebath request header reaches handlers under the hermes name, the icebath value winning a clash
func TestIcebathRequestHeadersFeedHermesNames(t *testing.T) {
	h := http.Header{}
	h.Set("X-Icebath-Session-Id", "new")
	h.Set("X-Hermes-Session-Id", "old")
	h.Set("X-Icebath-Session-Key", "key")
	icebathRequestHeaders(h)
	if h.Get("X-Hermes-Session-Id") != "new" || h.Get("X-Hermes-Session-Key") != "key" {
		t.Fatalf("%v", h)
	}
	legacy := http.Header{}
	legacy.Set("X-Hermes-Session-Id", "old")
	icebathRequestHeaders(legacy)
	if legacy.Get("X-Hermes-Session-Id") != "old" {
		t.Fatalf("%v", legacy)
	}
}

// Every hermes response header also goes out under its icebath name
func TestIcebathResponseHeadersTwinHermesNames(t *testing.T) {
	h := http.Header{}
	h.Set("X-Hermes-Session-Id", "s")
	h.Set("X-Hermes-Completed", "false")
	h.Set("Content-Type", "application/json")
	icebathResponseHeaders(h)
	if h.Get("X-Icebath-Session-Id") != "s" || h.Get("X-Icebath-Completed") != "false" || h.Get("X-Icebath-Content-Type") != "" {
		t.Fatalf("%v", h)
	}
	m := withIcebathTwins(map[string]string{"X-Hermes-Session-Key": "k", "Cache-Control": "no-cache"})
	if m["X-Icebath-Session-Key"] != "k" || len(m) != 3 {
		t.Fatalf("%v", m)
	}
}
