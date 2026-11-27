package provider

// Pages the Gemini prices are published on
const (
	geminiPublished  = "https://ai.google.dev/gemini-api/docs/pricing"
	geminiPublished2 = "https://ai.google.dev/pricing"
)

// Prices Google publishes for its Gemini models, on AI Studio and Vertex alike
var GeminiPrices = map[string]Price{
	"gemini-4-argon":                {Input: "2.00", Output: "10.00", CacheRead: "0.10", URL: geminiPublished, Version: "google-pricing-2026-10-13"},
	"gemini-3.8-flash":              {Input: "0.75", Output: "3.75", CacheRead: "0.075", URL: geminiPublished, Version: "google-pricing-2026-09-02"},
	"gemini-3.7-flash":              {Input: "0.75", Output: "3.75", CacheRead: "0.075", URL: geminiPublished, Version: "google-pricing-2026-09-02"},
	"gemini-3.6-flash":              {Input: "1.50", Output: "7.50", CacheRead: "0.15", URL: geminiPublished, Version: "google-pricing-2026-07-28"},
	"gemini-3.5-flash-lite":         {Input: "0.30", Output: "2.50", CacheRead: "0.03", URL: geminiPublished, Version: "google-pricing-2026-07-28"},
	"gemini-3.5-flash":              {Input: "1.50", Output: "9.00", CacheRead: "0.15", URL: geminiPublished2, Version: "google-pricing-2026-07-07"},
	"gemini-3.1-flash-lite":         {Input: "0.25", Output: "1.50", CacheRead: "0.025", URL: geminiPublished2, Version: "google-pricing-2026-07-07"},
	"gemini-3-pro-preview":          {Input: "2.00", Output: "12.00", CacheRead: "0.20", URL: geminiPublished2, Version: "google-pricing-2026-07-07"},
	"gemini-3-flash-preview":        {Input: "0.50", Output: "3.00", CacheRead: "0.05", URL: geminiPublished2, Version: "google-pricing-2026-07-07"},
	"gemini-2.5-flash":              {Input: "0.15", Output: "0.60", CacheRead: "0.015", URL: geminiPublished2, Version: "google-pricing-2026-07-07"},
	"gemini-2.0-flash":              {Input: "0.10", Output: "0.40", CacheRead: "0.01", URL: geminiPublished2, Version: "google-pricing-2026-07-07"},
	"gemini-3.1-pro":                {Input: "2.00", Output: "12.00", CacheRead: "0.20", Threshold: 200000, Above: &Price{Input: "4.00", Output: "18.00", CacheRead: "0.40"}, URL: geminiPublished2, Version: "google-pricing-2026-07-07"},
	"gemini-2.5-pro":                {Input: "1.25", Output: "10.00", CacheRead: "0.125", Threshold: 200000, Above: &Price{Input: "2.50", Output: "15.00"}, URL: geminiPublished2, Version: "google-pricing-2026-07-07"},
	"gemini-3.1-pro-preview":        {Input: "2.00", Output: "12.00", CacheRead: "0.20", Threshold: 200000, Above: &Price{Input: "4.00", Output: "18.00", CacheRead: "0.40"}, URL: geminiPublished2, Version: "google-pricing-2026-07-07"},
	"gemini-3.1-flash-lite-preview": {Input: "0.25", Output: "1.50", CacheRead: "0.025", URL: geminiPublished2, Version: "google-pricing-2026-07-07"},
}
