package server

import (
	"context"
	"log/slog"

	"github.com/nickheyer/icebath/internal/botdesktop"
	"github.com/nickheyer/icebath/internal/config"
)

// Tells the sessions of a profile held here of a change of the profile's Bot Desktop, the newest line going with each one's next message
type screens struct {
	agent   *Agent
	desktop *botdesktop.Registry
}

func (s screens) Changed(cfg *config.Config, event botdesktop.Event) {
	switch event.Kind {
	case botdesktop.Runtime:
	case botdesktop.Leased:
		// A change of hands carries only the lease, so the screen is read as it is now
		host := s.desktop.Host()
		if host == nil {
			return
		}
		published, err := host.Published(context.Background(), cfg)
		if err != nil {
			slog.Debug("Bot Desktop status unreadable for the turn note", "profile", cfg.Profile(), "error", err)
			return
		}
		event.Running, event.Display = published["DISPLAY"] != "", published["DISPLAY"]
	default:
		return
	}
	note := botdesktop.Note(event.Running, event.Display, event.Lease.Holder)
	if note == "" {
		return
	}
	for _, r := range s.agent.held() {
		r.mu.Lock()
		if r.profile == cfg.Profile() {
			r.screen = note
		}
		r.mu.Unlock()
	}
}

// Takes the newest line on the profile's Bot Desktop for the message starting a turn, so it is told once
func (r *run) screenNote() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	note := r.screen
	r.screen = ""
	return note
}
