package gateway

import (
	"context"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Text with a terminal run of end of sequence tokens cut and the remainder right stripped
func TrimTerminalSentinel(text string) string {
	runes := []rune(text)
	if start := terminalSentinelStart(runes); start >= 0 {
		return pystr.Rstrip(string(runes[:start]))
	}
	return text
}

// Whether streaming TTS already delivered this turn's audio on the adapter
func StreamingTTSTurnCompleted(adapter PlatformAdapter, sessionKey string, generation int) bool {
	return adapter.Base().streamingTTSTurnCompleted(sessionKey, generation, nil)
}

// Records the platform peer of a gateway session in the state database
func (s *SessionStore) RecordGatewaySessionPeer(ctx context.Context, sessionID, sessionKey string, source *SessionSource) error {
	return s.recordGatewaySessionPeer(ctx, sessionID, sessionKey, source, nil, false, nil)
}

// Whether the adapter synthesizes speech for the chat on its own
func ShouldAutoTTSForChat(adapter PlatformAdapter, chatID string) bool {
	return adapter.Base().shouldAutoTTSForChat(chatID)
}

// Stops the adapter typing indicator scoped to the thread metadata of the turn
func StopTypingWithMetadata(ctx context.Context, adapter PlatformAdapter, chatID string, metadata map[string]any) error {
	return adapter.Base().stopTypingWithMetadata(ctx, chatID, metadata)
}

// Defers a route's auto reset until the drain settles the executing admission
func (a *SessionAuthority) DeferReset(route string, reset func(ctx context.Context) error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.deferredResets == nil {
		a.deferredResets = map[string]func(ctx context.Context) error{}
	}
	a.deferredResets[route] = reset
}

// Finite turn requirement of the executing admission, nil outside a finite prompt
func FiniteTurnRequired(ctx context.Context) any { return finiteTurnRequired(ctx) }

// Messages of the current turn after the history offset, falling back to the last user row
func CurrentTurnMessages(messages []any, historyOffset int) ([]any, error) {
	return currentTurnMessages(messages, historyOffset)
}
