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
	{Value: azurefoundryconfig.APIKey, Title: "API key", About: "Sign in with an API key"},
	{Value: azurefoundryconfig.EntraID, Title: "Entra ID", About: "Sign in with Microsoft Entra ID"},
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
		Description:        "Models in your Azure Foundry project, with an API key or Entra ID",
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
			{Key: azurefoundryconfig.AuthMode, Title: "Sign-in method", About: "How icebath signs in to Azure Foundry.", Default: azurefoundryconfig.APIKey,
				Options: config.Options(modes...)},
			{Key: azurefoundryconfig.EntraScope, Title: "Entra scope", About: "The scope of the tokens that Microsoft Entra ID gives icebath.",
				Help: "Left empty, the scope is https://ai.azure.com/.default.", Default: "", Zero: "Azure AI scope", Advanced: true},
		},
		Account: []config.Setting{
			{Key: azurefoundryconfig.AuthMode, Title: "Sign-in method", About: "How this account signs in to Azure Foundry.", Default: "",
				Options: config.Options(append([]config.Option{{Value: "", Title: "Same as the provider", About: "The method the provider is set to"}},
					modes...)...)},
			{Key: azurefoundryconfig.TenantID, Title: "Tenant ID", About: "The ID of the Microsoft Entra tenant that the service principal is in.",
				Help: "Such as 72f988bf-86f1-41af-91ab-2d7cd011db47, from the Azure portal.", Default: "", Zero: "From AZURE_TENANT_ID"},
			{Key: azurefoundryconfig.ClientID, Title: "Client ID", About: "The app ID of the service principal or managed identity.",
				Help: "Such as 0d4b6e1a-1b2c-4d3e-8f9a-0b1c2d3e4f5a, from the Azure portal.", Default: "", Zero: "From AZURE_CLIENT_ID"},
			{Key: azurefoundryconfig.ClientSecret, Title: "Client secret", About: "Your client secret for the service principal.",
				Help: "Left empty, the variable AZURE_CLIENT_SECRET is used.", Default: "", Secret: true},
			azurefoundryconfig.CertificateFile,
			{Key: azurefoundryconfig.CertificatePassword, Title: "Certificate password", About: "Your password for the certificate file.",
				Help: "Left empty, the variable AZURE_CLIENT_CERTIFICATE_PASSWORD is used.", Default: "", Secret: true},
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
