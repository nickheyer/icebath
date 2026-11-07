package provider

import "context"

// Conversation lineage root id the context carries for ambient Portal tagging, empty when unset
func PortalConversationContext(ctx context.Context) string { return portalConversationContext(ctx) }

// Declared routing affinity scope the context carries, empty when unset
func PortalAffinityScope(ctx context.Context) string { return portalAffinityScope(ctx) }
