package home

import (
	"strings"
)

// Busy indicator styles the TUI accepts and the one it starts with
var IndicatorStyles = []string{"ascii", "emoji", "unicode"}

const DefaultIndicatorStyle = "unicode"

// Size ceiling for an attachment staged on the host before the base64 read back
const AttachmentMaxBytes = 16 * 1024 * 1024

// Root directories of the default home that hold local models and runtimes
var LocalRuntimeRootDirs = map[string]bool{"models": true, "runtimes": true, "node": true}

const (
	OpenRouterBaseURL   = "https://openrouter.ai/api/v1"
	OpenRouterModelsURL = OpenRouterBaseURL + "/models"
	AIGatewayBaseURL    = "https://ai-gateway.vercel.sh/v1"
)

// OpenRouter request time routing suffixes valid on any model id and never separate catalog entries
var OpenRouterVariantSuffixes = map[string]bool{"nitro": true, "floor": true, "exacto": true, "online": true}

// Base model id when the id carries a routing variant suffix, empty otherwise
func OpenRouterVariantBase(modelID string) string {
	i := strings.LastIndex(modelID, ":")
	if i <= 0 {
		return ""
	}
	if OpenRouterVariantSuffixes[strings.ToLower(modelID[i+1:])] {
		return modelID[:i]
	}
	return ""
}
