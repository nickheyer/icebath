package gatewayrun

import (
	"context"
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/skills"
)

// Prepends the topic or channel bound skill payloads to the event text of a new session
func (r *GatewayRunner) hmwaAutoLoadSkills(ctx context.Context, event *gateway.MessageEvent, skillNames []string, quickKey, sessionKey string) {
	defer func() {
		if rec := recover(); rec != nil {
			runLog().Warn(fmt.Sprintf("[Gateway] Failed to auto-load skill(s) %s: %v", pyStrList(skillNames), rec))
		}
	}()
	scope := skills.NewScope(home.Dir(ctx), nil)
	var parts, loadedNames []string
	for _, name := range skillNames {
		loaded, skillDir, displayName, ok := scope.LoadSkillPayload(ctx, name, quickKey)
		if !ok {
			runLog().Warn(fmt.Sprintf("[Gateway] Auto-skill '%s' not found", name))
			continue
		}
		part := scope.BuildSkillMessage(ctx, loaded, skillDir,
			fmt.Sprintf("[IMPORTANT: The \"%s\" skill is auto-loaded. Follow its instructions for this session.]", displayName), "", "", "")
		if part != "" {
			parts = append(parts, part)
			loadedNames = append(loadedNames, name)
		}
	}
	if len(parts) > 0 {
		parts = append(parts, event.Text)
		event.Text = strings.Join(parts, "\n\n")
		runLog().Info(fmt.Sprintf("[Gateway] Auto-loaded skill(s) %s for session %s", pyStrList(loadedNames), sessionKey))
	}
}

// Python list repr of strings for log lines
func pyStrList(items []string) string {
	quoted := make([]string, len(items))
	for i, s := range items {
		quoted[i] = "'" + strings.ReplaceAll(s, "'", "\\'") + "'"
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}
