package tui

import tea "charm.land/bubbletea/v2"

// Deletes a stored session, by the revision fenced session.mutate on the shared gateway, a refusal handing back the read's attachment
func (a *app) deleteStoredSession(id string, deleted func() tea.Cmd) tea.Cmd {
	if !a.gw.canonical() {
		return gatewayCall(a.gw, "session.delete", map[string]any{"session_id": id}, func(v any, err error) tea.Cmd {
			if err != nil {
				a.sys("error: " + rpcErrorMessage(err))
				return nil
			}
			if result := asRPCResult(v); result == nil || result["deleted"] != id {
				a.sys("error: " + tr("session.common.invalidResponse", "session.delete"))
				return nil
			}
			return deleted()
		})
	}
	var release map[string]any
	onRead := func(snapshot map[string]any) {
		resolved, _ := snapshot["session_id"].(string)
		if resolved == "" {
			resolved = id
		}
		subscription, _ := snapshot["subscription_id"].(string)
		release = nil
		if subscription != "" && resolved != a.store.ui.Sid {
			release = map[string]any{"session_id": resolved, "subscription_id": subscription}
		}
	}
	return a.store.mutateCanonicalRead(a.gw, id, "delete", map[string]any{}, func() bool { return false }, onRead, func(m *mutationOutcome, err error) tea.Cmd {
		if err != nil {
			a.sys("error: " + rpcErrorMessage(err))
			if release != nil {
				return a.events.quietRequest("session.detach", release)
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
