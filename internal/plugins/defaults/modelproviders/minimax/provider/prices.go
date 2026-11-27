package provider

import "github.com/nickheyer/icebath/internal/provider"

// Prices MiniMax publishes for its models
var prices = map[string]provider.Price{
	"minimax-m2.7": {Input: "0.30", Output: "1.20", Version: "minimax-pricing-2026-04"},
}
