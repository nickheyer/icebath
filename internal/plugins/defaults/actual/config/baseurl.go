package config

import (
	"strings"

	urlutil "github.com/nickheyer/icebath/pkg/utils/url"
)

func NormalizeBaseURL(baseURL string) string {
	trimmed := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if trimmed == "" {
		return DefaultBaseURL
	}
	host, _ := urlutil.BaseHost(trimmed)
	path, _ := urlutil.BasePath(trimmed)
	if path == "" && (host == "api.actual.inc" || Loopback(trimmed)) {
		return trimmed + "/v1"
	}
	return trimmed
}

func Loopback(baseURL string) bool {
	host, _ := urlutil.BaseHost(baseURL)
	switch host {
	case "localhost", "127.0.0.1", "::1", "0.0.0.0":
		return true
	}
	return false
}
