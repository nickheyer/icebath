package provider

import (
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Gateway functions provider reads, installed by the gateway package at init, each answering for no gateway while unset
var GatewayHooks struct {
	LegacyJSON            func(h string) map[string]any
	BridgedAllowAllUsers  func() (string, bool)
	MergePlatformSections func(yamlCfg map[string]any, gatewayBlock any, gwData map[string]any, also []string) map[string]any
	PlatformEnabled       func(block map[string]any) (bool, string)
	ServesHome            func(h string) bool
}

// Mapping of the home's legacy gateway.json, nil when no gateway is linked or the file holds none
func gatewayLegacyJSON(h string) map[string]any {
	if GatewayHooks.LegacyJSON == nil {
		return nil
	}
	return GatewayHooks.LegacyJSON(h)
}

// Allow all users value the gateway copied from its config into the environment, unset without a gateway
func gatewayBridgedAllowAllUsers() (string, bool) {
	if GatewayHooks.BridgedAllowAllUsers == nil {
		return "", false
	}
	return GatewayHooks.BridgedAllowAllUsers()
}

// Platform blocks of config.yaml merged into the gateway platforms mapping, unchanged without a gateway
func gatewayMergePlatformSections(yamlCfg map[string]any, gatewayBlock any, gwData map[string]any, also []string) map[string]any {
	if GatewayHooks.MergePlatformSections == nil {
		return gwData
	}
	return GatewayHooks.MergePlatformSections(yamlCfg, gatewayBlock, gwData, also)
}

// Enabled flag and token of one platform block as the gateway reads it, disabled without a gateway
func gatewayPlatformEnabled(block map[string]any) (bool, string) {
	if GatewayHooks.PlatformEnabled == nil {
		return false, ""
	}
	return GatewayHooks.PlatformEnabled(block)
}

// Whether a live gateway serves the home, false without a gateway linked or on any error
func gatewayServesHome(h string) bool {
	if GatewayHooks.ServesHome == nil {
		return false
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
