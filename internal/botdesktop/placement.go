package botdesktop

import (
	"context"
	"errors"
	"strings"

	bdconfig "github.com/nickheyer/icebath/internal/botdesktop/config"
	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/terminal"
	terminalconfig "github.com/nickheyer/icebath/internal/terminal/config"
)

// Where a screen runs
const (
	// On the machine icebath runs on
	Gateway = "gateway"
	// Inside the sandbox of the terminal backend
	Terminal = "terminal"
	// Nowhere: the backend's sandbox can't host it and the screen may not leave it
	Refused = "refused"
)

// Where the profile's screen runs, the terminal backend it judged by, and the sentence a refusal says
type Placement struct {
	Where, Backend, Reason string
}

// Configured placement, auto for anything else
func placement(cfg *config.Config) string {
	switch value := strings.ToLower(strings.TrimSpace(cfg.GetString(bdconfig.Placement))); value {
	case bdconfig.Terminal, bdconfig.Gateway:
		return value
	}
	return bdconfig.Auto
}

// Where the profile's screen runs, from its settings and the kind of its terminal backend alone, never starting a sandbox: on this
// machine for a local backend or when the settings say so, in the sandbox of a backend that can host it, else refused
func (d *Desktop) Resolve(cfg *config.Config) Placement {
	name := strings.ToLower(strings.TrimSpace(cfg.GetString(terminalconfig.Backend)))
	if name == "" || name == "local" {
		return Placement{Gateway, "local", "terminal.backend is local, so the terminal IS the gateway host"}
	}
	if placement(cfg) == bdconfig.Gateway {
		return Placement{Where: Gateway, Backend: name}
	}
	if backend, ok := d.terminal.Named(cfg, name); ok && backend.Streams {
		return Placement{Where: Terminal, Backend: name}
	}
	return Placement{Refused, name, "terminal.backend is " + name + ", which cannot host a screen yet, and running the desktop on the gateway " +
		"host would put the agent's screen, browser and computer_use outside the sandbox you chose for it. Set bot_desktop.placement: gateway " +
		"to allow that explicitly."}
}

// Whether the profile's screen lives in the terminal backend's sandbox
func (d *Desktop) InSandbox(cfg *config.Config) bool {
	return d.Resolve(cfg).Where == Terminal
}

// Runs work on the environment of the profile's terminal backend the screen lives in, made first when create says so; nil environment
// when there is none and none was made
func (d *Desktop) sandbox(ctx context.Context, cfg *config.Config, create bool, work func(env terminal.Environment) error) error {
	lease := d.terminal.Held(cfg, "", "")
	if lease == nil && create {
		var err error
		if lease, err = d.terminal.Acquire(ctx, cfg, "", ""); err != nil {
			return err
		}
	}
	if lease == nil {
		return work(nil)
	}
	defer lease.Release()
	return work(lease.Env)
}

// Refusal of a sandbox start when the backend made no environment
var errNoSandbox = errors.New("the terminal backend's sandbox could not be started, so there is nowhere to put the screen")
