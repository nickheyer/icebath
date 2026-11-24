package tools

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
)

// Stable config.yaml keys under onboarding.seen
const (
	OnboardingBusyInputFlag    = "busy_input_prompt"
	OnboardingToolProgressFlag = "tool_progress_prompt"
	OnboardingProfileBuildFlag = "profile_build_offered"
)

// Busy input hints keyed by the busy_input_mode just applied, interrupt being the default branch
var busyInputHintsGateway = map[string]string{
	"queue": "\U0001F4A1 First-time tip — I queued your message instead of interrupting. Send `/busy interrupt` to make new messages " +
		"stop the current task immediately, or `/busy status` to check. This notice won't appear again.",
	"steer": "\U0001F4A1 First-time tip — I steered your message into the current run; it will arrive after the next tool " +
		"call instead of interrupting. Send `/busy interrupt` or `/busy queue` to change this, or `/busy " +
		"status` to check. This notice won't appear again.",
	"redirect": "\U0001F4A1 First-time tip — I redirected the current run using your message. Completed work stays in " +
		"context, and `/stop` still cancels the task. Send `/busy queue` to wait for a separate turn, or " +
		"`/busy status` to check. This notice won't appear again.",
}

const busyInputHintGatewayDefault = "\U0001F4A1 First-time tip — I just interrupted my current task to answer you. Send `/busy queue` to queue " +
	"follow-ups for after the current task instead, `/busy steer` to inject them mid-run without " +
	"interrupting, or `/busy status` to check. This notice won't appear again."

// Markdown hint shown the first time a user messages while the agent is busy
func BusyInputHintGateway(mode string) string {
	if hint, ok := busyInputHintsGateway[mode]; ok {
		return hint
	}
	return busyInputHintGatewayDefault
}

// Markdown hint shown once after a long running tool streamed every step
func ToolProgressHintGateway() string {
	return "\U0001F4A1 First-time tip — that tool took a while and I'm streaming every step. If the progress messages " +
		"feel noisy, send `/verbose` to cycle modes (all → new → off). This notice won't appear again."
}

// Plain text CLI version of the tool progress hint
func ToolProgressHintCLI() string {
	return "(tip) That tool ran for a while. Use /verbose to cycle tool-progress " +
		"display modes (all -> new -> off -> verbose). This tip only shows once."
}

// The onboarding mapping of a config, empty for anything else
func onboardingSection(cfg any) map[string]any {
	m, ok := cfg.(map[string]any)
	if !ok {
		return map[string]any{}
	}
	section, ok := m["onboarding"].(map[string]any)
	if !ok {
		return map[string]any{}
	}
	return section
}

// off never offers the profile build, every other onboarding.profile_build value asks
func ProfileBuildMode(cfg any) string {
	if mode, ok := onboardingSection(cfg)["profile_build"].(string); ok && strings.ToLower(strings.TrimSpace(mode)) == "off" {
		return "off"
	}
	return "ask"
}

// Shared by both first contact notes so a real first message task is never replaced by the intro
const OnboardingTaskFirstClause = "If this message is itself a real request or task, DO THE TASK FIRST -- call " +
	"whatever tools it needs -- and only then, in the closing sentences of that same " +
	"reply, do what this note asks. Never let this note replace or skip work the user " +
	"actually asked for. "

// First contact note when the profile build offer is off or already latched
const OnboardingPlainIntroNote = "[System note: This is the user's very first message ever. " +
	OnboardingTaskFirstClause +
	"What this note asks: briefly introduce yourself and mention that /help shows " +
	"available commands, in one or two sentences.]"

// One shot sidecar note for the install's first ever message, nil when this is not first contact
func FirstContactTurnNote(ctx context.Context, cfg any, configPath string, sessionHistoryEmpty, installHasPriorSessions bool) *string {
	if !sessionHistoryEmpty || installHasPriorSessions {
		return nil
	}
	if ProfileBuildMode(cfg) == "ask" && !IsSeen(cfg, OnboardingProfileBuildFlag) {
		MarkSeen(ctx, configPath, OnboardingProfileBuildFlag)
		note := strings.TrimSpace(ProfileBuildDirective())
		return &note
	}
	note := OnboardingPlainIntroNote
	return &note
}

// System note directive of the very first message, an opt in profile build that asks before any lookup
func ProfileBuildDirective() string {
	return "\n\n" +
		"[System note: This is the user's very first message ever. " + OnboardingTaskFirstClause +
		"What this note asks: after a one-sentence introduction (mention /help " +
		"shows commands), OFFER — do not assume — to build a short profile of them so you can be more useful, and " +
		"explain they can decline or do it later. If and ONLY IF they accept:\n" +
		"  1. Ask for whatever they're comfortable sharing (name, what they do, how they like you to work). " +
		"Volunteered facts come first.\n" +
		"  2. Before ANY external lookup, say what you intend to look up and get explicit consent for that step. Never " +
		"read their connected accounts (email, calendar, etc.) silently — ask each time.\n" +
		"  3. With consent, you may use web_search to confirm public details (e.g. employer, public profiles) from the " +
		"data points they gave.\n" +
		"  4. Save each confirmed, durable fact with the memory tool using target=\"user\" — keep entries compact and " +
		"high-signal.\n" +
		"If they decline at any point, stop immediately and continue normally. Keep the whole exchange light and " +
		"conversational, not an interrogation.]"
}

// True when the user has already been shown this first touch hint
func IsSeen(cfg any, flag string) bool {
	seen, ok := onboardingSection(cfg)["seen"].(map[string]any)
	if !ok {
		return false
	}
	return pyTruthy(seen[flag])
}

// Persists onboarding.seen.flag as true atomically, false on any error
func MarkSeen(ctx context.Context, configPath, flag string) bool {
	cfg, err := config.ReadUserConfigRaw(ctx, configPath)
	if err != nil {
		pkgLog().Debug(fmt.Sprintf("onboarding: failed to mark flag %s: %s", flag, err))
		return false
	}
	section, ok := cfg["onboarding"].(map[string]any)
	if !ok {
		section = map[string]any{}
		cfg["onboarding"] = section
	}
	seen, ok := section["seen"].(map[string]any)
	if !ok {
		seen = map[string]any{}
		section["seen"] = seen
	}
	if seen[flag] != true {
		seen[flag] = true
		if err := config.AtomicConfigWrite(ctx, filepath.Dir(configPath), configPath, cfg, ""); err != nil {
			pkgLog().Debug(fmt.Sprintf("onboarding: failed to mark flag %s: %s", flag, err))
			return false
		}
	}
	return true
}
