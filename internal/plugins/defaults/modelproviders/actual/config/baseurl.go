package config

import (
	"strings"

	"github.com/nickheyer/icebath/internal/provider"
)

func NormalizeBaseURL(baseURL string) string {
	trimmed := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if trimmed == "" {
		return DefaultBaseURL
	}
	if provider.BasePath(trimmed) == "" && (provider.BaseHost(trimmed) == "api.actual.inc" || Loopback(trimmed)) {
		return trimmed + "/v1"
	}
	return trimmed
}

func Loopback(baseURL string) bool {
	switch provider.BaseHost(baseURL) {
	case "localhost", "127.0.0.1", "::1", "0.0.0.0":
		return true
	}
	return false
}
