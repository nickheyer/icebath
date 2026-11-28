package config

import "github.com/nickheyer/icebath/internal/config"

const (
	Name         = "azure-foundry"
	Title        = "Azure Foundry"
	BaseURL      = ""
	DefaultModel = ""
)

const (
	APIKeyEnv  = "AZURE_FOUNDRY_API_KEY"
	BaseURLEnv = "AZURE_FOUNDRY_BASE_URL"
)

// Settings of a provider, below its name. An account may set AuthMode too, which then wins over the provider's
const (
	AuthMode   = "auth_mode"
	EntraScope = "entra_scope"
)

// Ways AuthMode signs in
const (
	APIKey  = "api_key"
	EntraID = "entra_id"
)

// Fields of an account that signs in with a service principal of its own, each standing in for the AZURE_ variable of the same meaning
const (
	TenantID            = "tenant_id"
	ClientID            = "client_id"
	ClientSecret        = "client_secret"
	CertificatePath     = "certificate_path"
	CertificatePassword = "certificate_password"
)

// Field of an account that names the certificate of its service principal: the file its sign-in reads, which the account keeps by its
// whole path
var CertificateFile = config.Setting{Key: CertificatePath, Title: "Certificate", About: "The PEM or PFX certificate file of the service principal.",
	Help: "A path such as ~/certs/foundry.pem.", Default: "", Zero: "From AZURE_CLIENT_CERTIFICATE_PATH",
	Path: &config.Path{Kind: config.PathFile, Exists: true}}
