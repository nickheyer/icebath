package provider

import (
	"strings"

	"github.com/nickheyer/icebath/internal/provider"
)

// Model id as Xiaomi names it: its own prefix dropped, in lower case
func native(spec provider.Spec, model string) string {
	return strings.ToLower(provider.Unprefixed(spec, model))
}
