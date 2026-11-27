package provider

import "github.com/nickheyer/icebath/internal/provider"

// Pages the prices are published on
const (
	published  = "https://platform.claude.com/docs/en/about-claude/pricing"
	published2 = "https://openrouter.ai/anthropic/claude-opus-4.8-fast"
	published3 = "https://platform.claude.com/docs/en/about-claude/pricing#fast-mode-pricing"
)

// Prices Anthropic publishes for its models
var prices = map[string]provider.Price{
	"claude-opus-4-8":            {Input: "5.00", Output: "25.00", CacheRead: "0.50", CacheWrite: "6.25", URL: published, Version: "anthropic-pricing-2026-05"},
	"claude-opus-4-7":            {Input: "5.00", Output: "25.00", CacheRead: "0.50", CacheWrite: "6.25", URL: published, Version: "anthropic-pricing-2026-05"},
	"claude-opus-4-7-20250507":   {Input: "5.00", Output: "25.00", CacheRead: "0.50", CacheWrite: "6.25", URL: published, Version: "anthropic-pricing-2026-05"},
	"claude-opus-4-6":            {Input: "5.00", Output: "25.00", CacheRead: "0.50", CacheWrite: "6.25", URL: published, Version: "anthropic-pricing-2026-05"},
	"claude-opus-4-6-20250414":   {Input: "5.00", Output: "25.00", CacheRead: "0.50", CacheWrite: "6.25", URL: published, Version: "anthropic-pricing-2026-05"},
	"claude-opus-4-5":            {Input: "5.00", Output: "25.00", CacheRead: "0.50", CacheWrite: "6.25", URL: published, Version: "anthropic-pricing-2026-05"},
	"claude-sonnet-4-6":          {Input: "3.00", Output: "15.00", CacheRead: "0.30", CacheWrite: "3.75", URL: published, Version: "anthropic-pricing-2026-05"},
	"claude-sonnet-4-6-20250414": {Input: "3.00", Output: "15.00", CacheRead: "0.30", CacheWrite: "3.75", URL: published, Version: "anthropic-pricing-2026-05"},
	"claude-sonnet-4-5":          {Input: "3.00", Output: "15.00", CacheRead: "0.30", CacheWrite: "3.75", URL: published, Version: "anthropic-pricing-2026-05"},
	"claude-sonnet-4-20250514":   {Input: "3.00", Output: "15.00", CacheRead: "0.30", CacheWrite: "3.75", URL: published, Version: "anthropic-pricing-2026-05"},
	"claude-3-5-sonnet-20241022": {Input: "3.00", Output: "15.00", CacheRead: "0.30", CacheWrite: "3.75", URL: published, Version: "anthropic-pricing-2026-05"},
	"claude-haiku-4-5":           {Input: "1.00", Output: "5.00", CacheRead: "0.10", CacheWrite: "1.25", URL: published, Version: "anthropic-pricing-2026-05"},
	"claude-opus-4-20250514":     {Input: "15.00", Output: "75.00", CacheRead: "1.50", CacheWrite: "18.75", URL: published, Version: "anthropic-pricing-2026-05"},
	"claude-3-opus-20240229":     {Input: "15.00", Output: "75.00", CacheRead: "1.50", CacheWrite: "18.75", URL: published, Version: "anthropic-pricing-2026-05"},
	"claude-3-5-haiku-20241022":  {Input: "0.80", Output: "4.00", CacheRead: "0.08", CacheWrite: "1.00", URL: published, Version: "anthropic-pricing-2026-05"},
	"claude-3-haiku-20240307":    {Input: "0.25", Output: "1.25", CacheRead: "0.03", CacheWrite: "0.30", URL: published, Version: "anthropic-pricing-2026-05"},
	"claude-opus-4-8-fast":       {Input: "10.00", Output: "50.00", CacheRead: "1.00", CacheWrite: "12.50", URL: published2, Version: "anthropic-pricing-2026-05"},
	"claude-sonnet-5":            {Input: "2.00", Output: "10.00", CacheRead: "0.20", CacheWrite: "2.50", URL: published, Version: "anthropic-pricing-2026-06-intro"},
	"claude-opus-5":              {Input: "5.00", Output: "25.00", CacheRead: "0.50", CacheWrite: "6.25", URL: published, Version: "anthropic-pricing-2026-09"},
	"claude-opus-5-5":            {Input: "4.00", Output: "20.00", CacheRead: "0.20", CacheWrite: "5.00", URL: published, Version: "anthropic-pricing-2026-09"},
}

// Prices of the requests fast mode served
var fast = map[string]provider.Price{
	"claude-opus-4-8": {Input: "10.00", Output: "50.00", CacheRead: "1.00", CacheWrite: "12.50", URL: published3, Version: "anthropic-fast-mode-2026-09"},
	"claude-opus-5":   {Input: "10.00", Output: "50.00", CacheRead: "1.00", CacheWrite: "12.50", URL: published3, Version: "anthropic-fast-mode-2026-09"},
	"claude-opus-5-5": {Input: "8.00", Output: "40.00", CacheRead: "0.40", CacheWrite: "10.00", URL: published3, Version: "anthropic-fast-mode-2026-09"},
}
