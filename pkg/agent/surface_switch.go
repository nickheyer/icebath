package agent

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/mcp"
	"github.com/nickheyer/icebath/pkg/observability"
	"github.com/nickheyer/icebath/pkg/state"
)

const (
	surfaceSwitchNotePrefix = "[System: This conversation is now being answered on a different interface: "
	surfaceNameEnd          = " — any earlier interface guidance"
	surfaceNoteScanTail     = 200
)

// Tool names of the agent in wire order, a seam over the run_agent snapshot
var agentToolNamesFn = func(a *AIAgent) []string { return mcp.AgentToolNames(a) }

func init() { observability.IdentityLineValueHook = IdentityLineValue }

// SplitRuntimeBoundary splits a persisted prompt into identity, runtime marker and runtime, all identity without the end marker
func SplitRuntimeBoundary(prompt string) (string, string, string) {
	if !strings.HasSuffix(prompt, RuntimeEnvironmentEnd) {
		return prompt, "", ""
	}
	marker := "\n\n" + RuntimeEnvironmentHeading + "\n\n"
	i := strings.LastIndex(prompt, marker)
	if i < 0 {
		return "", "", prompt
	}
	return prompt[:i], marker, prompt[i+len(marker):]
}

// The labelled value from the host lines that follow the user home line, empty when absent
func runtimeHostValue(prompt, label string) string {
	_, marker, runtime := SplitRuntimeBoundary(prompt)
	prefix := label + ":"
	source := prompt
	if marker != "" {
		source, _, _ = strings.Cut(runtime, "\n\n")
	}
	lines := pystr.Splitlines(source, false)
	for idx, line := range lines {
		if !strings.HasPrefix(line, "User home directory:") {
			continue
		}
		for _, candidate := range lines[idx+1 : min(idx+4, len(lines))] {
			if strings.HasPrefix(candidate, prefix) {
				return pystr.Strip(candidate[len(prefix):])
			}
		}
	}
	return ""
}

// IdentityLineValue is the last labelled value in the trailer paragraph that ends the identity portion
func IdentityLineValue(prompt, label string) string {
	prefix := label + ":"
	identity, _, _ := SplitRuntimeBoundary(prompt)
	trailer := identity
	if i := strings.LastIndex(identity, "\n\n"); i >= 0 {
		trailer = identity[i+2:]
	}
	value := ""
	for _, line := range pystr.Splitlines(trailer, false) {
		if strings.HasPrefix(line, prefix) {
			value = pystr.Strip(line[len(prefix):])
		}
	}
	return value
}

// The surface named by the newest switch note on a recent user row, empty when none
func lastAnnouncedSurface(history []map[string]any) string {
	start := max(len(history)-surfaceNoteScanTail, 0)
	for i := len(history) - 1; i >= start; i-- {
		msg := history[i]
		if msg == nil || msg["role"] != "user" {
			continue
		}
		sidecar, _ := msg["api_content"].(string)
		text := sidecar + "\n" + state.FlattenMessageText(msg["content"], "\n")
		if j := strings.LastIndex(text, surfaceSwitchNotePrefix); j >= 0 {
			name, _, _ := strings.Cut(text[j+len(surfaceSwitchNotePrefix):], surfaceNameEnd)
			return pystr.Strip(name)
		}
	}
	return ""
}

// NoteInertPinnedTools names at the end of the staged note the pinned tools this surface did not build
func NoteInertPinnedTools(a *AIAgent, builtForThisSurface []string) {
	built := make(map[string]bool, len(builtForThisSurface))
	for _, name := range builtForThisSurface {
		built[name] = true
	}
	var inert []string
	for _, name := range agentToolNamesFn(a) {
		if !built[name] {
			inert = append(inert, name)
		}
	}
	if len(inert) == 0 || a.surfaceSwitchNote == "" {
		return
	}
	a.surfaceSwitchNote += "\n[System: These tools stay listed for this conversation (dropping them would discard " +
		"the cached request prefix) but were not loaded for this interface — expect a call to " +
		"one of them to fail: " + strings.Join(inert, ", ") + ".]"
}

// StageSurfaceSwitchNote stages a one shot turn note when the request would misdescribe the surface, reporting whether it staged
func StageSurfaceSwitchNote(ctx context.Context, a *AIAgent, prompt string, history []map[string]any) bool {
	if a.provider == "moa" || a.apiMode == "codex_app_server" {
		return false
	}
	current := pystr.Strip(a.platform)
	if current == "" {
		return false
	}
	described := IdentityLineValue(prompt, "Platform")
	told := lastAnnouncedSurface(history)
	if told == "" {
		told = described
	}
	if told == "" || told == current {
		return false
	}
	hint := ""
	if described != current {
		hint = PlatformHint(ctx, a)
	}
	where := "the interface section in the system prompt above"
	if hint != "" {
		where = "the guidance below"
	}
	note := surfaceSwitchNotePrefix + current + surfaceNameEnd + " in this conversation is " +
		"superseded — follow " + where + " for formatting, file delivery and any interface-specific " +
		"capability.]"
	if hint != "" {
		note += "\n" + hint
	}
	a.surfaceSwitchNote = note
	slog.Info(fmt.Sprintf("Session %s switched surface %s -> %s; keeping the stored system prompt and delivering "+
		"the new surface guidance as a turn note (prefix cache preserved).", a.sessionID, told, current))
	return true
}
