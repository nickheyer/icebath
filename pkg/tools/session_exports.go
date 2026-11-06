package tools

import (
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

// Bound session context variable for plugin sessions, the engaged session map before session values and process env
func SessionEnv(env *Env, name string) string { return sessionContextValue(env, name) }

// Scope aware TERMINAL_CWD value for plugin sessions, read through the per turn terminal scope
func ScopeTerminalCwd(env *Env) (string, error) { return scopeTerminalCwd(env) }

// Bound client tool environment for code execution children, installed by the agent package and nil until then
var SessionToolEnv func(env *Env) map[string]string

// IMPORTANT message a background process completion queue event injects into the conversation
func FormatProcessNotification(evt map[string]any) string { return formatProcessNotification(evt) }

// Terminates a host pid and its descendants after re validating its start time, a nil env reading the process home config
func TerminateHostPid(env *Env, pid int, expectedStart *int64) {
	if env == nil {
		h := home.Process()
		env = &Env{Home: h}
		if tree, err := config.Load(h, config.NewSecrets(h).Lookup()); err == nil {
			env.Config = tree
		}
	}
	processRegistry.terminateHostPid(env, pid, expectedStart)
}

// Argv of the first present PATH candidate for a command name, nil when it is not found
func LocateCommand(name string) []string { return locateCommand(name, nil, nil).Command() }

// Whether the Nous managed tool gateway serves this session, forceFresh bypassing the cached account read
func ManagedNousToolsEnabled(env *Env, forceFresh bool) bool {
	return managedNousToolsEnabled(env, forceFresh)
}

// Message naming why the Nous tool gateway is unavailable for a capability, with the account guidance
func NousToolGatewayUnavailableMessage(env *Env, capability string, forceFresh bool) string {
	return nousToolGatewayUnavailableMessage(env, capability, forceFresh)
}

// Provider, bearer key and base url for direct xAI calls, OAuth first unless the API key is preferred
func ResolveXAIHTTPCredentials(env *Env, preferAPIKey bool) (provider, apiKey, baseURL string) {
	c := resolveXAIHTTPCredentials(env, preferAPIKey)
	return c.Provider, c.APIKey, c.BaseURL
}

// Cheap xAI credential probe over the env key and the auth store tokens without any network call
func HasXAICredentials(env *Env) bool { return hasXAICredentials(env) }

// File sync manager of a remote backend environment, nil for backends that keep none
func EnvironmentSyncManager(e Environment) *FileSyncManager {
	switch b := e.(type) {
	case *sshEnv:
		return b.syncManager
	case *modalEnv:
		return b.syncManager
	case *daytonaEnv:
		b.lock.Lock()
		defer b.lock.Unlock()
		return b.syncManager
	case *vercelEnv:
		b.lock.Lock()
		defer b.lock.Unlock()
		return b.syncManager
	}
	return nil
}
