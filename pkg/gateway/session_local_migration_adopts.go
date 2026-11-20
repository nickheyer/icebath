package gateway

import (
	"context"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

// Whether a resume adopts a phone app's self labelled chat as a writable conversation for its native owner
func adoptsClientHistory(ctx context.Context, a *SessionAuthority, actor Principal, row *pyjson.Object) (bool, error) {
	id, _ := row.GetOr("id", "").(string)
	live, held := a.live(id)
	a.mu.Lock()
	legacy, known := a.nativeLegacyTransports[actor.TransportID]
	a.mu.Unlock()
	if !state.ClientHistoryRow(row) || (held && !isHistoryMount(live)) || !known || !samePrincipal(legacy, actor) {
		return false, nil
	}
	if err := a.DB.ValidateLegacyRow(row, actor.Subject); err != nil {
		if runtimeStoreErrorOf(err) != nil {
			return false, nil
		}
		return false, err
	}
	return true, nil
}
