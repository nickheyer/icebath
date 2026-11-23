package tui

import (
	tea "charm.land/bubbletea/v2"
	"github.com/google/uuid"

	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Deletes a stored session, by the revision fenced session.mutate on the background service, handing on why it failed
func (a *app) deleteStoredSession(id string, done func(err error) tea.Cmd) tea.Cmd {
	return a.mutateStoredSession(id, "delete", map[string]any{}, done)
}

// Changes the record of any session of the background service, open in a card or not, fenced by the revision read without attaching to it
func (a *app) mutateStoredSession(id, operation string, payload map[string]any, done func(err error) tea.Cmd) tea.Cmd {
	return call(a.gw, tuigateway.CanonicalSessionRevision, tuigateway.SessionRevisionParams{SessionID: id}, func(rev *tuigateway.SessionRevisionResult, err error) tea.Cmd {
		if err != nil {
			return done(err)
		}
		params := tuigateway.SessionMutateParams{SessionID: firstText(rev.SessionID, id), RequestID: uuid.NewString(), ExpectedRevision: rev.Revision,
			Operation: operation, Payload: payload}
		if operation != "rename" {
			params.ExpectedGeneration = ptr(rev.ExecutionGeneration)
		}
		return call(a.gw, tuigateway.CanonicalSessionMutate, params, func(_ *tuigateway.SessionMutateResult, err error) tea.Cmd { return done(err) })
	})
}
