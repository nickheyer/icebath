package tui

import (
	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Deletes a stored session, by the revision fenced session.mutate on the shared gateway, a refusal handing back the read's attachment
func (a *app) deleteStoredSession(id string, deleted func() tea.Cmd) tea.Cmd {
	if !a.gw.canonical() {
		return call(a.gw, tuigateway.SessionDelete, tuigateway.SessionDeleteParams{SessionID: id}, func(result *tuigateway.SessionDeleteResult, err error) tea.Cmd {
			if err != nil {
				a.sys("error: " + rpcErrorMessage(err))
				return nil
			}
			if result.Deleted != id {
				a.sys("error: " + tr("session.common.invalidResponse", "session.delete"))
				return nil
			}
			return deleted()
		})
	}
	var release *tuigateway.SessionDetachParams
	onRead := func(snapshot *tuigateway.CanonicalSnapshot) {
		resolved := firstText(snapshot.SessionID, id)
		release = nil
		if snapshot.SubscriptionID != "" && resolved != a.store.ui.Sid {
			release = &tuigateway.SessionDetachParams{SessionID: resolved, SubscriptionID: snapshot.SubscriptionID}
		}
	}
	return a.store.mutateCanonicalRead(a.gw, id, "delete", map[string]any{}, func() bool { return false }, onRead, func(m *mutationOutcome, err error) tea.Cmd {
		if err != nil {
			a.sys("error: " + rpcErrorMessage(err))
			if release != nil {
				return a.events.quietRequest(tuigateway.CanonicalSessionDetach.Name(), wireParams(*release))
			}
			return nil
		}
		if m == nil || m.Result == nil {
			a.sys("error: " + tr("session.common.invalidResponse", "session.mutate"))
			return nil
		}
		return deleted()
	})
}
