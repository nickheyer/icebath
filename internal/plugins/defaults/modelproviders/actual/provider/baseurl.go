package provider

import (
	"strings"

	actualconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/actual/config"
	"github.com/nickheyer/icebath/internal/provider"
)

// Base URL of an Actual account: the default when none is set, /v1 added to its own host or a server of this machine named bare
func NormalizeBaseURL(baseURL string) string {
	trimmed := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if trimmed == "" {
		return actualconfig.DefaultBaseURL
	}
	if provider.BasePath(trimmed) == "" && (provider.BaseHost(trimmed) == "api.actual.inc" || Loopback(trimmed)) {
		return trimmed + "/v1"
	}
	return trimmed
}

// Whether the base URL names a server of this machine
func Loopback(baseURL string) bool {
	switch provider.BaseHost(baseURL) {
	case "localhost", "127.0.0.1", "::1", "0.0.0.0":
		return true
	}
	return false
}
