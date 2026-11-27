package server

import (
	"context"
	"errors"
	"slices"
	"strings"

	"connectrpc.com/connect/v2"

	"github.com/nickheyer/icebath/internal/sessions"
	agentv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/agent/v1"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
)

// Sessions the server holds open and what each is doing, by id
func (a *Agent) Live() []*agentv1.LiveSession {
	runs := a.held()
	out := make([]*agentv1.LiveSession, 0, len(runs))
	for _, r := range runs {
		r.mu.Lock()
		watchers := 0
		for _, n := range r.life.clients {
			watchers += n
		}
		out = append(out, &agentv1.LiveSession{SessionId: r.sessionID(), Busy: r.busy, Waiting: len(a.approvals.Pending(r.sessionID())) > 0 || r.questions != nil, Watchers: int32(watchers)})
		r.mu.Unlock()
	}
	slices.SortFunc(out, func(x, y *agentv1.LiveSession) int { return strings.Compare(x.GetSessionId(), y.GetSessionId()) })
	return out
}

// Closes a session the server holds open, stopping its turn; it stays stored, ended as the person closed it
func (a *Agent) Shut(id string) error {
	a.mu.Lock()
	r, open := a.runs[id]
	a.mu.Unlock()
	if !open {
		return connect.NewError(connect.CodeNotFound, "That session is not open, so there is nothing to close.")
	}
	r.mu.Lock()
	watched := 0
	for _, n := range r.life.clients {
		watched += n
	}
	if watched > 0 {
		r.mu.Unlock()
		return connect.NewError(connect.CodeFailedPrecondition, "That session is open in a window. Close the window to close the session.")
	}
	if r.stop != nil {
		r.stop()
	}
	r.mu.Unlock()
	a.close(r, sessions.EndUserClosed, 0)
	return nil
}

// Takes back a message the user sent and everything after it, then sends the text in its place, the model told when the person cut a
// spoken reply off first; how many messages were taken back
func (a *Agent) Edit(ctx context.Context, id string, message int64, text string, interrupted bool) (int, error) {
	r, err := a.run(id)
	if err != nil {
		return 0, err
	}
	store, err := a.store()
	if err != nil {
		return 0, err
	}
	r.mu.Lock()
	if r.busy {
		r.mu.Unlock()
		return 0, errors.New("A reply is still running. Stop it first, then change your message.")
	}
	sent, err := a.sent(r)
	if err != nil {
		r.mu.Unlock()
		return 0, err
	}
	at := slices.IndexFunc(sent, func(m *sessionsv1.Message) bool { return m.GetId() == message })
	if at < 0 {
		r.mu.Unlock()
		return 0, connect.NewError(connect.CodeFailedPrecondition, "That message is no longer in the session, so nothing was changed.")
	}
	shown, err := store.Messages(a.ctx, id, sessions.Display)
	if err != nil {
		r.mu.Unlock()
		return 0, err
	}
	removed := 0
	for _, m := range shown {
		if m.GetId() >= message {
			removed++
		}
	}
	err = a.takeBack(r, sent[at])
	r.mu.Unlock()
	if err != nil || strings.TrimSpace(text) == "" {
		return removed, err
	}
	return removed, a.Message(ctx, id, text, false, interrupted)
}
