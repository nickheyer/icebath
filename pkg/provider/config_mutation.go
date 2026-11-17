package provider

import (
	"context"
	"fmt"
	"sync"

	"github.com/nickheyer/icebath/pkg/home"
)

// Refusal carrying a status code and the detail the user sees
type HTTPException struct {
	StatusCode int
	Detail     string
}

func (e *HTTPException) Error() string { return fmt.Sprintf("%d: %s", e.StatusCode, e.Detail) }

// Refusal with a status and detail
func httpException(status int, detail string) *HTTPException {
	return &HTTPException{StatusCode: status, Detail: detail}
}

var configMutationMu sync.Mutex

type configMutationKey struct{}

// Serializes config read modify write spans, a ctx already holding the lock passing straight through
func LockConfigMutation(ctx context.Context) (context.Context, func()) {
	if ctx.Value(configMutationKey{}) != nil {
		return ctx, func() {}
	}
	configMutationMu.Lock()
	return context.WithValue(ctx, configMutationKey{}, true), configMutationMu.Unlock
}

// Context bound to the secret scope of the home in ctx, hosting a second profile when it is not the process home
func currentProfileScope(ctx context.Context) context.Context {
	processHome := home.Process()
	profileDir := ""
	if home.Override(ctx) != "" {
		profileDir = home.Dir(ctx)
	}
	var secrets map[string]string
	if profileDir != "" && launchResolvedPath(profileDir) != launchResolvedPath(processHome) {
		ActivateMultiProfileHosting()
		HydrateProfileSecretSources(profileDir)
		secrets = BuildProfileSecretScope(profileDir)
	} else {
		secrets = LaunchSecretScopeCtx(ctx, processHome)
	}
	profileHome := processHome
	if profileDir != "" {
		ctx = home.WithOverride(ctx, profileDir)
		profileHome = profileDir
	}
	return WithSecretScope(ctx, secrets, profileHome)
}
