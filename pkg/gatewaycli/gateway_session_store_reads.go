package gatewaycli

import (
	"context"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

// Stored session row from the read only reader, nil without a reader or a row
func (s *GatewaySessionStore) GetSession(ctx context.Context, sessionID string) (map[string]any, error) {
	if db := s.Reader(ctx); db != nil {
		return db.GetSession(ctx, sessionID)
	}
	return nil, nil
}

// Title of a session from the reader
func (s *GatewaySessionStore) GetSessionTitle(ctx context.Context, sessionID string) (string, bool, error) {
	if db := s.Reader(ctx); db != nil {
		return db.DB().GetSessionTitle(ctx, sessionID)
	}
	return "", false, nil
}

// Session row holding a title from the reader
func (s *GatewaySessionStore) GetSessionByTitle(ctx context.Context, title string) (*pyjson.Object, error) {
	if db := s.Reader(ctx); db != nil {
		return db.DB().GetSessionByTitle(ctx, title)
	}
	return nil, nil
}

// Resume target of a session id from the reader
func (s *GatewaySessionStore) ResolveResumeSessionID(ctx context.Context, sessionID string) (string, error) {
	if db := s.Reader(ctx); db != nil {
		return db.DB().ResolveResumeSessionID(sessionID)
	}
	return "", nil
}

// Transcript of a session as conversation messages from the reader
func (s *GatewaySessionStore) GetMessagesAsConversation(ctx context.Context, sessionID string, o state.ConversationOptions) ([]map[string]any, error) {
	if db := s.Reader(ctx); db != nil {
		return db.DB().GetMessagesAsConversation(ctx, sessionID, o)
	}
	return nil, nil
}

// Model and display conversations of a resumed session from the reader
func (s *GatewaySessionStore) GetResumeConversations(ctx context.Context, sessionID string) ([]map[string]any, []map[string]any, error) {
	if db := s.Reader(ctx); db != nil {
		return db.DB().GetResumeConversations(ctx, sessionID)
	}
	return nil, nil, nil
}

// Resume safety check of a session through the reader
func (s *GatewaySessionStore) AssertResumeSafe(ctx context.Context, sessionID string, maxMessages *int, tipOnly bool) (int, error) {
	if db := s.Reader(ctx); db != nil {
		return db.DB().AssertResumeSafe(ctx, sessionID, maxMessages, tipOnly)
	}
	return 0, nil
}

// Whether a kanban task owns the session, read from the reader
func (s *GatewaySessionStore) IsKanbanOwnedSession(ctx context.Context, sessionID string) (bool, error) {
	if db := s.Reader(ctx); db != nil {
		return db.DB().IsKanbanOwnedSession(ctx, sessionID)
	}
	return false, nil
}

// Rich session page from the reader with created surface matching
func (s *GatewaySessionStore) ListSessionsRich(ctx context.Context, o state.ListSessionsRichOptions) ([]*pyjson.Object, error) {
	if db := s.Reader(ctx); db != nil {
		return db.ListSessionsRich(ctx, o)
	}
	return nil, nil
}

// Recent sessions from the reader with created surface matching
func (s *GatewaySessionStore) SearchSessions(ctx context.Context, o SearchSessionsOptions) ([]map[string]any, error) {
	if db := s.Reader(ctx); db != nil {
		return db.SearchSessions(ctx, o)
	}
	return nil, nil
}

// Next free numbered title of a lineage from the reader
func (s *GatewaySessionStore) GetNextTitleInLineage(ctx context.Context, baseTitle string) (string, error) {
	if db := s.Reader(ctx); db != nil {
		return db.DB().GetNextTitleInLineage(ctx, baseTitle)
	}
	return "", nil
}

// Handoff state of a session from the reader
func (s *GatewaySessionStore) GetHandoffState(ctx context.Context, sessionID string) *pyjson.Object {
	if db := s.Reader(ctx); db != nil {
		return db.DB().GetHandoffState(ctx, sessionID)
	}
	return nil
}

// Store metadata value from the reader
func (s *GatewaySessionStore) GetMeta(ctx context.Context, key string) (string, bool, error) {
	if db := s.Reader(ctx); db != nil {
		return db.DB().GetMeta(ctx, key)
	}
	return "", false, nil
}

// Live compression tip of a lineage from the reader
func (s *GatewaySessionStore) GetCompressionTip(ctx context.Context, sessionID string) (string, error) {
	if db := s.Reader(ctx); db != nil {
		return db.GetCompressionTip(ctx, sessionID)
	}
	return "", nil
}

// Session id a title resolves to from the reader
func (s *GatewaySessionStore) ResolveSessionByTitle(ctx context.Context, title string) (string, error) {
	if db := s.Reader(ctx); db != nil {
		return db.ResolveSessionByTitle(ctx, title)
	}
	return "", nil
}

// Stored message rows of a session from the reader
func (s *GatewaySessionStore) GetMessages(ctx context.Context, sessionID string, o state.GetMessagesOptions) ([]*pyjson.Object, error) {
	if db := s.Reader(ctx); db != nil {
		return db.DB().GetMessages(ctx, sessionID, o)
	}
	return nil, nil
}

// Session count from the reader with created surface matching
func (s *GatewaySessionStore) SessionCount(ctx context.Context, o state.SessionCountOptions) (int, error) {
	if db := s.Reader(ctx); db != nil {
		return db.SessionCount(ctx, o)
	}
	return 0, nil
}

// Validated title text, the session store rule that needs no database
func (s *GatewaySessionStore) SanitizeTitle(title string) (string, error) {
	return state.SanitizeTitle(title)
}

// Whether session metadata carries the approval bypass, the session store rule that needs no database
func (s *GatewaySessionStore) SessionYoloEnabled(sessionMeta map[string]any) bool {
	return state.SessionYoloEnabled(sessionMeta)
}

// One model config value of a session from the reader
func (s *GatewaySessionStore) GetSessionModelConfigValue(ctx context.Context, sessionID, key string, def any) (any, error) {
	if db := s.Reader(ctx); db != nil {
		return db.DB().GetSessionModelConfigValue(ctx, sessionID, key, def)
	}
	return nil, nil
}

// Row creation the gateway performs itself
func (s *GatewaySessionStore) CreateSession(args ...any) { noop(args...) }

// Row reopening the gateway performs itself
func (s *GatewaySessionStore) ReopenSession(args ...any) { noop(args...) }

// Transcript flush the gateway performs itself
func (s *GatewaySessionStore) AppendMessagesBatch(args ...any) { noop(args...) }

// Token accounting flush the gateway performs itself
func (s *GatewaySessionStore) FlushTokenCounts(args ...any) { noop(args...) }

// Model record the gateway performs itself
func (s *GatewaySessionStore) UpdateSessionModel(args ...any) { noop(args...) }

// Model config patch the gateway performs itself
func (s *GatewaySessionStore) PatchSessionModelConfig(args ...any) { noop(args...) }

// Archive maintenance the gateway performs itself
func (s *GatewaySessionStore) MaybeAutoArchive(args ...any) { noop(args...) }

// Prune and vacuum maintenance the gateway performs itself
func (s *GatewaySessionStore) MaybeAutoPruneAndVacuum(args ...any) { noop(args...) }

// Metadata write the gateway performs itself
func (s *GatewaySessionStore) SetMeta(args ...any) { noop(args...) }

// Token accounting the gateway performs itself
func (s *GatewaySessionStore) UpdateTokenCounts(args ...any) { noop(args...) }

// Writer close the gateway owns, the viewer holding no writer
func (s *GatewaySessionStore) Close(args ...any) { noop(args...) }

// Ghost session repair the gateway owns, zero rows repaired here
func (s *GatewaySessionStore) PruneEmptyGhostSessions(args ...any) int { return zero(args...) }

// Orphaned compression repair the gateway owns, zero rows repaired here
func (s *GatewaySessionStore) FinalizeOrphanedCompressionSessions(args ...any) int {
	return zero(args...)
}

// Accepts any arguments and does nothing, the answer to a write the gateway already performs
func noop(...any) {}

// Accepts any arguments and counts no rows, the answer to a repair the gateway owns
func zero(...any) int { return 0 }
