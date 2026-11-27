package provider

import "github.com/nickheyer/icebath/internal/provider"

// Pages the prices are published on
const (
	published  = "https://openai.com/index/previewing-gpt-5-6-sol/"
	published2 = "https://openai.com/api/pricing/"
	published3 = "https://developers.openai.com/api/docs/models/gpt-6-astra"
	published4 = "https://developers.openai.com/api/docs/models/gpt-6-sol"
	published5 = "https://developers.openai.com/api/docs/models/gpt-6.1-sol"
	published6 = "https://developers.openai.com/api/docs/models/gpt-6-luna"
	published7 = "https://developers.openai.com/api/docs/pricing?latest-pricing=ultrafast"
)

// Prices OpenAI publishes for its models
var prices = map[string]provider.Price{
	"gpt-5.6-sol":        {Input: "5.00", Output: "30.00", CacheRead: "0.50", CacheWrite: "6.25", URL: published, Version: "openai-gpt-5.6-2026-07"},
	"gpt-5.6-terra":      {Input: "2.50", Output: "15.00", CacheRead: "0.25", CacheWrite: "3.125", URL: published, Version: "openai-gpt-5.6-2026-07"},
	"gpt-5.6-luna":       {Input: "1.00", Output: "6.00", CacheRead: "0.10", CacheWrite: "1.25", URL: published, Version: "openai-gpt-5.6-2026-07"},
	"gpt-4o":             {Input: "2.50", Output: "10.00", CacheRead: "1.25", URL: published2, Version: "openai-pricing-2026-03-16"},
	"gpt-4o-mini":        {Input: "0.15", Output: "0.60", CacheRead: "0.075", URL: published2, Version: "openai-pricing-2026-03-16"},
	"gpt-4.1":            {Input: "2.00", Output: "8.00", CacheRead: "0.50", URL: published2, Version: "openai-pricing-2026-03-16"},
	"gpt-4.1-mini":       {Input: "0.40", Output: "1.60", CacheRead: "0.10", URL: published2, Version: "openai-pricing-2026-03-16"},
	"gpt-4.1-nano":       {Input: "0.10", Output: "0.40", CacheRead: "0.025", URL: published2, Version: "openai-pricing-2026-03-16"},
	"o3":                 {Input: "10.00", Output: "40.00", CacheRead: "2.50", URL: published2, Version: "openai-pricing-2026-03-16"},
	"o3-mini":            {Input: "1.10", Output: "4.40", CacheRead: "0.55", URL: published2, Version: "openai-pricing-2026-03-16"},
	"gpt-6-astra":        {Input: "10.00", Output: "50.00", CacheRead: "1.00", CacheWrite: "12.50", Threshold: 272000, Above: &provider.Price{Input: "20.00", Output: "75.00", CacheRead: "2.00", CacheWrite: "25.00"}, URL: published3, Version: "openai-gpt-6-astra-2026-09"},
	"gpt-6-sol":          {Input: "2.00", Output: "10.00", CacheRead: "0.20", CacheWrite: "2.50", Threshold: 272000, Above: &provider.Price{Input: "4.00", Output: "15.00", CacheRead: "0.40", CacheWrite: "5.00"}, URL: published4, Version: "openai-gpt-6-tiers-2026-09"},
	"gpt-6.1-sol":        {Input: "2.00", Output: "10.00", CacheRead: "0.10", CacheWrite: "2.50", Threshold: 272000, Above: &provider.Price{Input: "4.00", Output: "15.00", CacheRead: "0.20", CacheWrite: "5.00"}, URL: published5, Version: "openai-gpt-6-tiers-2026-09"},
	"gpt-6-luna":         {Input: "0.10", Output: "0.50", CacheRead: "0.01", CacheWrite: "0.125", Threshold: 272000, Above: &provider.Price{Input: "0.20", Output: "0.75", CacheRead: "0.02", CacheWrite: "0.25"}, URL: published6, Version: "openai-gpt-6-tiers-2026-09"},
	"gpt-5.6-sol-pro":    {Input: "5.00", Output: "30.00", CacheRead: "0.50", CacheWrite: "6.25", URL: published, Version: "openai-gpt-5.6-2026-07"},
	"gpt-5.6-sol-900k":   {Input: "5.00", Output: "30.00", CacheRead: "0.50", CacheWrite: "6.25", URL: published, Version: "openai-gpt-5.6-2026-07"},
	"gpt-5.6-terra-pro":  {Input: "2.50", Output: "15.00", CacheRead: "0.25", CacheWrite: "3.125", URL: published, Version: "openai-gpt-5.6-2026-07"},
	"gpt-5.6-terra-900k": {Input: "2.50", Output: "15.00", CacheRead: "0.25", CacheWrite: "3.125", URL: published, Version: "openai-gpt-5.6-2026-07"},
	"gpt-5.6-luna-pro":   {Input: "1.00", Output: "6.00", CacheRead: "0.10", CacheWrite: "1.25", URL: published, Version: "openai-gpt-5.6-2026-07"},
	"gpt-5.6-luna-900k":  {Input: "1.00", Output: "6.00", CacheRead: "0.10", CacheWrite: "1.25", URL: published, Version: "openai-gpt-5.6-2026-07"},
	"gpt-6-sol-pro":      {Input: "2.00", Output: "10.00", CacheRead: "0.20", CacheWrite: "2.50", Threshold: 272000, Above: &provider.Price{Input: "4.00", Output: "15.00", CacheRead: "0.40", CacheWrite: "5.00"}, URL: published4, Version: "openai-gpt-6-tiers-2026-09"},
	"gpt-6-sol-900k":     {Input: "2.00", Output: "10.00", CacheRead: "0.20", CacheWrite: "2.50", Threshold: 272000, Above: &provider.Price{Input: "4.00", Output: "15.00", CacheRead: "0.40", CacheWrite: "5.00"}, URL: published4, Version: "openai-gpt-6-tiers-2026-09"},
	"gpt-6-luna-pro":     {Input: "0.10", Output: "0.50", CacheRead: "0.01", CacheWrite: "0.125", Threshold: 272000, Above: &provider.Price{Input: "0.20", Output: "0.75", CacheRead: "0.02", CacheWrite: "0.25"}, URL: published6, Version: "openai-gpt-6-tiers-2026-09"},
	"gpt-6-luna-900k":    {Input: "0.10", Output: "0.50", CacheRead: "0.01", CacheWrite: "0.125", Threshold: 272000, Above: &provider.Price{Input: "0.20", Output: "0.75", CacheRead: "0.02", CacheWrite: "0.25"}, URL: published6, Version: "openai-gpt-6-tiers-2026-09"},
	"gpt-6.1-sol-pro":    {Input: "2.00", Output: "10.00", CacheRead: "0.10", CacheWrite: "2.50", Threshold: 272000, Above: &provider.Price{Input: "4.00", Output: "15.00", CacheRead: "0.20", CacheWrite: "5.00"}, URL: published5, Version: "openai-gpt-6-tiers-2026-09"},
	"gpt-6.1-sol-900k":   {Input: "2.00", Output: "10.00", CacheRead: "0.10", CacheWrite: "2.50", Threshold: 272000, Above: &provider.Price{Input: "4.00", Output: "15.00", CacheRead: "0.20", CacheWrite: "5.00"}, URL: published5, Version: "openai-gpt-6-tiers-2026-09"},
}

// Prices of the requests the ultrafast tier served, six times the standard ones
var ultrafast = map[string]provider.Price{
	"gpt-6-astra": {Input: "60.00", Output: "300.00", CacheRead: "6.00", CacheWrite: "75.00", Threshold: 272000, Above: &provider.Price{Input: "120.00", Output: "450.00", CacheRead: "12.00", CacheWrite: "150.00"}, URL: published7, Version: "openai-ultrafast-2026-09"},
	"gpt-6.1-sol": {Input: "12.00", Output: "60.00", CacheRead: "0.60", CacheWrite: "15.00", Threshold: 272000, Above: &provider.Price{Input: "24.00", Output: "90.00", CacheRead: "1.20", CacheWrite: "30.00"}, URL: published7, Version: "openai-ultrafast-2026-10-08"},
}
