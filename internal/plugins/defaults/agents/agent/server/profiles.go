package server

import (
	"fmt"

	"github.com/nickheyer/icebath/internal/config"
)

// Why a profile can't start on a model of the provider named, empty when it can: the provider is none the profile has, nor a kind a plugin
// sets up
func (a *Agent) Pinnable(settings *config.Config, name string) string {
	if _, ok := a.provider(settings, name); ok {
		return ""
	}
	return fmt.Sprintf("the profile has no provider named %s and no plugin sets one up; add it in /providers, then pick the model with /model", name)
}

// A reply being written on the profile holds its folder in place until it ends
func (a *Agent) Holds(profile string) string {
	a.mu.Lock()
	runs := make([]*run, 0, len(a.runs))
	for _, r := range a.runs {
		runs = append(runs, r)
	}
	a.mu.Unlock()
	for _, r := range runs {
		r.mu.Lock()
		writing := r.profile == profile && r.busy
		r.mu.Unlock()
		if writing {
			return fmt.Sprintf("A reply is being written on profile %s. Wait for it to finish, or stop it, before renaming or deleting the profile.", profile)
		}
	}
	return ""
}

// Runs of a renamed profile's sessions go on under its new name, as the store moved the sessions there
func (a *Agent) Rename(old, renamed string) error {
	a.mu.Lock()
	runs := make([]*run, 0, len(a.runs))
	for _, r := range a.runs {
		runs = append(runs, r)
	}
	a.mu.Unlock()
	for _, r := range runs {
		r.mu.Lock()
		moved := r.profile == old
		if moved {
			r.profile = renamed
		}
		r.mu.Unlock()
		if moved {
			a.changed(r)
		}
	}
	return nil
}
