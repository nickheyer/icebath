package dashboard

import (
	"context"
	"fmt"
	"sync"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Directory of a valid existing profile, 400 for an invalid name and 404 for a missing one
func resolveProfileDir(name string) (string, error) {
	if err := profiles.ValidateProfileName(name); err != nil {
		return "", NewHTTPException(400, err.Error())
	}
	if !profiles.ProfileExists(name) {
		return "", NewHTTPException(404, fmt.Sprintf("Profile '%s' does not exist.", name))
	}
	return profiles.GetProfileDir(name)
}

// Context bound to the profile home and secret scope, the scoped dir empty when it is the process profile
func configProfileScope(ctx context.Context, profile string) (context.Context, string, error) {
	sctx, scoped, err := provider.ConfigProfileScope(ctx, profile)
	if err != nil {
		return ctx, "", asHTTPError(err)
	}
	return sctx, scoped, nil
}

var skillsProfileMu sync.Mutex

type skillsProfileKey struct{}

// Config profile scope plus the skills profile lock held until release, reentrant for a ctx already holding it
func profileScope(ctx context.Context, profile string) (context.Context, string, func(), error) {
	sctx, scoped, err := configProfileScope(ctx, profile)
	if err != nil {
		return ctx, "", func() {}, err
	}
	if sctx.Value(skillsProfileKey{}) != nil {
		return sctx, scoped, func() {}, nil
	}
	skillsProfileMu.Lock()
	return context.WithValue(sctx, skillsProfileKey{}, true), scoped, skillsProfileMu.Unlock, nil
}

// Dash p argv of a validated named profile, empty for none or current
func profileCLIArgs(profile string) ([]string, error) {
	requested := pystr.Strip(profile)
	if requested == "" || pystr.Lower(requested) == "current" {
		return []string{}, nil
	}
	if _, err := resolveProfileDir(requested); err != nil {
		return nil, err
	}
	name, err := profiles.NormalizeProfileName(requested)
	if err != nil {
		return nil, err
	}
	return []string{"-p", name}, nil
}
