package tuiserver

import (
	"context"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
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

// Context bound to the home, hydrated secrets and terminal policy of a profile home, empty meaning the launch profile
func ProfileRuntimeScope(ctx context.Context, profileHome string) context.Context {
	return profileRuntimeScope(ctx, profileHome, true)
}
