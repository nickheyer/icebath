package provider

import "context"

type portalCtxKey int

const (
	portalConversationKey portalCtxKey = iota
	portalAffinityKey
)

// Child context publishing the declared routing affinity scope, an empty scope clears it
func WithPortalAffinityScope(ctx context.Context, scope string) context.Context {
	return context.WithValue(ctx, portalAffinityKey, scope)
}

// Declared routing affinity scope the context carries, empty when unset
func portalAffinityScope(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	s, _ := ctx.Value(portalAffinityKey).(string)
	return s
}

// Child context publishing the conversation lineage root id for ambient Portal tagging, an empty id clears it
func WithPortalConversationContext(ctx context.Context, conversationID string) context.Context {
	return context.WithValue(ctx, portalConversationKey, conversationID)
}

// Conversation lineage root id the context carries, empty when unset
func portalConversationContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	s, _ := ctx.Value(portalConversationKey).(string)
	return s
}

// Client attribution tag carrying the running build's base version
func icebathClientTag() string {
	return "client=icebath-client-v" + BuildVersion
}

// Conversation tag for a session id
func conversationTag(sessionID string) string {
	return "conversation=" + sessionID
}

// Fresh Nous Portal tag list, the ambient conversation id winning over the explicit session id
func nousPortalTags(ctx context.Context, sessionID string) []string {
	tags := []string{"product=icebath-agent", icebathClientTag()}
	if effective := firstNonEmpty(portalConversationContext(ctx), sessionID); effective != "" {
		tags = append(tags, conversationTag(effective))
	}
	return tags
}

// Exported fresh Nous Portal tag list for packages that assert the tags an agent request carries
func NousPortalTags(ctx context.Context, sessionID string) []string {
	return nousPortalTags(ctx, sessionID)
}
