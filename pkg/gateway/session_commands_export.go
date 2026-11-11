package gateway

import (
	"context"

	"github.com/nickheyer/icebath/pkg/state"
)

// Stored row id a TUI read renders for the session, its local receipt's entry winning
func TUIStoredID(ctx context.Context, a *SessionAuthority, ref SessionRef) string {
	return storedID(ctx, a, ref)
}

// Compression tip the session's model context lives in
func TUIContextID(ctx context.Context, a *SessionAuthority, ref SessionRef) (string, error) {
	return contextID(ctx, a, ref)
}

// Active rows of the tip as live replay loads them without session_meta bookkeeping
func TUIModelHistory(ctx context.Context, db *state.SessionDB, tip string) ([]map[string]any, error) {
	return modelHistory(ctx, db, tip)
}

// Display rows of a stored history as viewers render it
func DisplayProjection(ctx context.Context, history []map[string]any, profileHome string) ([]any, error) {
	return displayProjection(ctx, history, profileHome)
}
