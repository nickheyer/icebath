package browser

import (
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Cloud browser backend selected by browser.cloud_provider, each call reading the profile's secrets through env
type BrowserProvider interface {
	provider.ProviderBase
	IsAvailable(env *tools.Env) bool
	CreateSession(env *tools.Env, taskID string) (map[string]any, error)
	CloseSession(env *tools.Env, sessionID string) bool
	EmergencyCleanup(env *tools.Env, sessionID string)
}

// Availability probe of a provider bound to the env it reads, the shape the registry's safe probe takes
type boundAvailability struct {
	BrowserProvider
	env *tools.Env
}

func (b boundAvailability) IsAvailable() bool { return b.BrowserProvider.IsAvailable(b.env) }
