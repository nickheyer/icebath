package gateway

import (
	"context"
	"path/filepath"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Refuses a verb reached through a stale connection to a retired authority
func requireServing(a *SessionAuthority) error {
	if a.Retired {
		return runtimeRefusal("runtime_retired")
	}
	return nil
}

// Runtime scope of the profile owning the authority under multiplex, the launch scope or ambient one otherwise
func ownerScope(ctx context.Context, a *SessionAuthority, hydrateSecrets bool) context.Context {
	if a.Runner == nil || !a.Runner.MultiplexProfiles() {
		return provider.WithLaunchProfileScopeIfMultiplexed(ctx)
	}
	profileHome := a.ProfileID
	if !filepath.IsAbs(profileHome) || !isDir(profileHome) {
		return ctx
	}
	return profileRuntimeScope(ctx, profileHome, hydrateSecrets)
}

// Scopes config, skills, memory, credentials and the complete terminal policy to one profile home
var profileRuntimeScope = func(ctx context.Context, profileHome string, hydrateSecrets bool) context.Context {
	ctx = home.WithOverride(ctx, profileHome)
	var secrets map[string]string
	if hydrateSecrets {
		secrets = loadProfileSecretScope(ctx, profileHome)
	} else {
		secrets = provider.OwnerSecretScope(ctx, profileHome)
	}
	ctx = provider.WithSecretScope(ctx, secrets, profileHome)
	return tools.WithProfileTerminalScope(ctx, profileHome, nil)
}

// Hydrates and loads one profile's secrets under its home override
func loadProfileSecretScope(ctx context.Context, profileHome string) map[string]string {
	ctx = home.WithOverride(ctx, profileHome)
	provider.HydrateProfileSecretSources(profileHome)
	return provider.OwnerSecretScope(ctx, profileHome)
}
