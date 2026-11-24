package gateway

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/nickheyer/icebath/pkg/profiles"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	pathutil "github.com/nickheyer/icebath/pkg/utils/path"
)

// Setup status verb, whether the profile has any provider configured
func (c *AuthorityConnection) setupStatus(ctx context.Context, ref SessionRef, params *jsonutil.Object) (any, error) {
	return readiness(ctx, c.authority, c.actor, params, false)
}

// Profile bound setup probe that reports a failure in its answer instead of raising
func readiness(ctx context.Context, a *SessionAuthority, actor Principal, params *jsonutil.Object, runtime bool) (*jsonutil.Object, error) {
	if !actor.Capabilities["session:create"] || actor.ProfileID != a.ProfileID {
		return nil, runtimeRefusal("permission_denied")
	}
	allowed := map[string]bool{"profile": true}
	if runtime {
		allowed["provider"] = true
	}
	for _, key := range params.Keys() {
		if _, isStr := params.Value(key).(string); !allowed[key] || !isStr {
			return nil, runtimeRefusal("invalid_params")
		}
	}
	if profile, _ := params.ValueOr("profile", "").(string); profile != "" {
		if profiles.ValidateProfileName(profile) != nil || !profiles.ProfileExists(profile) {
			return nil, runtimeRefusal("profile_mismatch")
		}
		dir, err := profiles.GetProfileDir(profile)
		if err != nil {
			return nil, err
		}
		resolved, err := pathutil.Resolve(dir)
		if err != nil {
			return nil, err
		}
		if resolved != filepath.Clean(a.ProfileID) {
			return nil, runtimeRefusal("profile_mismatch")
		}
	}
	scoped := profileRuntimeScope(ctx, a.ProfileID, true)
	if runtime {
		requested, _ := params.ValueOr("provider", "").(string)
		answer, err := CheckRuntimeReadiness(scoped, requested, true)
		if err != nil {
			pkgLog().Debug(fmt.Sprintf("readiness probe failed: %s", err))
			return ReadinessFailure(err), nil
		}
		return answer, nil
	}
	return jsonutil.ObjectOf("provider_configured", HasAnyProviderConfigured(scoped, true)), nil
}
