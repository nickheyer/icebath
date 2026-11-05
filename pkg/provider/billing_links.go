package provider

import (
	"strings"
	"unicode"
)

// BillingBlock is the structured billing wall descriptor every surface renders
type BillingBlock struct {
	Provider      string
	ProviderLabel string
	Model         string
	BillingURL    string
	IsNous        bool
	Message       string
}

// ToDict is the wire form of the billing block with a null url when none is known
func (b BillingBlock) ToDict() map[string]any {
	var url any
	if b.BillingURL != "" {
		url = b.BillingURL
	}
	return map[string]any{"provider": b.Provider, "provider_label": b.ProviderLabel, "model": b.Model, "billing_url": url, "is_nous": b.IsNous, "message": b.Message}
}

type billingProvider struct {
	label, url   string
	slugs, hosts []string
}

var billingProviders = []billingProvider{
	{"OpenAI", "https://platform.openai.com/settings/organization/billing", []string{"openai"}, []string{"api.openai.com"}},
	{"Anthropic", "https://console.anthropic.com/settings/billing", []string{"anthropic"}, []string{"api.anthropic.com"}},
	{"OpenRouter", "https://openrouter.ai/settings/credits", []string{"openrouter"}, []string{"openrouter.ai"}},
	{"xAI", "https://console.x.ai/team/default/billing", []string{"xai", "xai-oauth"}, []string{"api.x.ai"}},
	{"DeepSeek", "https://platform.deepseek.com/top_up", []string{"deepseek"}, []string{"api.deepseek.com"}},
	{"Groq", "https://console.groq.com/settings/billing", []string{"groq"}, []string{"api.groq.com"}},
	{"Mistral", "https://console.mistral.ai/billing", []string{"mistral"}, []string{"api.mistral.ai"}},
	{"Together AI", "https://api.together.ai/settings/billing", []string{"together"}, []string{"api.together.ai", "api.together.xyz"}},
	{"Fireworks AI", "https://fireworks.ai/account/billing", []string{"fireworks"}, []string{"fireworks.ai"}},
	{"Perplexity", "https://www.perplexity.ai/settings/api", []string{"perplexity"}, []string{"perplexity.ai"}},
	{"Google AI", "https://aistudio.google.com/app/billing", []string{"google", "gemini"}, []string{"generativelanguage.googleapis.com"}},
	{"Cohere", "https://dashboard.cohere.com/billing", []string{"cohere"}, nil},
	{"Moonshot AI", "https://platform.moonshot.ai/console/pay", []string{"moonshot"}, nil},
	{"NVIDIA", "https://build.nvidia.com/settings/billing", []string{"nvidia"}, nil},
}

// IsNousInferenceRoute reports the Nous managed inference gateway by provider or host
func IsNousInferenceRoute(provider, baseURL string) bool {
	return strings.ToLower(strings.TrimSpace(provider)) == "nous" || baseURLHostMatches(baseURL, "inference-api.nousresearch.com")
}

// Python str.title, a letter after a non letter is upper cased and every other letter lower cased
func pyTitle(s string) string {
	var b strings.Builder
	prevCased := false
	for _, r := range s {
		switch {
		case unicode.IsLetter(r) && !prevCased:
			b.WriteRune(unicode.ToTitle(r))
		case unicode.IsLetter(r):
			b.WriteRune(unicode.ToLower(r))
		default:
			b.WriteRune(r)
		}
		prevCased = unicode.IsLetter(r)
	}
	return b.String()
}

// Label and billing page by exact slug, then base url host, then a readable label without a url
func resolveProviderLink(slug, baseURL string) (string, string) {
	for _, p := range billingProviders {
		for _, s := range p.slugs {
			if s == slug {
				return p.label, p.url
			}
		}
	}
	for _, p := range billingProviders {
		for _, host := range p.hosts {
			if baseURLHostMatches(baseURL, host) {
				return p.label, p.url
			}
		}
	}
	label := pyTitle(strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(slug, "_", " "), "-", " ")))
	if label == "" {
		label = "your provider"
	}
	return label, ""
}

// BuildBillingBlock describes a billing classified failure with the provider add credits page
func BuildBillingBlock(provider, baseURL, model, message string) BillingBlock {
	slug := strings.ToLower(strings.TrimSpace(provider))
	model = strings.TrimSpace(model)
	if IsNousInferenceRoute(slug, baseURL) {
		if slug == "" {
			slug = "nous"
		}
		return BillingBlock{Provider: slug, ProviderLabel: "Nous Portal", Model: model, BillingURL: NousPortalBillingURL(""), IsNous: true, Message: message}
	}
	label, url := resolveProviderLink(slug, baseURL)
	return BillingBlock{Provider: slug, ProviderLabel: label, Model: model, BillingURL: url, Message: message}
}
