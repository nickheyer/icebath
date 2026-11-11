package tuiserver

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Records the newest model switch pivot in the live history and the session store, dropping older markers
func appendModelSwitchMarker(ctx context.Context, session *tuigateway.Session, model, providerName string) {
	if session == nil {
		return
	}
	sessionKey := strings.TrimSpace(strOr(session.Get("session_key")))
	if sessionKey == "" {
		return
	}
	providerPart := ""
	if providerName != "" {
		providerPart = " via provider " + providerName
	}
	marker := fmt.Sprintf("%s%s%s. From this point forward, use this runtime metadata when answering questions about what model/provider is active.]",
		modelSwitchMarkerPrefix, model, providerPart)
	entry := map[string]any{"role": "user", "content": marker, "display_kind": "model_switch"}
	func() {
		unlock := lockHistory(session)
		defer unlock()
		history, _ := session.SetDefault("history", []any{}).([]any)
		history = slices.DeleteFunc(slices.Clone(history), isModelSwitchMarker)
		session.Set("history", append(history, entry))
		session.Set("history_version", int(intOr(session.Get("history_version")))+1)
	}()
	failure := catchPanic(func() {
		var db *state.DB
		if a := sessionAgent(session); a != nil {
			db = a.SessionDB()
		}
		release := func() {}
		if db == nil {
			ensureSessionDBRow(ctx, session)
			db, release = sessionDB(ctx, session)
		}
		defer release()
		if db == nil {
			return
		}
		target := submitRowTargetKey(session)
		if _, err := db.DeactivateMessagesByDisplayKind(ctx, target, "model_switch"); err != nil {
			panic(err)
		}
		rowID, err := db.AppendMessage(ctx, target, map[string]any{"role": "user", "content": marker, "display_kind": "model_switch",
			"message_uid": state.StampMessageUID(entry)}, state.AppendGuards{})
		if err != nil {
			panic(err)
		}
		entry["_row_id"] = rowID
		entry[state.DBPersistedMarker] = true
	})
	if failure != nil {
		logger().Warn("failed to persist model switch marker", "error", failure)
	}
}
