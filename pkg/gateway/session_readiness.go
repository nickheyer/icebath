package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/profiles"
)

// Setup status verb, whether the profile has any provider configured
func (c *AuthorityConnection) setupStatus(ctx context.Context, ref SessionRef, params *pyjson.Object) (any, error) {
	return readiness(ctx, c.authority, c.actor, params, false)
}

// Setup runtime check verb, the readiness verdict of the runtime a session would build with
func (c *AuthorityConnection) setupRuntimeCheck(ctx context.Context, ref SessionRef, params *pyjson.Object) (any, error) {
	return readiness(ctx, c.authority, c.actor, params, true)
}

// Profile bound setup probe that reports a failure in its answer instead of raising
func readiness(ctx context.Context, a *SessionAuthority, actor Principal, params *pyjson.Object, runtime bool) (*pyjson.Object, error) {
	if !actor.Capabilities["session:create"] || actor.ProfileID != a.ProfileID {
		return nil, runtimeRefusal("permission_denied")
	}
	allowed := map[string]bool{"profile": true}
	if runtime {
		allowed["provider"] = true
	}
	for _, key := range params.Keys {
		if _, isStr := params.Vals[key].(string); !allowed[key] || !isStr {
			return nil, runtimeRefusal("invalid_params")
		}
	}
	if profile, _ := params.GetOr("profile", "").(string); profile != "" {
		if profiles.ValidateProfileName(profile) != nil || !profiles.ProfileExists(profile) {
			return nil, runtimeRefusal("profile_mismatch")
		}
		dir, err := profiles.GetProfileDir(profile)
		if err != nil {
			return nil, err
		}
		resolved, err := pypath.Resolve(dir)
		if err != nil {
			return nil, err
		}
		if resolved != filepath.Clean(a.ProfileID) {
			return nil, runtimeRefusal("profile_mismatch")
		}
	}
	scoped := profileRuntimeScope(ctx, a.ProfileID, true)
	if runtime {
		requested, _ := params.GetOr("provider", "").(string)
		answer, err := CheckRuntimeReadiness(scoped, requested, true)
		if err != nil {
			slog.Debug(fmt.Sprintf("readiness probe failed: %s", err))
			return ReadinessFailure(err), nil
		}
		return answer, nil
	}
	return pyjson.ObjectOf("provider_configured", HasAnyProviderConfigured(scoped, true)), nil
}
