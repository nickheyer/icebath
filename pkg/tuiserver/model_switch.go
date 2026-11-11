package tuiserver

import (
	"context"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Binds home, secret and terminal scope of a profile home, the launch profile secrets always and its home once multiplexing
func profileRuntimeScope(ctx context.Context, profileHome string, hydrateSecrets bool) context.Context {
	if profileHome != "" {
		if hydrateSecrets {
			provider.HydrateProfileSecretSources(profileHome)
		}
		secrets := provider.BuildProfileSecretScope(profileHome)
		ctx = home.WithOverride(ctx, profileHome)
		ctx = provider.WithSecretScope(ctx, secrets, profileHome)
		return tools.WithProfileTerminalScope(ctx, profileHome, nil)
	}
	launch := launchHome()
	secrets := provider.LaunchSecretScopeCtx(ctx, launch)
	ctx = provider.WithSecretScope(ctx, secrets, "")
	if !provider.IsMultiplexActive(ctx) {
		return ctx
	}
	ctx = home.WithOverride(ctx, launch)
	return tools.WithProfileTerminalScope(ctx, launch, provider.LaunchTerminalEnv())
}

// Context bound to the model resolution, config and secrets of the session profile, launch profile included
func sessionProfileRuntimeScope(ctx context.Context, session *tuigateway.Session, hydrateSecrets bool) context.Context {
	return profileRuntimeScope(ctx, strOr(session.Get("profile_home")), hydrateSecrets)
}

// Model and provider a not yet built session of its own profile runs on, the pinned free tier route included
func sessionDefaultRoute(ctx context.Context, session *tuigateway.Session) (string, string) {
	scoped := profileRuntimeScope(ctx, strOr(session.Get("profile_home")), false)
	if _, explicitProvider := resolveStartupRuntime(scoped); explicitProvider == "" && provider.FreeTierRoute(scoped) {
		return provider.GuestModel, "nous"
	}
	return resolveModel(scoped), ""
}

// Model and provider session info reports for a not yet built session, the client's sticky pick else the profile default
func lazyInfoRoute(ctx context.Context, session *tuigateway.Session, override map[string]any) map[string]any {
	var model, providerName any
	if len(override) > 0 {
		model, providerName = override["model"], override["provider"]
	} else {
		m, p := sessionDefaultRoute(ctx, session)
		model, providerName = m, p
	}
	out := map[string]any{"model": model}
	if truthy(providerName) {
		out["provider"] = providerName
	}
	return out
}
