package provider

import "github.com/nickheyer/icebath/internal/provider"

// Pages the prices are published on
const (
	published = "https://api-docs.deepseek.com/quick_start/pricing"
)

// Prices DeepSeek publishes for its models
var prices = map[string]provider.Price{
	"deepseek-flash":    {Input: "0.15", Output: "0.60", CacheRead: "0.003", URL: published, Version: "deepseek-pricing-2026-09-10"},
	"deepseek-v4-flash": {Input: "0.15", Output: "0.60", CacheRead: "0.003", URL: published, Version: "deepseek-pricing-2026-09-10"},
	"deepseek-chat":     {Input: "0.15", Output: "0.60", CacheRead: "0.003", URL: published, Version: "deepseek-pricing-2026-09-10"},
	"deepseek-reasoner": {Input: "0.15", Output: "0.60", CacheRead: "0.003", URL: published, Version: "deepseek-pricing-2026-09-10"},
	"deepseek-v4-pro":   {Input: "0.66", Output: "1.98", CacheRead: "0.022", URL: published, Version: "deepseek-pricing-2026-09-10"},
}
