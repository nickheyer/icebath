package app

import (
	"context"
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tuiserver"
)

// Resident agent of a TUI read, nil when the owner holds none
func tuiReadAgent(held any) *agent.AIAgent {
	resident, _ := held.(*agent.AIAgent)
	return resident
}

// Main's TUI session status text for the owner's session
func TUIStatusRead(ctx context.Context, a *gateway.SessionAuthority, ref gateway.SessionRef, live *gateway.LiveSession, policy *gateway.LocalSessionPolicy, held any, arg string) (string, error) {
	resident := tuiReadAgent(held)
	stored := gateway.TUIStoredID(ctx, a, ref)
	row := map[string]any{}
	if found, err := a.DB.GetSession(ctx, stored); err == nil && found != nil {
		if plain, ok := pyjson.Plain(found).(map[string]any); ok {
			row = plain
		}
	}
	handle, err := a.Handle(ctx, ref)
	if err != nil {
		return "", err
	}
	in := statusFieldsInput{ctx: home.WithOverride(ctx, a.ProfileID), sessionID: stored, meta: row,
		agentRunning: handle.ExecutionState != "idle"}
	if resident == nil {
		model, err := policy.CurrentModel(ctx, a)
		if err != nil {
			return "", err
		}
		if model != nil {
			in.model = *model
		}
		if pyrepr.Truthy(policy.Provider()) {
			in.provider = pyrepr.Str(policy.Provider())
		}
	} else {
		in.agentModel, in.agentProvider = resident.Model(), resident.Provider()
		in.agentTokens = resident.SessionTotalTokens()
	}
	fields := buildStatusFields(in)
	if fields["provider"] == "custom" {
		requested, baseURL := "", ""
		if resident != nil {
			requested, baseURL = resident.RequestedProvider(), resident.BaseURL()
		}
		if baseURL == "" && pyrepr.Truthy(policy.BaseURL()) {
			baseURL = pyrepr.Str(policy.BaseURL())
		}
		scoped := gateway.ProfileRuntimeScope(ctx, a.ProfileID, true)
		fields["provider"] = provider.SessionProviderIdentity(scoped, "custom", requested, baseURL, pyrepr.Str(fields["model"]), nil)
	}
	lines := []string{"Icebath TUI Status", ""}
	lines = append(lines, statusLines(fields, "session_id", "path")...)
	if project, ok := tuiserver.ProjectInfoForCwd(ctx, policy.Cwd).(map[string]any); ok && project != nil {
		lines = append(lines, "Project: "+pyrepr.Str(project["name"]))
	}
	lines = append(lines, statusLines(fields, "title", "model", "created", "last_activity", "tokens", "agent_running")...)
	return strings.Join(lines, "\n"), nil
}

// Role of a display row, unknown when absent or empty
func displayRole(row any) string {
	var role any
	switch typed := row.(type) {
	case map[string]any:
		role = typed["role"]
	case *pyjson.Object:
		role = typed.GetOr("role", nil)
	}
	if !pyrepr.Truthy(role) {
		return "unknown"
	}
	return pyrepr.Str(role)
}

// Main's live context output, role counts and model then the category table or the stored usage
func TUIContextRead(ctx context.Context, a *gateway.SessionAuthority, ref gateway.SessionRef, live *gateway.LiveSession, policy *gateway.LocalSessionPolicy, held any, arg string) (string, error) {
	resident := tuiReadAgent(held)
	stored, err := gateway.TUIContextID(ctx, a, ref)
	if err != nil {
		return "", err
	}
	var messages []any
	rows, readErr := a.DB.GetMessagesAsConversation(ctx, stored, state.ConversationOptions{IncludeAncestors: true, IncludeRowIDs: true})
	if readErr == nil && len(rows) > 0 {
		if messages, err = gateway.DisplayProjection(ctx, rows, a.ProfileID); err != nil {
			return "", err
		}
	}
	var breakdown []string
	if resident != nil {
		if history, err := gateway.TUIModelHistory(ctx, a.DB, stored); err == nil {
			if payload, err := agent.ComputeSessionContextBreakdown(ctx, resident, history); err == nil {
				var details *agent.ContextDetails
				switch pystr.Lower(pystr.Strip(arg)) {
				case "all", "full", "details":
					if found, err := agent.ComputeContextDetails(ctx, resident); err == nil {
						details = &found
					}
				}
				breakdown = agent.RenderContextBreakdownLines(ctx, payload, details, false)
			}
		}
	}
	lines := []string{"Conversation is empty (no messages yet)."}
	if len(messages) > 0 {
		lines = []string{fmt.Sprintf("Conversation: %d messages", len(messages))}
	}
	roles := map[string]int{}
	for _, msg := range messages {
		roles[displayRole(msg)]++
	}
	counts := make([]string, 0, 4)
	for _, role := range []string{"user", "assistant", "tool", "system"} {
		counts = append(counts, fmt.Sprintf("%s: %d", role, roles[role]))
	}
	lines = append(lines, "  "+strings.Join(counts, ", "))
	model := ""
	if resident != nil {
		model = resident.Model()
	}
	if model == "" {
		current, err := policy.CurrentModel(ctx, a)
		if err != nil {
			return "", err
		}
		if current != nil {
			model = *current
		}
	}
	if model != "" {
		lines = append(lines, "Model: "+model)
	}
	lines = append(lines, "Provider: auto")
	if len(breakdown) > 0 {
		return strings.Join(append(append(lines, ""), breakdown...), "\n"), nil
	}
	used, maximum, count := 0, 0, 0
	if resident != nil {
		if engine := resident.ContextCompressor(); engine != nil {
			base := engine.Base()
			used, maximum, count = base.LastPromptTokens, base.ContextLength, base.CompressionCount
		}
	}
	if used != 0 && maximum != 0 {
		lines = append(lines, fmt.Sprintf("Context usage: %s / %s tokens (%.1f%%)", pystr.GroupThousands(int64(used)),
			pystr.GroupThousands(int64(maximum)), float64(used)/float64(maximum)*100))
	} else if used != 0 {
		lines = append(lines, fmt.Sprintf("Context usage: %s tokens", pystr.GroupThousands(int64(used))))
	}
	if count != 0 {
		lines = append(lines, "Compressions: "+pystr.GroupThousands(int64(count)))
	}
	return strings.Join(lines, "\n"), nil
}

// Toolsets report lines of the enabled toolsets
func ToolsetsLines(ctx context.Context, enabled []string) []string {
	return toolsetsLines(ctx, enabled)
}

// Platforms report lines
func PlatformsLines(ctx context.Context) []string { return platformsLines(ctx) }
