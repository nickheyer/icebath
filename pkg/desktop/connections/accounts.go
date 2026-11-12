package connections

import (
	"os"
	"runtime"
)

// Accounts serves the profile, sign in, cloud, api, MCP OAuth and machine facts preload functions
type Accounts struct {
	remoteDisplayReason string
}

// NewAccounts builds the service with the remote display decision fixed for the process lifetime
func NewAccounts() *Accounts {
	return &Accounts{remoteDisplayReason: DetectRemoteDisplay(os.Getenv, machinePlatform(runtime.GOOS))}
}

// GetMachineProfile answers getMachineProfile
func (a *Accounts) GetMachineProfile() MachineProfile { return ReadMachineProfile() }

// GetRemoteDisplayReason answers getRemoteDisplayReason, nil when the display is local
func (a *Accounts) GetRemoteDisplayReason() *string {
	if a.remoteDisplayReason == "" {
		return nil
	}
	reason := a.remoteDisplayReason
	return &reason
}
