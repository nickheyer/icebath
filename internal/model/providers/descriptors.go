package providers

import (
	"context"
	"slices"

	"github.com/nickheyer/icebath/internal/config"
)

const (
	TabKeys     = "keys"
	TabAccounts = "accounts"
)

var accountsAuthTypes = []string{authTypeOAuthDeviceCode, authTypeOAuthExternal, authTypeOAuthMiniMax, authTypeExternalProcess}

func TabForAuthType(authType string) string {
	if slices.Contains(accountsAuthTypes, authType) {
		return TabAccounts
	}
	return TabKeys
}

type CanonicalEntry struct {
	Slug    string
	Label   string
	TUIDesc string
}

type Descriptor struct {
	Slug          string
	Label         string
	Description   string
	AuthType      string
	Tab           string
	APIKeyEnvVars []string
	BaseURLEnvVar string
	SignupURL     string
	Order         int
}

func (r *Registry) Descriptors(ctx context.Context, entries []CanonicalEntry, env *config.EnvRegistry) []Descriptor {
	profiles := map[string]*Spec{}
	for _, p := range r.All(ctx) {
		profiles[p.Spec().Name] = p.Spec()
	}
	out := make([]Descriptor, 0, len(entries))
	for order, entry := range entries {
		row, hasRow := r.RegistryRow(ctx, entry.Slug)
		prof := profiles[entry.Slug]
		overlay, hasOverlay := staticOverlays[entry.Slug]
		authType := ""
		switch {
		case hasRow && row.AuthType != "":
			authType = row.AuthType
		case prof != nil:
			authType = prof.AuthTypeOrDefault()
		case hasOverlay && overlay.AuthType != "":
			authType = overlay.AuthType
		default:
			authType = authTypeAPIKey
		}
		var keyVars []string
		urlVar := ""
		switch {
		case hasRow && len(row.APIKeyEnvVars) > 0:
			keyVars, urlVar = slices.Clone(row.APIKeyEnvVars), row.BaseURLEnvVar
		case prof != nil && len(prof.EnvVars) > 0:
			keyVars, urlVar = prof.KeyEnvVars(), prof.URLEnvVar()
		default:
			keyVars = []string{}
		}
		label := ""
		if prof != nil {
			label = prof.DisplayName
		}
		if label == "" {
			label = entry.Label
		}
		if label == "" {
			label = entry.Slug
		}
		signup := ""
		if prof != nil {
			signup = prof.SignupURL
		}
		if signup == "" && len(keyVars) > 0 && env != nil {
			if v, ok := env.Get(keyVars[0]); ok && v.URL != nil {
				signup = *v.URL
			}
		}
		description := ""
		if prof != nil {
			description = prof.Description
		}
		if description == "" {
			description = entry.TUIDesc
		}
		if description == "" {
			description = label
		}
		out = append(out, Descriptor{Slug: entry.Slug, Label: label, Description: description, AuthType: authType,
			Tab: TabForAuthType(authType), APIKeyEnvVars: keyVars, BaseURLEnvVar: urlVar, SignupURL: signup, Order: order})
	}
	return out
}

func registerEnvMetadata(reg *config.EnvRegistry, profiles []Profile) {
	for _, p := range profiles {
		s := p.Spec()
		switch s.AuthTypeOrDefault() {
		case authTypeAPIKey:
			reg.RegisterProvider(s.Label(), s.SignupURL, s.EnvVars)
		case authTypeExternalProcess:
			reg.RegisterLaunchEnv(append(slices.Clone(s.Process.CommandEnv), s.Process.ArgsEnv)...)
		}
	}
}
