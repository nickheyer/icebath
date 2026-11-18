package provider

import "context"

type routingCtxKey int

const (
	conversationKey routingCtxKey = iota
	affinityKey
)

// Child context publishing the declared routing affinity scope, an empty scope clears it
func WithAffinityScope(ctx context.Context, scope string) context.Context {
	return context.WithValue(ctx, affinityKey, scope)
}

// Declared routing affinity scope the context carries, empty when unset
func AffinityScope(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	s, _ := ctx.Value(affinityKey).(string)
	return s
}

// Child context publishing the conversation lineage root id for sticky routing, an empty id clears it
func WithConversationContext(ctx context.Context, conversationID string) context.Context {
	return context.WithValue(ctx, conversationKey, conversationID)
}

// Conversation lineage root id the context carries, empty when unset
func ConversationContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	s, _ := ctx.Value(conversationKey).(string)
	return s
}
