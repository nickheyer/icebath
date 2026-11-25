package config

import "testing"

func TestNormalizeBaseURLGivesActualRootsTheirV1Surface(t *testing.T) {
	for raw, want := range map[string]string{
		"":                           DefaultBaseURL,
		"  ":                         DefaultBaseURL,
		"https://api.actual.inc":     "https://api.actual.inc/v1",
		" https://API.actual.inc/ ":  "https://API.actual.inc/v1",
		"https://api.actual.inc/v1/": "https://api.actual.inc/v1",
		"http://localhost:8080":      "http://localhost:8080/v1",
		"http://127.0.0.1:8080/":     "http://127.0.0.1:8080/v1",
		"http://[::1]:8080":          "http://[::1]:8080/v1",
		"http://0.0.0.0:8080/api":    "http://0.0.0.0:8080/api",
		"https://relay.example.com":  "https://relay.example.com",
	} {
		if got := NormalizeBaseURL(raw); got != want {
			t.Errorf("NormalizeBaseURL(%q) = %q, want %q", raw, got, want)
		}
	}
	for raw, want := range map[string]bool{
		"http://LOCALHOST.:1234/v1": true,
		"http://127.0.0.1:8080":     true,
		"http://0.0.0.0":            true,
		"https://api.actual.inc/v1": false,
		"":                          false,
	} {
		if got := Loopback(raw); got != want {
			t.Errorf("Loopback(%q) = %v, want %v", raw, got, want)
		}
	}
}
