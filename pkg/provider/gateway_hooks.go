package provider

import (
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Gateway functions provider reads, installed by the gateway package at init
var GatewayHooks struct {
	LegacyJSON            func(h string) map[string]any
	BridgedAllowAllUsers  func() (string, bool)
	MergePlatformSections func(yamlCfg map[string]any, gatewayBlock any, gwData map[string]any, also []string) map[string]any
	PlatformEnabled       func(block map[string]any) (bool, string)
	ServesHome            func(h string) bool
}

// Legacy gateway.json mapping of the home, nil when the file holds no mapping
func gatewayLegacyJSON(h string) map[string]any {
	if GatewayHooks.LegacyJSON == nil {
		panic("provider: the gateway package must install GatewayHooks.LegacyJSON (gateway/config_loader.py load_legacy_gateway_json)")
	}
	return GatewayHooks.LegacyJSON(h)
}

// GATEWAY_ALLOW_ALL_USERS when the gateway config bridge wrote it
func gatewayBridgedAllowAllUsers() (string, bool) {
	if GatewayHooks.BridgedAllowAllUsers == nil {
		panic("provider: the gateway package must install GatewayHooks.BridgedAllowAllUsers (gateway/config_loader.py bridged_allow_all_users)")
	}
	return GatewayHooks.BridgedAllowAllUsers()
}

// Platform blocks merged into gwData platforms in the gateway loader order
func gatewayMergePlatformSections(yamlCfg map[string]any, gatewayBlock any, gwData map[string]any, also []string) map[string]any {
	if GatewayHooks.MergePlatformSections == nil {
		panic("provider: the gateway package must install GatewayHooks.MergePlatformSections (gateway/config_loader.py merge_platform_sections)")
	}
	return GatewayHooks.MergePlatformSections(yamlCfg, gatewayBlock, gwData, also)
}

// Enabled flag and token text of a platform block read as PlatformConfig.from_dict reads it
func gatewayPlatformEnabled(block map[string]any) (bool, string) {
	if GatewayHooks.PlatformEnabled == nil {
		panic("provider: the gateway package must install GatewayHooks.PlatformEnabled (gateway/config.py PlatformConfig.from_dict)")
	}
	return GatewayHooks.PlatformEnabled(block)
}

// Whether a live gateway serves the home, the uncached liveness ladder scoped to it, false on any error
func gatewayServesHome(h string) bool {
	if GatewayHooks.ServesHome == nil {
		panic("provider: the gateway package must install GatewayHooks.ServesHome (gateway/status.py resolve_gateway_liveness)")
	}
	return GatewayHooks.ServesHome(h)
}

// True for a string secret of at least minLength characters that is no known placeholder
func HasUsableSecret(value any, minLength int) bool {
	s, ok := value.(string)
	if !ok {
		return false
	}
	cleaned := pystr.Strip(s)
	return utf8.RuneCountInString(cleaned) >= minLength && !placeholderSecretValues[pystr.Lower(cleaned)] && !isPlaceholderShape(cleaned)
}
