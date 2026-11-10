package provider

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/profiles"
)

// HTTPException is a dashboard route refusal carrying the status code and the detail the response names
type HTTPException struct {
	StatusCode int
	Detail     string
}

func (e *HTTPException) Error() string { return fmt.Sprintf("%d: %s", e.StatusCode, e.Detail) }

// Route refusal with a status and detail
func httpException(status int, detail string) *HTTPException {
	return &HTTPException{StatusCode: status, Detail: detail}
}

// Refusal of an expected config backend rejection, nil for a write failure the caller must not explain
func configRefusalHTTP(err error) *HTTPException {
	var locked *config.ConfigLockedError
	if errors.As(err, &locked) {
		return httpException(403, err.Error())
	}
	var value *config.ConfigValueError
	if errors.As(err, &value) {
		status := 400
		if value.Code == "config_level_too_large" || value.Code == "config_body_too_large" {
			status = 413
		}
		return httpException(status, err.Error())
	}
	var write *config.ConfigWriteError
	if errors.As(err, &write) && write.Code == "config_version_conflict" {
		return httpException(409, err.Error())
	}
	return nil
}

// Route error of a failed handler body, refusals kept and anything else logged then answered with status
func httpFailure(err error, logMsg string, status int, prefix string, detail *string) error {
	if err == nil {
		return nil
	}
	if refusal, ok := err.(*HTTPException); ok {
		return refusal
	}
	var write *config.ConfigWriteError
	if errors.As(err, &write) {
		if refusal := configRefusalHTTP(err); refusal != nil {
			return refusal
		}
	}
	slog.Error(logMsg, "error", err)
	if detail != nil {
		return httpException(status, *detail)
	}
	return httpException(status, prefix+": "+err.Error())
}

var configMutationMu sync.Mutex

type configMutationKey struct{}

// Serializes dashboard config read modify write spans, a ctx already holding the lock passing straight through
func LockConfigMutation(ctx context.Context) (context.Context, func()) {
	if ctx.Value(configMutationKey{}) != nil {
		return ctx, func() {}
	}
	configMutationMu.Lock()
	return context.WithValue(ctx, configMutationKey{}, true), configMutationMu.Unlock
}

// None, empty and current all name the dashboard's own profile
func isCurrentProfile(profile string) bool {
	requested := pystr.Strip(profile)
	return requested == "" || pystr.Lower(requested) == "current"
}

// Directory of a valid existing profile, a 400 for an invalid name and a 404 for a missing one
func resolveProfileDir(name string) (string, error) {
	if err := profiles.ValidateProfileName(name); err != nil {
		return "", httpException(400, err.Error())
	}
	if !profiles.ProfileExists(name) {
		return "", httpException(404, fmt.Sprintf("Profile '%s' does not exist.", name))
	}
	return profiles.GetProfileDir(name)
}

// Context bound to a profile's home and secret scope, returning the scoped dir or empty for the process profile
func ConfigProfileScope(ctx context.Context, profile string) (context.Context, string, error) {
	processHome := home.Process()
	profileDir := ""
	if isCurrentProfile(profile) {
		if home.Override(ctx) != "" {
			profileDir = home.Dir(ctx)
		}
	} else {
		dir, err := resolveProfileDir(pystr.Strip(profile))
		if err != nil {
			return ctx, "", err
		}
		profileDir = dir
	}
	scoped := ""
	if profileDir != "" && launchResolvedPath(profileDir) != launchResolvedPath(processHome) {
		scoped = profileDir
	}
	var secrets map[string]string
	if scoped != "" {
		ActivateMultiProfileHosting()
		HydrateProfileSecretSources(scoped)
		secrets = BuildProfileSecretScope(scoped)
	} else {
		secrets = LaunchSecretScopeCtx(ctx, processHome)
	}
	profileHome := processHome
	if profileDir != "" {
		ctx = home.WithOverride(ctx, profileDir)
		profileHome = profileDir
	}
	return WithSecretScope(ctx, secrets, profileHome), scoped, nil
}
