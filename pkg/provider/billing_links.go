package provider

import (
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
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

// Nous managed inference gateway by provider or host, the urlparse ValueError of a malformed base url returned
func isNousInferenceRoute(provider, baseURL string) (bool, error) {
	if pystr.Lower(pystr.Strip(provider)) == "nous" {
		return true, nil
	}
	return pyurl.BaseURLHostMatches(baseURL, "inference-api.nousresearch.com")
}

// IsNousInferenceRoute reports the Nous managed inference gateway by provider or host
func IsNousInferenceRoute(provider, baseURL string) bool {
	nous, err := isNousInferenceRoute(provider, baseURL)
	return err == nil && nous
}

// Python str.title of the text
func pyTitle(s string) string { return pystr.Title(s) }

// Label and billing page by exact slug, then base url host, then a readable label without a url
func resolveProviderLinkErr(slug, baseURL string) (string, string, error) {
	for _, p := range billingProviders {
		for _, s := range p.slugs {
			if s == slug {
				return p.label, p.url, nil
			}
		}
	}
	for _, p := range billingProviders {
		for _, host := range p.hosts {
			matches, err := pyurl.BaseURLHostMatches(baseURL, host)
			if err != nil {
				return "", "", err
			}
			if matches {
				return p.label, p.url, nil
			}
		}
	}
	label := pystr.Title(pystr.Strip(strings.ReplaceAll(strings.ReplaceAll(slug, "_", " "), "-", " ")))
	if label == "" {
		label = "your provider"
	}
	return label, "", nil
}

// Label and billing page of a provider, a readable label without a url when the base url is malformed
func resolveProviderLink(slug, baseURL string) (string, string) {
	label, url, err := resolveProviderLinkErr(slug, baseURL)
	if err != nil {
		return "", ""
	}
	return label, url
}

// BuildBillingBlockErr describes a billing classified failure, the urlparse ValueError of a malformed base url returned
func BuildBillingBlockErr(provider, baseURL, model, message string) (BillingBlock, error) {
	slug := pystr.Lower(pystr.Strip(provider))
	model = pystr.Strip(model)
	nous, err := isNousInferenceRoute(slug, baseURL)
	if err != nil {
		return BillingBlock{}, err
	}
	if nous {
		if slug == "" {
			slug = "nous"
		}
		return BillingBlock{Provider: slug, ProviderLabel: "Nous Portal", Model: model, BillingURL: NousPortalBillingURL(""), IsNous: true, Message: message}, nil
	}
	label, url, err := resolveProviderLinkErr(slug, baseURL)
	if err != nil {
		return BillingBlock{}, err
	}
	return BillingBlock{Provider: slug, ProviderLabel: label, Model: model, BillingURL: url, Message: message}, nil
}

// BuildBillingBlock describes a billing classified failure with the provider add credits page
func BuildBillingBlock(provider, baseURL, model, message string) BillingBlock {
	block, _ := BuildBillingBlockErr(provider, baseURL, model, message)
	return block
}
