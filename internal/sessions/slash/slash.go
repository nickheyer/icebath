package slash

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/sessions"
	sessionsconfig "github.com/nickheyer/icebath/internal/sessions/config"
	"github.com/nickheyer/icebath/internal/slash"
	"github.com/nickheyer/icebath/pkg/strs"
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
		{Name: "discard", Usage: "[id...]", Help: "Unblock a session that icebath stopped mid-reply", Run: c.discard},
	}
}

func (c commands) autoprune(ctx context.Context, call *slash.Call) (string, error) {
	store, err := c.sessions.Current()
	if err != nil {
		return "", err
	}
	const unsaved = "Delete old sessions could not be saved. Check that the disk has space and that you may write the settings file."
	set := func(on bool, done string) (string, error) {
		if err := c.config.Set(sessionsconfig.AutoPrune, on); err != nil {
			slog.Warn("Could not save sessions.auto_prune", "error", err)
			return unsaved, nil
		}
		sessions.Unsnooze(c.config)
		if c.config.GetBool(sessionsconfig.AutoPrune) != on {
			return unsaved, nil
		}
		return done, nil
	}
	switch strings.ToLower(strings.TrimSpace(call.Argument)) {
	case "on", "enable", "yes":
		return set(true, "Delete old sessions is now On. Once a day, icebath deletes the ended sessions unused for longer than Delete after in "+
			"/settings under Sessions.")
	case "off", "disable", "no":
		return set(false, "Delete old sessions is now Off, and icebath will not ask again. You can still run icebath sessions prune at any time.")
	case "later", "snooze":
		if err := sessions.Snooze(c.config); err != nil {
			return "", err
		}
		return "icebath asks again in a week if the disk is still low.", nil
	}
	on, source := sessions.AutoPruneState(c.config)
	state := "Off"
	if on {
		state = "On"
	}
	from := map[string]string{"default": "as it is by default", "your setting": "as you set it", "your environment": "as your environment sets it"}[source]
	days := c.config.GetInt(sessionsconfig.RetentionDays)
	if days < 0 {
		days = 90
	}
	return fmt.Sprintf("Delete old sessions is %s, %s, for sessions unused for %s.%s\nUsage: /autoprune [on|off|later|status]", state, from,
		strs.Span(time.Duration(days)*24*time.Hour), sessions.DiskLine(ctx, store)), nil
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
		return "This session has no message that icebath lost when it stopped, so there is nothing to discard.", nil
	}
	return fmt.Sprintf("Discarded %s. The session works again, and the lost message was not sent again.", strings.Join(discarded, ", ")), nil
}
