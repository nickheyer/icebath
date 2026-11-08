package agent

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/nickheyer/icebath/pkg/botmode"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const (
	messageAgentToolName = "message_agent"
	messageMaxChars      = 16000
)

// OpenAI format message_agent schema the Bot Chat agent gets injected, never registered
func messageAgentToolSchema() map[string]any {
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name": messageAgentToolName,
			"description": "Send a message to ANOTHER agent (teammate) on this install, or to an " +
				"agent on a registered peer gateway. This is FIRE-AND-FORGET and " +
				"asynchronous, like texting: it validates the target against the live " +
				"roster, delivers your message into that agent's own Bot Chat with your " +
				"attribution automatically prefixed, and returns immediately with a " +
				"dispatch acknowledgement — status queued plus a delivery_id (the hand-off to a background " +
				"delivery process, not a delivery receipt). It does NOT return their reply and you must " +
				"not wait or poll for one — send it, finish your turn, and that process's " +
				"completion notification wakes you with the outcome: their reply, or the " +
				"delivery failure — unless the ack returns reply_delivery=\"poll\", in which case " +
				"follow its process(action=\"wait\") instruction before ending the turn. COMPOSE the message yourself: write what YOU want to say to " +
				"that agent (lead with the point; include the concrete ask or result). " +
				"Never paste the user's words verbatim — paraphrase the actionable " +
				"substance, and keep private 1:1 chat content private. Message one " +
				"clearly relevant teammate when it genuinely helps the user's goal; " +
				"don't fan out to several agents unless the user explicitly asked. " +
				"Use the teammate roster in your system prompt (names + roles) to pick " +
				"the right recipient; targets: a teammate name (e.g. 'researcher'), " +
				"'<peer>/<agent>' for an agent on a registered peer gateway " +
				"(e.g. 'spark/researcher', or just '<peer>' for the peer's main agent), " +
				"or an agent on another connected machine from your roster (use " +
				"'<handle>@<connection>' if the same handle exists on several).",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"target": map[string]any{
						"type": "string",
						"description": "Who to message: a teammate profile name from your roster " +
							"('researcher', 'icebath' for the default agent), or " +
							"'<peer>' / '<peer>/<agent>' for a registered peer gateway.",
					},
					"message": map[string]any{
						"type": "string",
						"description": "The message YOU composed for that agent (max " +
							fmt.Sprint(messageMaxChars) + " chars). Do not include the " +
							"'Message from …' prefix — it is added automatically.",
					},
				},
				"required": []any{"target", "message"},
			},
		},
	}
}

// True only for a protocol enabled agent in the canonical Bot Chat of a Bot Mode managed install, never failing
func messageAgentAuthorized(a *AIAgent) (ok bool) {
	defer func() {
		if rec := recover(); rec != nil {
			turnContextLog().Debug("message_agent_authorized failed", "error", rec)
			ok = false
		}
	}()
	if !a.botModeProtocol {
		return false
	}
	return botModeSessionTitle(a) == botmode.BotChatTitle && botmode.IsBotModeManaged(botModeAgentHome(a))
}

// Injects message_agent once into an authorized agent's tools and keeps it dispatchable, never failing
func ensureMessageAgentTool(ctx context.Context, a *AIAgent) (ok bool) {
	defer func() {
		if rec := recover(); rec != nil {
			turnContextLog().Debug("ensure_message_agent_tool failed", "error", rec)
			ok = false
		}
	}()
	if !a.botModeProtocol {
		return false
	}
	present := false
	for _, t := range a.tools {
		if m, isMap := t.(map[string]any); isMap {
			if fn, _ := m["function"].(map[string]any); fn != nil && fn["name"] == messageAgentToolName {
				present = true
				break
			}
		}
	}
	if !present {
		if !messageAgentAuthorized(a) {
			return false
		}
		if a.tools == nil {
			a.tools = []any{}
		}
		a.tools = append(a.tools, messageAgentToolSchema())
	}
	if a.validToolNames != nil {
		a.validToolNames[messageAgentToolName] = true
	}
	return true
}

// The agent's own home from its session db path, the process home when it has none
func botModeAgentHome(a *AIAgent) string {
	if a.sessionDB != nil {
		if p := a.sessionDB.Path(); p != "" {
			return filepath.Dir(p)
		}
	}
	return home.Process()
}

// Session title from the hint, else the stored title, empty when neither is readable
func botModeSessionTitle(a *AIAgent) string {
	if title := pystr.Strip(a.sessionTitleHint); title != "" {
		return title
	}
	if a.sessionDB != nil && a.sessionID != "" {
		if t, _, err := a.sessionDB.GetSessionTitle(context.Background(), a.sessionID); err == nil {
			return pystr.Strip(t)
		}
	}
	return ""
}
