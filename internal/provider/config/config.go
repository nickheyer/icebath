package config

import (
	"slices"

	"github.com/nickheyer/icebath/internal/config"
)

// Providers of a profile, each under its name
const Key = "providers"

// Names of the settings of one provider, below its name
const (
	// Kind of the provider, by the name of its spec
	Type         = "type"
	Title        = "title"
	BaseURL      = "base_url"
	DefaultModel = "default_model"
	Models       = "models"
	// Account in use
	Account = "account"
	// Accounts, each under its name, keeping its title and the fields of the kind
	Accounts = "accounts"
	// API key of an account
	APIKeyField = "api_key"
)

// Settings with their keys put below the prefix
func Under(prefix string, settings []config.Setting) []config.Setting {
	out := make([]config.Setting, len(settings))
	for i, setting := range settings {
		setting.Key = prefix + "." + setting.Key
		out[i] = setting
	}
	return out
}

// Settings every provider keeps, with the defaults of its kind
func Instance(kind, title, base, baseEnv, model string, accounts []string) []config.Setting {
	var env []string
	if baseEnv != "" {
		env = []string{baseEnv}
	}
	return []config.Setting{
		{Key: Type, Title: "Kind", About: "Kind of provider", Default: kind, Internal: true},
		{Key: Title, Title: "Name", About: "Name the provider is shown by", Default: title},
		{Key: BaseURL, Title: "Base URL", About: "API base URL", Default: base, Env: env},
		{Key: DefaultModel, Title: "Default model", About: "Model used when none is picked", Default: model},
		{Key: Models, Title: "Models", About: "Models offered in /model; every model it lists when empty", Default: []string{}},
		{Key: Account, Title: "Account", About: "Account requests are made with", Default: "", Options: func() []config.Option {
			options := []config.Option{{Title: "First"}}
			for _, name := range slices.Clone(accounts) {
				options = append(options, config.Option{Value: name, Title: name})
			}
			return options
		}},
	}
}

// Title every account keeps
func AccountTitle(name string) config.Setting {
	return config.Setting{Key: Title, Title: "Name", About: "Name the account is shown by", Default: name}
}

// API key of an account, the field an account keeps when its kind names none
func APIKey() config.Setting {
	return config.Setting{Key: APIKeyField, Title: "API key", About: "API key", Default: "", Secret: true}
}
