package provider

import "github.com/nickheyer/icebath/pkg/observability"

// Installs the tool schema token estimate and the Portal conversation lineage the relay segments read
func init() {
	observability.EstimateToolsTokensHook = EstimateToolsTokens
	observability.ConversationContextHook = portalConversationContext
}
