package provider

import (
	"github.com/nickheyer/icebath/internal/config"
	qwenoauthconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/qwenoauth/config"
	"github.com/nickheyer/icebath/internal/provider"
)

type specs struct{}

func Register(registry *provider.Registry) {
	registry.Register(specs{})
}

func (specs) Specs() []provider.Spec {
	base := qwenoauthconfig.BaseURL
	return []provider.Spec{{
		Name:               qwenoauthconfig.Name,
		Messages:           messages,
		ExtraBody:          extraBody,
		Shape:              shape,
		RequestHeaders:     headers,
		Native:             provider.Unprefixed,
		DisplayName:        qwenoauthconfig.Title,
		BaseURL:            base,
		BaseURLEnv:         qwenoauthconfig.BaseURLEnv,
		ModelsURL:          provider.ModelsURL(base),
		Mode:               provider.ModeChatCompletions,
		Auth:               provider.AuthOAuth,
		AuthType:           provider.AuthTypeOAuthExternal,
		KeyEnv:             []string{qwenoauthconfig.APIKeyEnv},
		Aliases:            []string{"qwen", "qwen-portal", "qwen-cli"},
		DefaultModel:       qwenoauthconfig.DefaultModel,
		ModelListing:       true,
		HealthCheck:        true,
		VisionToolMessages: true,
		DefaultMaxTokens:   65536,
		ModelsDevID:        "alibaba",
		Settings: []config.Setting{{Key: qwenoauthconfig.AdoptExternalLogins, Title: "Use Qwen CLI login",
			About: "Use the Qwen CLI's login if one is on this machine", Default: true}},
		Account: []config.Setting{
			{Key: qwenoauthconfig.AccessToken, Title: "Access token", About: "Token of the Qwen sign-in", Default: "", Secret: true, Internal: true},
			{Key: qwenoauthconfig.RefreshToken, Title: "Refresh token", About: "Renews the access token before it expires", Default: "", Secret: true, Internal: true},
			{Key: qwenoauthconfig.ResourceURL, Title: "Resource", About: "Host the sign-in was issued for", Default: "", Internal: true},
			{Key: qwenoauthconfig.Expiry, Title: "Sign-in expiry", About: "When the access token expires, in Unix milliseconds", Default: 0, Internal: true},
		},
	}}
}
