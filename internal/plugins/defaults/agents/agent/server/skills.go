package server

import (
	"context"
	"strings"

	"connectrpc.com/connect/v2"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/skills"
)

// The agent as the skills host sees it: what runs sessions, with no skills of its own to bring
type runner struct {
	*Agent
}

func (runner) Sources(*config.Config) []skills.Source { return nil }

// Sends the text to the model as the user's next message of the session, queued behind a reply in progress; display is what the user typed,
// which the session's title and transcript show in place of the text
func (a *Agent) Submit(ctx context.Context, id, text, display string) error {
	r, err := a.run(id)
	if err != nil {
		return err
	}
	if text = strings.TrimSpace(text); text == "" {
		return connect.NewError(connect.CodeInvalidArgument, "Your message is empty. Type something to send.")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := a.blocked(ctx, r); err != nil {
		return err
	}
	a.admit(r, text)
	r.queue = append(r.queue, queued{text: text, display: strings.TrimSpace(display)})
	if r.busy {
		r.yield.Store(true)
		a.notify(r, queuedNote)
		return nil
	}
	return a.start(r)
}
