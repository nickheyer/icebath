package web

import (
	"context"
	"net/url"
	"strings"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/credentials"
	"github.com/nickheyer/icebath/internal/provider"
)

// Credential of a kind of model provider for a web backend or tool: from the provider of that kind the profile has, else from the
// environment when the profile has none
func Credential(providers *provider.Registry, creds *credentials.Registry, cfg *config.Config, kind string) (credentials.Credential, bool) {
	for _, instance := range providers.Instances(cfg) {
		if instance.Known && instance.Spec.Name == kind {
			// The account the rotation gives, its sign-in renewed when due
			route, err := creds.Route(instance, "")
			if err != nil {
				return credentials.Credential{}, false
			}
			credential, ok := creds.Credential(credentials.Of(instance, route.Account))
			credential.APIKey = route.APIKey
			return credential, ok && strings.TrimSpace(credential.APIKey) != ""
		}
	}
	spec, ok := providers.Spec(kind)
	if !ok {
		return credentials.Credential{}, false
	}
	credential, ok := creds.Detect(cfg, spec)
	return credential, ok && strings.TrimSpace(credential.APIKey) != ""
}

// Renews the Grok sign-in of the profile now, after xAI refused a call made with it
func RenewXAI(ctx context.Context, providers *provider.Registry, creds *credentials.Registry, cfg *config.Config) error {
	for _, instance := range providers.Instances(cfg) {
		if instance.Known && instance.Spec.Name == XAIOAuth {
			route, err := creds.Route(instance, "")
			if err != nil {
				return err
			}
			return creds.Refresh(ctx, credentials.Of(instance, route.Account), true)
		}
	}
	return credentials.ErrNotRenewable
}

// Address of direct calls to xAI
const xaiBase = "https://api.x.ai/v1"

// Provider kinds of an xAI API key and of a Grok sign in
const (
	xaiKey   = "xai"
	XAIOAuth = "xai-oauth"
)

// Bearer of a direct call to xAI and the address it goes to
type XAI struct {
	Key     string
	BaseURL string
	// Provider kind the bearer comes from, xai or XAIOAuth
	Source string
}

// Bearer of direct calls to xAI: the API key first when preferred, else the Grok sign in first. An address set in ICEBATH_XAI_BASE_URL
// or XAI_BASE_URL is used only over HTTPS on x.ai, so the bearer never leaves xAI
func XAIBearer(providers *provider.Registry, creds *credentials.Registry, cfg *config.Config, preferKey bool) (XAI, bool) {
	kinds := []string{XAIOAuth, xaiKey}
	if preferKey {
		kinds = []string{xaiKey, XAIOAuth}
	}
	for _, kind := range kinds {
		credential, ok := Credential(providers, creds, cfg, kind)
		if !ok {
			continue
		}
		base := strings.TrimRight(strings.TrimSpace(credential.BaseURL), "/")
		if base == "" || !xaiOrigin(base) {
			base = xaiBase
		}
		if override := xaiOverride(cfg); override != "" && xaiOrigin(override) {
			base = override
		}
		return XAI{Key: strings.TrimSpace(credential.APIKey), BaseURL: base, Source: kind}, true
	}
	return XAI{}, false
}

// Address set in the environment for direct calls to xAI, empty when none is
func xaiOverride(cfg *config.Config) string {
	for _, name := range []string{"ICEBATH_XAI_BASE_URL", "XAI_BASE_URL"} {
		if value := strings.TrimRight(strings.TrimSpace(cfg.Getenv(name)), "/"); value != "" {
			return value
		}
	}
	return ""
}

// Whether an address is HTTPS on x.ai or a subdomain of it
func xaiOrigin(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return u.Scheme == "https" && (host == "x.ai" || strings.HasSuffix(host, ".x.ai"))
}
