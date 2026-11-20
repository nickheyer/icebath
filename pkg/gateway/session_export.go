package gateway

import "context"

// Whether the agent will have Discord tools this session under the scoped token and toolset config
func DiscordToolsLoaded(ctx context.Context) bool { return discordToolsLoaded(ctx) }

// Character budget of untrusted metadata inlined into a prompt
const MaxPromptMetadataChars = maxPromptMetadataChars
