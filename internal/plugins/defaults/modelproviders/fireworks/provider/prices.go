package provider

import "github.com/nickheyer/icebath/internal/provider"

// Pages the prices are published on
const (
	published = "https://docs.fireworks.ai/serverless/pricing"
)

// Prices Fireworks publishes for its serverless models
var prices = map[string]provider.Price{
	"kimi-k2p6":           {Input: "0.95", Output: "4.00", CacheRead: "0.16", URL: published, Version: "fireworks-pricing-2026-07"},
	"kimi-k2p7-code":      {Input: "0.95", Output: "4.00", CacheRead: "0.19", URL: published, Version: "fireworks-pricing-2026-07"},
	"glm-5p2":             {Input: "1.40", Output: "4.40", CacheRead: "0.14", URL: published, Version: "fireworks-pricing-2026-07"},
	"deepseek-v4-pro":     {Input: "1.74", Output: "3.48", CacheRead: "0.145", URL: published, Version: "fireworks-pricing-2026-07"},
	"deepseek-v4-flash":   {Input: "0.14", Output: "0.28", CacheRead: "0.028", URL: published, Version: "fireworks-pricing-2026-07"},
	"qwen3p7-plus":        {Input: "0.40", Output: "1.60", CacheRead: "0.08", URL: published, Version: "fireworks-pricing-2026-07"},
	"minimax-m3":          {Input: "0.30", Output: "1.20", CacheRead: "0.06", URL: published, Version: "fireworks-pricing-2026-07"},
	"gpt-oss-120b":        {Input: "0.15", Output: "0.60", CacheRead: "0.015", URL: published, Version: "fireworks-pricing-2026-07"},
	"gpt-oss-20b":         {Input: "0.07", Output: "0.30", CacheRead: "0.035", URL: published, Version: "fireworks-pricing-2026-07"},
	"glm-5p1":             {Input: "1.40", Output: "4.40", CacheRead: "0.26", URL: published, Version: "fireworks-pricing-2026-07"},
	"minimax-m2p7":        {Input: "0.30", Output: "1.20", CacheRead: "0.06", URL: published, Version: "fireworks-pricing-2026-07"},
	"kimi-k2p6-fast":      {Input: "2.00", Output: "8.00", CacheRead: "0.30", URL: published, Version: "fireworks-pricing-2026-07"},
	"kimi-k2p6-turbo":     {Input: "2.00", Output: "8.00", CacheRead: "0.30", URL: published, Version: "fireworks-pricing-2026-07"},
	"kimi-k2p7-code-fast": {Input: "1.90", Output: "8.00", CacheRead: "0.38", URL: published, Version: "fireworks-pricing-2026-07"},
	"glm-5p2-fast":        {Input: "2.10", Output: "6.60", CacheRead: "0.21", URL: published, Version: "fireworks-pricing-2026-07"},
	"glm-5p1-fast":        {Input: "2.80", Output: "8.80", CacheRead: "0.52", URL: published, Version: "fireworks-pricing-2026-07"},
}
