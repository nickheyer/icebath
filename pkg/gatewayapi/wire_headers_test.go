package gatewayapi

import (
	"net/http"
	"testing"

	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// An old prefix request header reaches handlers under the icebath name, the icebath value winning a clash
func TestLegacyRequestHeadersReachIcebathNames(t *testing.T) {
	h := http.Header{}
	h.Set("X-Icebath-Session-Id", "new")
	h.Set("X-Hermes-Session-Id", "old")
	h.Set("X-Hermes-Session-Key", "key")
	acceptLegacyRequestHeaders(h)
	if h.Get("X-Icebath-Session-Id") != "new" || h.Get("X-Icebath-Session-Key") != "key" {
		t.Fatalf("%v", h)
	}
	plain := http.Header{}
	plain.Set("X-Icebath-Session-Id", "s")
	acceptLegacyRequestHeaders(plain)
	if plain.Get("X-Icebath-Session-Id") != "s" || len(plain) != 1 {
		t.Fatalf("%v", plain)
	}
}

// A replay record an older icebath stored answers under the icebath header names
func TestStoredReplayHeadersUseIcebathNames(t *testing.T) {
	old := storedHeaders(jsonutil.ObjectOf("X-Hermes-Session-Id", "s", "X-Hermes-Session-Key", "k"))
	if old["X-Icebath-Session-Id"] != "s" || old["X-Icebath-Session-Key"] != "k" || len(old) != 2 {
		t.Fatalf("%v", old)
	}
	current := storedHeaders(jsonutil.ObjectOf("X-Icebath-Session-Id", "s"))
	if current["X-Icebath-Session-Id"] != "s" || len(current) != 1 || len(storedHeaders(nil)) != 0 {
		t.Fatalf("%v", current)
	}
}
