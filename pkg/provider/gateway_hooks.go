package provider

import (
	"strings"
	"unicode/utf8"
)

// Gateway functions provider reads, installed by the gateway package at init, each answering for no gateway while unset
var GatewayHooks struct {
	ServesHome func(h string) bool
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
	cleaned := strings.TrimSpace(s)
	return utf8.RuneCountInString(cleaned) >= minLength && !placeholderSecretValues[strings.ToLower(cleaned)] && !isPlaceholderShape(cleaned)
}
