package prompt

import (
	"strings"

	agentconfig "github.com/nickheyer/icebath/internal/plugins/defaults/agents/agent/config"
	"github.com/nickheyer/icebath/internal/prompt"
)

// How replies are shown and files are delivered on each surface
var platformHints = map[string]string{
	"cron":       "You are running as a scheduled cron job. There is no user present — you cannot ask questions, request clarification, or wait for follow-up. Execute the task fully and autonomously, making reasonable decisions where needed. Your final response is automatically delivered to the job's configured destination — put the primary content directly in your response.",
	"cli":        "You are in a plain terminal (CLI). Markdown does NOT render — asterisks, headers, and fences appear as literal characters, so write plain text (indentation and blank lines are your only layout tools). Files: there is no attachment channel and MEDIA:/path tags are NOT intercepted here (they print as literal text) — deliver a file by stating its absolute path or URL in plain text; the user opens it themselves.",
	"tui":        "You are in the Icebath terminal UI (TUI). Files: there is no attachment channel and MEDIA:/path tags are NOT intercepted here (they print as literal text) — deliver a file by stating its absolute path or URL in plain text.",
	"api_server": "You're responding through an API server. The rendering layer is unknown — assume plain text. No markdown formatting (no asterisks, bullets, headers, code fences). Treat this like a conversation, not a document. Keep responses brief and natural. File/media delivery: images referenced as MEDIA:/absolute/path tags (.png/.jpg/.jpeg/.gif/.webp/.bmp, up to 5MB) are inlined as base64 data URLs in responses on the chat, completions, and responses endpoints. Non-image files are NOT intercepted anywhere, and the runs endpoint intercepts nothing — a MEDIA: tag there renders as literal text exposing a raw host filesystem path. For those cases, state the plain file path in your response text instead of a MEDIA: tag.",
}

// The surface's hint with the user's change to it: the surface hints setting holds, by surface, text to append, or a table whose replace
// takes the hint's place and whose append follows it. A malformed entry keeps the hint
func platformHint(env *prompt.Env) string {
	key := strings.ToLower(strings.TrimSpace(env.Platform))
	hint := platformHints[key]
	overrides, _ := env.Config.Get(agentconfig.PlatformHints).(map[string]any)
	spec := overrides[key]
	if text, ok := spec.(string); ok {
		spec = map[string]any{"append": text}
	}
	change, ok := spec.(map[string]any)
	if key == "" || !ok {
		return hint
	}
	text := func(value any) string {
		s, _ := value.(string)
		return strings.TrimSpace(s)
	}
	if replace := text(change["replace"]); replace != "" {
		hint = replace
	}
	if extra := text(change["append"]); extra != "" {
		return strings.TrimSpace(hint + "\n\n" + extra)
	}
	return hint
}
