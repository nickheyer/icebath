package gateway

import (
	"context"
	"database/sql"
	"errors"
	"maps"
	"strings"

	"github.com/nickheyer/icebath/pkg/state"
)

// True when two principals carry the same subject, profile, transport and capability set
func samePrincipal(a, b Principal) bool {
	return a.Subject == b.Subject && a.ProfileID == b.ProfileID && a.TransportID == b.TransportID &&
		maps.Equal(a.Capabilities, b.Capabilities)
}

// True for the importer an import binding names, else for an operator or the profile owner's native peer
func historyReader(ctx context.Context, a *SessionAuthority, actor Principal, sid string) (bool, error) {
	var binding any
	err := a.DB.ReadCtx(ctx, func(conn state.SQLiteConn) error {
		var err error
		binding, err = state.HistoryBinding(ctx, conn, sid)
		return err
	})
	if err != nil {
		return false, err
	}
	if binding != nil {
		return state.BindingNames(binding, actor.ProfileID, actor.Subject), nil
	}
	if actor.Capabilities["session:operator"] {
		return true, nil
	}
	a.mu.Lock()
	native, held := a.nativeLegacyTransports[actor.TransportID]
	a.mu.Unlock()
	return held && samePrincipal(native, actor), nil
}

// Mounts a stored session read only for its owner, false when another loader owns it
func mountHistory(ctx context.Context, a *SessionAuthority, actor Principal, sid string) (bool, error) {
	row, err := a.DB.GetSession(ctx, sid)
	if err != nil {
		return false, err
	}
	if row == nil {
		return false, &PyError{Kind: "TypeError", Msg: "'NoneType' object is not subscriptable"}
	}
	label, has := row.Get("source")
	if !has {
		return false, &pyKeyError{key: "source"}
	}
	if state.LegacyLocalSource(label) || label == any("local") {
		return false, nil
	}
	own := a.DB.OwnProfileName()
	if own == "" {
		own = "default"
	}
	if name, _ := row.Get("profile_name"); name != nil && name != any("") && name != any(own) {
		return false, nil
	}
	reads, err := historyReader(ctx, a, actor, sid)
	if err != nil || !reads {
		return false, err
	}
	bound := false
	err = a.DB.ReadCtx(ctx, func(conn state.SQLiteConn) error {
		var one int
		err := conn.QueryRowContext(ctx, "SELECT 1 FROM state_meta WHERE key=?", state.LocalAPIBindingPrefix+sid).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		bound = err == nil
		return err
	})
	if err != nil || bound {
		return false, err
	}
	if err := a.requireAdmissionOpen(false); err != nil {
		return false, err
	}
	a.mu.Lock()
	if _, held := a.Sessions[sid]; !held {
		a.Sessions[sid] = NewLiveSession(nil, historyMountRoutePrefix+sid)
	}
	a.mu.Unlock()
	return true, nil
}

// Refuses a delete of a history mount whose compression chain has child sessions hanging off it
func requireHistoryLeaf(ctx context.Context, a *SessionAuthority, sid string) error {
	live, ok := a.live(sid)
	if !ok {
		return &pyKeyError{key: sid}
	}
	if !isHistoryMount(live) {
		return nil
	}
	lineage, err := a.DB.GetCompressionLineage(ctx, sid)
	if err != nil {
		return err
	}
	chain := map[string]bool{}
	for _, id := range lineage {
		chain[id] = true
	}
	return a.DB.ReadCtx(ctx, func(conn state.SQLiteConn) error {
		targets, err := state.DeleteTargets(ctx, conn, sid)
		if err != nil {
			return err
		}
		args := make([]any, len(targets))
		for i, id := range targets {
			args[i] = id
		}
		marks := strings.TrimSuffix(strings.Repeat("?,", len(targets)), ",")
		rows, err := conn.QueryContext(ctx, "SELECT id FROM sessions WHERE parent_session_id IN ("+marks+")", args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		named := append([]string{}, targets...)
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			named = append(named, id)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		for _, id := range named {
			if !chain[id] {
				field, detail := "session_id", "this stored conversation has child sessions a delete would take with it"
				return state.NewRuntimeStoreError("session_has_children", &field, &detail)
			}
		}
		return nil
	})
}

// Refusal of every verb but a read on a stored conversation nothing can run
func readOnly() error {
	field, detail := "session_id", "this stored conversation can be read but not continued; start a new chat"
	return state.NewRuntimeStoreError("session_read_only", &field, &detail)
}

// Refuses a history mount to anyone but its reader and every capability but reading
func requireHistoryAccess(ctx context.Context, a *SessionAuthority, actor Principal, sid, capability string) error {
	reads, err := historyReader(ctx, a, actor, sid)
	if err != nil {
		return err
	}
	if !reads {
		return runtimeRefusal("not_found")
	}
	if capability != "session:read" {
		return readOnly()
	}
	return nil
}
