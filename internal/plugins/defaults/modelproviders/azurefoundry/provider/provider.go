package provider

import (
	"strings"

	"github.com/nickheyer/icebath/internal/config"
	azurefoundryconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/azurefoundry/config"
	"github.com/nickheyer/icebath/internal/provider"
)

var responsesPrefixes = []string{"gpt-5", "codex", "gpt-codex", "o1", "o3", "o4"}

// Ways of signing in to Azure Foundry
var modes = []config.Option{
	{Value: azurefoundryconfig.APIKey, Title: "Key", About: "An API key"},
	{Value: azurefoundryconfig.EntraID, Title: "Entra ID", About: "Your Microsoft Entra sign-in"},
}

type specs struct{}

func Register(registry *provider.Registry) {
	registry.Register(specs{})
}

func (specs) Specs() []provider.Spec {
	base := azurefoundryconfig.BaseURL
	return []provider.Spec{{
		Name:               azurefoundryconfig.Name,
		DisplayName:        azurefoundryconfig.Title,
		Description:        "OpenAI-compatible; set your endpoint as the base URL",
		SignupURL:          "https://ai.azure.com/",
		BaseURL:            base,
		BaseURLEnv:         azurefoundryconfig.BaseURLEnv,
		ModelsURL:          provider.ModelsURL(base),
		Mode:               provider.ModeChatCompletions,
		Auth:               provider.AuthAPIKey,
		AuthType:           provider.AuthTypeAPIKey,
		KeyEnv:             []string{azurefoundryconfig.APIKeyEnv},
		Aliases:            []string{"azure", "azure-ai-foundry", "azure-ai"},
		DefaultModel:       azurefoundryconfig.DefaultModel,
		ModelListing:       true,
		HealthCheck:        true,
		VisionToolMessages: true,
		PerModelMode:       mode,
		Resolve:            resolve,
		Settings: []config.Setting{
			{Key: azurefoundryconfig.AuthMode, Title: "Azure sign-in", About: "How icebath signs in to Azure Foundry", Default: azurefoundryconfig.APIKey,
				Options: config.Options(modes...)},
			{Key: azurefoundryconfig.EntraScope, Title: "Entra scope", About: "Scope of the Microsoft Entra ID tokens, https://ai.azure.com/.default when empty",
				Default: ""},
		},
		Account: []config.Setting{
			{Key: azurefoundryconfig.AuthMode, Title: "Azure sign-in", About: "How this account signs in, the provider's way when empty", Default: "",
				Options: config.Options(append([]config.Option{{Value: "", Title: "The provider's", About: "The sign-in the provider is set to"}}, modes...)...)},
			{Key: azurefoundryconfig.TenantID, Title: "Tenant ID", About: "Microsoft Entra tenant of the account's service principal, AZURE_TENANT_ID when empty",
				Default: ""},
			{Key: azurefoundryconfig.ClientID, Title: "Client ID", About: "Application of the service principal or user-assigned managed identity, AZURE_CLIENT_ID when empty",
				Default: ""},
			{Key: azurefoundryconfig.ClientSecret, Title: "Client secret", About: "Secret of the service principal, AZURE_CLIENT_SECRET when empty",
				Default: "", Secret: true},
			{Key: azurefoundryconfig.CertificatePath, Title: "Certificate", About: "PEM or PFX certificate of the service principal, AZURE_CLIENT_CERTIFICATE_PATH when empty",
				Default: ""},
			{Key: azurefoundryconfig.CertificatePassword, Title: "Certificate password", About: "Password of the certificate, AZURE_CLIENT_CERTIFICATE_PASSWORD when empty",
				Default: "", Secret: true},
		},
	}}
}

func mode(model string) provider.APIMode {
	raw := strings.ToLower(strings.TrimSpace(model))
	raw = raw[strings.LastIndex(raw, "/")+1:]
	if raw == "" {
		return ""
	}
	for _, prefix := range responsesPrefixes {
		if strings.HasPrefix(raw, prefix) {
			return provider.ModeCodexResponses
		}
	}
	return ""
}

// The kind as one provider has it: a Foundry endpoint of Claude, which its address names with an anthropic path or its API setting names,
// speaks the Anthropic Messages API whatever the model; every other takes chat completions and the Responses API by the model
func resolve(spec provider.Spec, _ config.View) provider.Spec {
	if spec.Mode == provider.ModeAnthropicMessages || provider.ModeForURL(spec.BaseURL) == provider.ModeAnthropicMessages {
		spec.Mode, spec.PerModelMode = provider.ModeAnthropicMessages, nil
	}
	return spec
}
