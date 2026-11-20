package gateway

import (
	"context"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

const PromptPinVersion = 1

// Validated durable snapshot of the ephemeral prompt inputs, nil when malformed or of another version
func SanitizePromptPin(pin any) *pyjson.Object {
	obj, ok := pin.(*pyjson.Object)
	if !ok {
		return nil
	}
	if v, _ := obj.Get("version"); !isPyInt(v, PromptPinVersion) {
		return nil
	}
	contextKey, _ := obj.Get("context_key")
	contextPrompt, _ := obj.Get("context_prompt")
	redactPII, _ := obj.Get("redact_pii")
	channelPrompt, _ := obj.Get("channel_prompt")
	parentChatID, _ := obj.Get("parent_chat_id")
	key, ok := contextKey.(string)
	if !ok || key == "" {
		return nil
	}
	if _, ok := contextPrompt.(string); !ok {
		return nil
	}
	if _, ok := redactPII.(bool); !ok {
		return nil
	}
	if _, ok := channelPrompt.(string); channelPrompt != nil && !ok {
		return nil
	}
	if _, ok := parentChatID.(string); parentChatID != nil && !ok {
		return nil
	}
	out := pyjson.NewObject()
	out.Set("version", int64(PromptPinVersion))
	out.Set("context_key", key)
	out.Set("context_prompt", contextPrompt)
	out.Set("redact_pii", redactPII)
	out.Set("channel_prompt", channelPrompt)
	out.Set("parent_chat_id", parentChatID)
	return out
}

// True when v is a JSON number equal to want the way Python compares 1 == 1.0
func isPyInt(v any, want int64) bool {
	switch x := v.(type) {
	case int64:
		return x == want
	case int:
		return int64(x) == want
	case float64:
		return x == float64(want)
	case bool:
		return (x && want == 1) || (!x && want == 0)
	}
	return false
}

// Persists the prompt pin unless the route moved off the expected session, the candidate durable first
func (s *SessionStore) SetPromptPin(ctx context.Context, sessionKey string, pin any, expectedSessionID *string) (bool, error) {
	cleaned := SanitizePromptPin(pin)
	if cleaned == nil {
		return false, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := s.entryLocked(ctx, sessionKey)
	if entry == nil || (expectedSessionID != nil && entry.SessionID != *expectedSessionID) {
		return false, nil
	}
	if entry.PromptPin == nil || !pyjson.Equal(entry.PromptPin, cleaned) {
		candidate := *entry
		candidate.PromptPin = cleaned
		if err := s.saveEntry(ctx, sessionKey, candidate.ToDict(), true); err != nil {
			return false, err
		}
		entry.PromptPin = cleaned
	}
	return true, nil
}

// Copy of the prompt pin while the route still owns the expected session, nil otherwise
func (s *SessionStore) GetPromptPin(ctx context.Context, sessionKey string, expectedSessionID *string) *pyjson.Object {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := s.entryLocked(ctx, sessionKey)
	if entry == nil || (expectedSessionID != nil && entry.SessionID != *expectedSessionID) {
		return nil
	}
	if entry.PromptPin == nil || entry.PromptPin.Len() == 0 {
		return nil
	}
	return entry.PromptPin.Copy()
}
