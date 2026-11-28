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
		SignIn:             true,
		Messages:           messages,
		ExtraBody:          extraBody,
		Shape:              shape,
		RequestHeaders:     headers,
		Native:             provider.Unprefixed,
		DisplayName:        qwenoauthconfig.Title,
		Description:        "Qwen models through your Qwen account, with no API key",
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
		Account: []config.Setting{
			{Key: qwenoauthconfig.AccessToken, Title: "Access token", About: "Your Qwen sign-in token.", Default: "", Secret: true, Internal: true},
			{Key: qwenoauthconfig.RefreshToken, Title: "Refresh token", About: "Your token that renews the sign-in before it expires.", Default: "", Secret: true,
				Internal: true},
			{Key: qwenoauthconfig.ResourceURL, Title: "Resource", About: "The host the sign-in was issued for.", Default: "", Internal: true},
			{Key: qwenoauthconfig.Expiry, Title: "Sign-in expiry", About: "When the access token expires.", Help: "A time in Unix milliseconds.", Default: 0,
				Internal: true},
		},
	}}
}
