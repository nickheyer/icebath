package gateway

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

// Local surfaces a legacy session row can carry as its source or created source
var legacyLocalSources = map[string]bool{"cli": true, "tui": true, "gui": true, "desktop": true, "oneshot": true, "tool": true, "acp": true}

// Python str of a row field, empty for a missing or falsy value
func botRowText(row *pyjson.Object, key string) string {
	if row == nil {
		return ""
	}
	value, _ := row.Get(key)
	if !pyTruthy(value) {
		return ""
	}
	return pyStrOf(value)
}

// First id of the compression lineage of sessionID, sessionID itself when the lineage is empty
func botLineageRoot(ctx context.Context, db SessionStoreDB, sessionID string) (string, error) {
	lineage, err := db.GetCompressionLineage(ctx, sessionID)
	if err != nil {
		return "", err
	}
	if len(lineage) == 0 {
		return sessionID, nil
	}
	return lineage[0], nil
}

// Moves a handed off Bot Chat aside once the lane on its route switched to another conversation
func releaseSupersededBotChat(ctx context.Context, db SessionStoreDB, store *SessionStore, route, sessionID string) error {
	root, err := botLineageRoot(ctx, db, sessionID)
	if err != nil {
		return err
	}
	row, err := db.GetSession(ctx, root)
	if err != nil {
		return err
	}
	if row == nil {
		return nil
	}
	title, _ := row.Get("title")
	if title != "Bot Chat" || !legacyLocalSources[botRowCreatedSource(row)] {
		return nil
	}
	source, _ := row.Get("source")
	if _, ok := messagingPlatform(ctx, source); !ok {
		return nil
	}
	store.mu.Lock()
	entry := store.entryLocked(ctx, route)
	store.mu.Unlock()
	if entry == nil {
		return nil
	}
	held, err := botLineageRoot(ctx, db, entry.SessionID)
	if err != nil {
		return err
	}
	if held == root {
		return nil
	}
	moved, err := displaceBotChat(ctx, db, root)
	if err != nil {
		return err
	}
	if moved {
		slog.Info(fmt.Sprintf("Bot Chat %s was handed off to %s and that lane moved on: moved it aside", root, route))
	}
	return nil
}

// Created source of a row with a missing or NULL column reading as no source
func botRowCreatedSource(row *pyjson.Object) string {
	value, _ := row.Get("created_source")
	if value == nil {
		return ""
	}
	return pyStrOf(value)
}

// Boot pass releasing the titled Bot Chat an older build left handed off to a lane that moved on
func releaseSupersededBotChatAtBoot(ctx context.Context, db botChatBootDB, store *SessionStore) error {
	row, err := db.GetSessionByTitle(ctx, "Bot Chat")
	if err != nil || row == nil {
		return err
	}
	rowID := botRowText(row, "id")
	tipID, err := db.GetCompressionTip(ctx, rowID)
	if err != nil {
		return err
	}
	if tipID == "" {
		tipID = rowID
	}
	tip, err := db.GetSession(ctx, tipID)
	if err != nil {
		return err
	}
	if tip == nil {
		tip = row
	}
	return releaseSupersededBotChat(ctx, db, store, botRowText(tip, "session_key"), rowID)
}

// Session database surface of the boot release, the store surface plus the title lookup
type botChatBootDB interface {
	SessionStoreDB
	GetSessionByTitle(ctx context.Context, title string) (*pyjson.Object, error)
}

// Renames the canonical Bot Chat title off sid under a visible label, false when the title moved meanwhile
func displaceBotChat(ctx context.Context, db SessionStoreDB, sid string) (bool, error) {
	row, err := db.GetSession(ctx, sid)
	if err != nil {
		return false, err
	}
	source := botRowText(row, "source")
	label := source
	if source == "" || source == "api_server" || source == "bot_peer_dm" {
		label = "API"
	}
	name := fmt.Sprintf("Bot Chat (%s %s)", label, sid)
	if runes := []rune(name); len(runes) > state.MaxTitleLength {
		name = string(runes[:state.MaxTitleLength-2]) + "\u2026)"
	}
	var changed int64
	err = db.ExecuteWrite(ctx, func(conn state.SQLiteConn) error {
		result, err := conn.ExecContext(ctx, "UPDATE sessions SET title=?, title_source=?, hidden=0 WHERE id=? AND title='Bot Chat'",
			name, state.TitleSourceUser, sid)
		if err != nil {
			return err
		}
		changed, err = result.RowsAffected()
		return err
	})
	if err != nil {
		return false, err
	}
	return changed > 0, nil
}
