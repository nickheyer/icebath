package slash

import (
	"context"
	"errors"
	"strings"

	"github.com/nickheyer/icebath/internal/config"
	ironterminal "github.com/nickheyer/icebath/internal/plugins/defaults/egress/ironproxy/terminal"
	"github.com/nickheyer/icebath/internal/sessions"
	"github.com/nickheyer/icebath/internal/slash"
)

type commands struct {
	config   *config.Config
	egress   *ironterminal.Egress
	sessions *sessions.Registry
}

func Register(registry *slash.Registry, cfg *config.Config, egress *ironterminal.Egress, stores *sessions.Registry) {
	registry.Register(commands{config: cfg, egress: egress, sessions: stores})
}

func (c commands) Commands() []slash.Command {
	return []slash.Command{{Name: "egress", Usage: "[status]", Help: "Show the state of the sandbox proxy", Run: c.status}}
}

func (c commands) status(ctx context.Context, call *slash.Call) (string, error) {
	if argument := strings.ToLower(strings.TrimSpace(call.Argument)); argument != "" && argument != "status" {
		return "", errors.New("/egress takes nothing, or the word status.\nUsage: /egress [status]")
	}
	cfg, err := c.sessions.Profile(ctx, c.config, call.SessionID)
	if err != nil {
		return "", err
	}
	return c.egress.Text(ctx, cfg, false), nil
}
