package provider

import (
	"strings"

	"github.com/nickheyer/icebath/internal/provider"
)

// Model id as Anthropic names it: its own prefix dropped and dashes for the dots of its version
func native(spec provider.Spec, model string) string {
	bare := provider.Unprefixed(spec, model)
	if strings.Contains(bare, "/") {
		return bare
	}
	return strings.ReplaceAll(bare, ".", "-")
}
