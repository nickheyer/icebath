package slash

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/sessions"
	sessionsconfig "github.com/nickheyer/icebath/internal/sessions/config"
	"github.com/nickheyer/icebath/internal/slash"
)

type commands struct {
	config   *config.Config
	sessions *sessions.Sessions
}

func Register(registry *slash.Slash, cfg *config.Config, s *sessions.Sessions) {
	registry.Register(commands{config: cfg, sessions: s})
}

func (c commands) Commands() []slash.Command {
	return []slash.Command{
		{Name: "autoprune", Usage: "[on|off|later|status]", Help: "Turn automatic pruning of old sessions on or off", Run: c.autoprune},
		{Name: "discard", Usage: "[admission]", Help: "Acknowledge turns lost when icebath stopped mid-turn, so the session runs again", Run: c.discard},
	}
}

func (c commands) autoprune(ctx context.Context, call *slash.Call) (string, error) {
	store, err := c.sessions.Current()
	if err != nil {
		return "", err
	}
	set := func(on bool, done string) (string, error) {
		if err := c.config.Set(sessionsconfig.AutoPrune, on); err != nil {
			slog.Warn("Could not save sessions.auto_prune", "error", err)
			return "Could not save sessions.auto_prune; check disk space and config.yaml permissions.", nil
		}
		sessions.Unsnooze(c.config)
		if c.config.GetBool(sessionsconfig.AutoPrune) != on {
			return "Could not save sessions.auto_prune; check disk space and config.yaml permissions.", nil
		}
		return done, nil
	}
	switch strings.ToLower(strings.TrimSpace(call.Argument)) {
	case "on", "enable", "yes":
		return set(true, "Auto-prune is on: ended sessions inactive past the keep time are deleted once a day.")
	case "off", "disable", "no":
		return set(false, "Auto-prune is off and you will not be asked again. `icebath sessions prune` still works any time.")
	case "later", "snooze":
		if err := sessions.Snooze(c.config); err != nil {
			return "", err
		}
		return "OK, I'll ask again in a week if the disk is still low.", nil
	}
	on, source := sessions.AutoPruneState(c.config)
	state := "off"
	if on {
		state = "on"
	}
	days := c.config.GetInt(sessionsconfig.RetentionDays)
	if days < 0 {
		days = 90
	}
	return fmt.Sprintf("Auto-prune is %s (%s), retention %d days.%s Usage: /autoprune on | off | later", state, source, days, sessions.DiskLine(ctx, store)), nil
}

func (c commands) discard(ctx context.Context, call *slash.Call) (string, error) {
	store, err := c.sessions.Current()
	if err != nil {
		return "", err
	}
	var ids []string
	if admission := strings.TrimSpace(call.Argument); admission != "" {
		ids = strings.Fields(admission)
	}
	discarded, err := store.Discard(ctx, call.SessionID, ids)
	if err != nil {
		return "", err
	}
	if len(discarded) == 0 {
		return "Nothing to discard: this session has no turn lost when icebath stopped.", nil
	}
	return fmt.Sprintf("Discarded %s. The session runs again; the lost message was not sent again.", strings.Join(discarded, ", ")), nil
}
