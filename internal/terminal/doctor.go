package terminal

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/internal/config"
	terminalconfig "github.com/nickheyer/icebath/internal/terminal/config"
)

// States of a place, in the words every page and command says them in
const (
	Ready      = "Ready"
	NeedsSetup = "Needs setup"
	CantRun    = "Can't run"
)

// How a place stands for a profile
type Place struct {
	Backend Backend
	// The profile's commands run there
	Selected bool
	// Ready, Needs setup or Can't run
	State string
	// This computer lacks what the place needs, such as its program, so nothing on its page makes it run
	Missing bool
	// What keeps it from running, a sentence; empty while it is ready
	Why    string
	Checks []Check
}

// Every place as the profile sees it, in the order they were registered
func (t *Terminal) Places(cfg *config.Config) []Place {
	var places []Place
	for _, backend := range t.Backends() {
		places = append(places, t.Place(cfg, backend))
	}
	return places
}

// How the place stands for the profile: Can't run while this computer lacks what it needs, Needs setup while a setting or a check keeps it
// from running, else Ready; why, and what was checked
func (t *Terminal) Place(cfg *config.Config, backend Backend) Place {
	place := Place{Backend: backend, Selected: backend.Name == t.Chosen(cfg), State: Ready}
	if backend.Doctor != nil {
		place.Checks = backend.Doctor(cfg)
	}
	var err error
	if backend.Check != nil {
		err = backend.Check(cfg)
	}
	// A failed check keeps the place from being ready whether or not it says more than its state
	failed := false
	for _, check := range place.Checks {
		if check.OK {
			continue
		}
		if !failed {
			place.Why = check.Detail
		}
		failed = true
	}
	switch _, missing := errors.AsType[*MissingError](err); {
	case missing:
		place.State, place.Missing = CantRun, true
	case err != nil || failed:
		place.State = NeedsSetup
	}
	if place.Why == "" && err != nil {
		place.Why = err.Error()
	}
	if place.Why == "" && failed {
		for _, check := range place.Checks {
			if !check.OK {
				place.Why = cmp.Or(check.Fix, check.Label+": "+check.State)
				break
			}
		}
	}
	return place
}

// Name of the place the profile's commands run on: what an environment variable names over the settings, else the settings
func (t *Terminal) Chosen(cfg *config.Config) string {
	return strings.ToLower(strings.TrimSpace(cfg.GetString(terminalconfig.Backend)))
}

// Environment variable that names the place over the settings, empty when none does
func (t *Terminal) Holder(cfg *config.Config) string {
	_, setting, ok := cfg.Setting(terminalconfig.Backend)
	if !ok {
		return ""
	}
	if source, variable := cfg.Source(setting); source == config.FromEnvironment {
		return variable
	}
	return ""
}

// Runs the profile's commands on the place of the name from the next one on, answering what changed; held when an environment variable
// names another place, which the commands still run on
func (t *Terminal) Use(cfg *config.Config, name string) (string, bool, error) {
	backend, ok := t.Backend(name)
	if !ok {
		return "", false, fmt.Errorf("icebath has no place to run commands named %q. The places are %s.", name, t.titles())
	}
	if err := cfg.Set(terminalconfig.Backend, backend.Name); err != nil {
		return "", false, err
	}
	if variable := t.Holder(cfg); variable != "" && t.Chosen(cfg) != backend.Name {
		named := t.Chosen(cfg)
		if other, ok := t.Backend(named); ok {
			named = other.Title
		}
		return backend.Title + " is chosen, but " + variable + " names " + named + ", which wins while it is set.", true, nil
	}
	if place := t.Place(cfg, backend); place.State != Ready {
		return backend.Title + " now runs the agent's commands, but it can't run yet.", false, nil
	}
	return backend.Title + " now runs the agent's commands.", false, nil
}

// Readies the backend of the name for the profile ahead of its first command, saying how it goes line by line
func (t *Terminal) Prepare(ctx context.Context, cfg *config.Config, name string, say func(line string)) error {
	backend, ok := t.Backend(name)
	if !ok {
		return fmt.Errorf("icebath has no place to run commands named %q. The places are %s.", name, t.titles())
	}
	if backend.Prepare == nil {
		say(backend.Title + " needs nothing set up first. Commands run there as soon as you choose it.")
		return nil
	}
	return backend.Prepare(ctx, cfg, say)
}
