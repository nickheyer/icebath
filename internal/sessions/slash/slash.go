package slash

import (
	"context"
	"errors"
	"fmt"
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
		{Name: "autoprune", Usage: "[on|off|later]", Help: "Delete old sessions when the server starts", Run: c.autoprune},
		{Name: "discard", Usage: "[id...]", Help: "Unblock a session that icebath stopped mid-reply", Run: c.discard},
	}
}

// /autoprune in a chat app: on and off are the shortcut to Delete old sessions, with the note its row gives, and later puts the question about
// a low disk off for a week; alone it says in one line how things stand
func (c commands) autoprune(ctx context.Context, call *slash.Call) (string, error) {
	switch word := strings.ToLower(strings.TrimSpace(call.Argument)); word {
	case "on", "off":
		if err := c.config.Change(sessionsconfig.AutoPrune, word == "on"); err != nil {
			return "", fmt.Errorf("Delete old sessions was not changed. The error was: %s", strings.TrimSuffix(err.Error(), ".")+".")
		}
		return "Delete old sessions is now " + strs.OnOff(c.config.GetBool(sessionsconfig.AutoPrune)) + ". Takes effect after icebath restarts.", nil
	case "later":
		if err := sessions.Snooze(c.config); err != nil {
			return "", err
		}
		return "icebath asks again in a week if the disk is still low.", nil
	case "":
	default:
		return "", errors.New("/autoprune takes on, off or later.")
	}
	store, err := c.sessions.Current()
	if err != nil {
		return "", err
	}
	if !c.config.GetBool(sessionsconfig.AutoPrune) {
		return "Delete old sessions is Off." + sessions.DiskLine(ctx, store), nil
	}
	days := max(c.config.GetInt(sessionsconfig.RetentionDays), 0)
	return fmt.Sprintf("Delete old sessions is On. Each time the server starts, it deletes the sessions unused for %s.%s",
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
