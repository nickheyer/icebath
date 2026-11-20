package gateway

const relayOptString = 3

var relayReasoningChoices = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra", "reset", "show", "hide"}

// Optional string option row with its choices offered verbatim
func relayManifestOpt(name, description string, choices ...string) map[string]any {
	row := map[string]any{"type": relayOptString, "name": name, "description": description, "required": false}
	if len(choices) > 0 {
		rows := make([]any, len(choices))
		for i, c := range choices {
			rows[i] = map[string]any{"name": c, "value": c}
		}
		row["choices"] = rows
	}
	return row
}

// Command row with its options when any are given
func relayManifestCmd(name, description string, options ...map[string]any) map[string]any {
	row := map[string]any{"name": name, "description": description}
	if len(options) > 0 {
		rows := make([]any, len(options))
		for i, o := range options {
			rows[i] = o
		}
		row["options"] = rows
	}
	return row
}

// Discord slash command manifest the relay hello declares, mirroring the native command tree
func BuildRelayCommandManifest() []map[string]any {
	return []map[string]any{
		relayManifestCmd("new", "Start a new conversation"),
		relayManifestCmd("reset", "Reset your Icebath session"),
		relayManifestCmd("model", "Show or change the model", relayManifestOpt("name", "Model name. Leave empty to see current.")),
		relayManifestCmd("reasoning", "Show/change reasoning effort, or toggle showing it",
			relayManifestOpt("effort", "Level, reset, or show/hide. Leave empty to see current.", relayReasoningChoices...)),
		relayManifestCmd("personality", "Set a personality", relayManifestOpt("name", "Personality name. Leave empty to list.")),
		relayManifestCmd("retry", "Retry your last message"),
		relayManifestCmd("undo", "Remove the last exchange"),
		relayManifestCmd("status", "Show Icebath session status"),
		relayManifestCmd("sethome", "Set this chat as the home channel"),
		relayManifestCmd("stop", "Stop the running Icebath agent"),
		relayManifestCmd("steer", "Inject a message after the next tool call (no interrupt)", relayManifestOpt("text", "What to tell the agent")),
		relayManifestCmd("compress", "Compress conversation context"),
		relayManifestCmd("title", "Set or show the session title", relayManifestOpt("text", "New title. Leave empty to show.")),
		relayManifestCmd("resume", "Resume a previously-named session", relayManifestOpt("name", "Session title or id")),
		relayManifestCmd("usage", "Show token usage for this session"),
		relayManifestCmd("help", "Show available commands"),
		relayManifestCmd("insights", "Show usage insights and analytics"),
		relayManifestCmd("reload-mcp", "Reload MCP servers from config"),
		relayManifestCmd("reload-skills", "Re-scan skills for new or removed entries"),
		relayManifestCmd("voice", "Toggle voice reply mode"),
		relayManifestCmd("update", "Update Icebath Agent to the latest version"),
		relayManifestCmd("restart", "Gracefully restart the Icebath gateway"),
		relayManifestCmd("approve", "Approve a pending dangerous command",
			relayManifestOpt("scope", "Approval scope", "once", "session", "always", "all")),
		relayManifestCmd("deny", "Deny a pending dangerous command", relayManifestOpt("reason", "Why (relayed to the agent)")),
		relayManifestCmd("thread", "Create a new thread and start an Icebath session in it", relayManifestOpt("name", "Thread name")),
		relayManifestCmd("queue", "Queue a prompt for the next turn (doesn't interrupt)", relayManifestOpt("text", "The prompt to queue")),
		relayManifestCmd("bg", "Run a prompt in a separate background session", relayManifestOpt("text", "The prompt to run")),
		relayManifestCmd("btw", "Ask a side question about the current conversation", relayManifestOpt("text", "The question to answer")),
	}
}
