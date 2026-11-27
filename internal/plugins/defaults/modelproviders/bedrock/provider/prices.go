package provider

import "github.com/nickheyer/icebath/internal/provider"

// Pages the prices are published on
const (
	published = "https://aws.amazon.com/bedrock/pricing/"
)

// Prices Amazon publishes for the models Bedrock serves
var prices = map[string]provider.Price{
	"anthropic.claude-opus-4-8":   {Input: "5.00", Output: "25.00", CacheRead: "0.50", CacheWrite: "6.25", URL: published, Version: "anthropic-list-2026-07"},
	"anthropic.claude-opus-4-7":   {Input: "5.00", Output: "25.00", CacheRead: "0.50", CacheWrite: "6.25", URL: published, Version: "anthropic-list-2026-07"},
	"anthropic.claude-opus-4-6":   {Input: "5.00", Output: "25.00", CacheRead: "0.50", CacheWrite: "6.25", URL: published, Version: "anthropic-list-2026-07"},
	"anthropic.claude-opus-5":     {Input: "5.00", Output: "25.00", CacheRead: "0.50", CacheWrite: "6.25", URL: published, Version: "anthropic-list-2026-09"},
	"anthropic.claude-opus-5-5":   {Input: "4.00", Output: "20.00", CacheRead: "0.20", CacheWrite: "5.00", URL: published, Version: "anthropic-list-2026-09"},
	"anthropic.claude-sonnet-5":   {Input: "3.00", Output: "15.00", CacheRead: "0.30", CacheWrite: "3.75", URL: published, Version: "bedrock-pricing-2026-06"},
	"anthropic.claude-sonnet-4-6": {Input: "3.00", Output: "15.00", CacheRead: "0.30", CacheWrite: "3.75", URL: published, Version: "bedrock-pricing-2026-04"},
	"anthropic.claude-sonnet-4-5": {Input: "3.00", Output: "15.00", CacheRead: "0.30", CacheWrite: "3.75", URL: published, Version: "bedrock-pricing-2026-04"},
	"anthropic.claude-haiku-4-5":  {Input: "0.80", Output: "4.00", CacheRead: "0.08", CacheWrite: "1.00", URL: published, Version: "bedrock-pricing-2026-04"},
	"amazon.nova-pro":             {Input: "0.80", Output: "3.20", URL: published, Version: "bedrock-pricing-2026-04"},
	"amazon.nova-lite":            {Input: "0.06", Output: "0.24", URL: published, Version: "bedrock-pricing-2026-04"},
	"amazon.nova-micro":           {Input: "0.035", Output: "0.14", URL: published, Version: "bedrock-pricing-2026-04"},
}
