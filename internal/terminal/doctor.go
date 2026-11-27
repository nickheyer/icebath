package terminal

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/internal/config"
	terminalconfig "github.com/nickheyer/icebath/internal/terminal/config"
)

// One backend as the backends screen shows it to a profile
type Survey struct {
	Backend Backend
	// The profile's commands run on it
	Selected bool
	// Why it can't run with the profile's settings, empty when it can
	Unavailable string
	Checks      []Check
	// What the user does to make it usable
	Setup []string
}

// Every backend as the profile sees it, the one it picked first
func (t *Terminal) Survey(cfg *config.Config) []Survey {
	picked := strings.ToLower(strings.TrimSpace(cfg.GetString(terminalconfig.Backend)))
	var reports []Survey
	for _, backend := range t.Backends() {
		report := Survey{Backend: backend, Selected: backend.Name == picked}
		if backend.Check != nil {
			if err := backend.Check(cfg); err != nil {
				report.Unavailable = err.Error()
			}
		}
		if backend.Doctor != nil {
			report.Checks = backend.Doctor(cfg)
		}
		if backend.Setup != nil {
			report.Setup = backend.Setup(cfg)
		}
		reports = append(reports, report)
	}
	slices.SortStableFunc(reports, func(a, b Survey) int {
		switch {
		case a.Selected == b.Selected:
			return 0
		case a.Selected:
			return -1
		}
		return 1
	})
	return reports
}

// Readies the backend of the name for the profile ahead of its first command, saying how it goes line by line
func (t *Terminal) Prepare(ctx context.Context, cfg *config.Config, name string, say func(line string)) error {
	backend, ok := t.Backend(name)
	if !ok {
		return fmt.Errorf("There is no terminal backend named '%s'.", name)
	}
	say("Terminal backend: " + backend.Title)
	if backend.Prepare == nil {
		say("Nothing to set up: commands run as soon as the backend is picked.")
		return nil
	}
	return backend.Prepare(ctx, cfg, say)
}
