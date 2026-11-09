package tools

import (
	"context"

	"github.com/nickheyer/icebath/pkg/state"
)

// Agent fields the onboarding chat pieces read
type OnboardingAgent interface {
	ValidToolNames() map[string]bool
	EnabledToolsets() []string
	SessionID() string
	SessionDB() *state.DB
}

// Agent whose pending gateway turn notes the onboarding note rides on
type OnboardingNoteAgent interface {
	GatewayTurnContextNotes() string
	SetGatewayTurnContextNotes(notes string)
}

// One scripted step of the opening, the text to say then the tool call to make
type OnboardingPreludeStep struct {
	Text string
	Tool string
	Args map[string]any
}

// True when the agent was built with setup_beat among its tools
func BuildsOnboarding(agent OnboardingAgent) bool {
	if agent == nil {
		return false
	}
	return agent.ValidToolNames()["setup_beat"]
}

// An explicit empty list is a zero tool chat, nil is unrestricted and still gets the fold in
func KeepsToolsets(enabled []string) bool {
	if enabled == nil {
		return false
	}
	if len(enabled) == 0 {
		return true
	}
	for _, name := range enabled {
		if name == "onboarding" {
			return true
		}
	}
	return false
}

// The enabled toolsets plus onboarding, nil spelled out as every toolset outside the session gated ones
func WithOnboarding(ctx context.Context, enabled []string) []string {
	if enabled == nil {
		gated := ToolsetSessionPlatforms
		for _, name := range Default.OrderedToolsetNames(ctx, "") {
			if _, isGated := gated[name]; !isGated {
				enabled = append(enabled, name)
			}
		}
	}
	return append(append([]string{}, enabled...), "onboarding")
}

// The enabled toolsets plus onboarding for a chat whose run is in progress, decided once at agent build
func OnboardingToolsets(ctx context.Context, enabled []string, db *state.DB, sessionID string) []string {
	if KeepsToolsets(enabled) {
		return enabled
	}
	if OnboardingActive(LoadOnboardingRecord(db, sessionID)) {
		return WithOnboarding(ctx, enabled)
	}
	return enabled
}

// The session authority per turn fold in, onboarding added for a chat that is or was an onboarding chat
func OnboardingChatToolsets(ctx context.Context, enabled []string, db *state.DB, sessionID string) []string {
	if KeepsToolsets(enabled) || db == nil {
		return enabled
	}
	tip, err := db.CompressionTip(sessionID)
	if err != nil || tip == "" {
		tip = sessionID
	}
	if LoadOnboardingRecord(db, tip) != nil {
		return WithOnboarding(ctx, enabled)
	}
	return enabled
}

// A tool refresh keeps the onboarding toolset the agent was built with
func CarryOnboardingToolset(ctx context.Context, enabled []string, agent OnboardingAgent) []string {
	if KeepsToolsets(enabled) || agent == nil {
		return enabled
	}
	for _, name := range agent.EnabledToolsets() {
		if name == "onboarding" {
			return WithOnboarding(ctx, enabled)
		}
	}
	return enabled
}

// The scripted opening while the name beat was never shown, nil otherwise, a greeting already said not repeated
func OnboardingPrelude(agent OnboardingAgent, history []map[string]any) []OnboardingPreludeStep {
	if !BuildsOnboarding(agent) {
		return nil
	}
	record := LoadOnboardingRecord(agent.SessionDB(), agent.SessionID())
	if record == nil || !OnboardingActive(record) {
		return nil
	}
	beats, _ := record["beats"].(map[string]any)
	if beats["name"] != onbPending {
		return nil
	}
	facts, _ := record["facts"].(map[string]any)
	locale, _ := facts["locale"].(string)
	hello := OnboardingGreetingFor(locale)
	said := false
	for _, m := range history {
		if m["role"] == "assistant" && m["content"] == hello {
			said = true
			break
		}
	}
	text := hello
	if said {
		text = ""
	}
	return []OnboardingPreludeStep{{Text: text, Tool: "setup_beat", Args: map[string]any{"beat": "name"}}}
}

// Which closed card a user turn in an open onboarding chat brings back, empty while a card waits
func OnboardingSessionTurnNote(agent OnboardingAgent, historyEmpty, cardWaiting bool) string {
	if historyEmpty || cardWaiting || !BuildsOnboarding(agent) {
		return ""
	}
	record := LoadOnboardingRecord(agent.SessionDB(), agent.SessionID())
	if record == nil || !OnboardingActive(record) {
		return ""
	}
	return OnboardingTurnNote(record)
}

// Appends the note to the agent's pending gateway turn notes, a blank note ignored
func AppendTurnNote(agent OnboardingNoteAgent, note string) {
	if note == "" || agent == nil {
		return
	}
	if prior := agent.GatewayTurnContextNotes(); prior != "" {
		agent.SetGatewayTurnContextNotes(prior + "\n\n" + note)
		return
	}
	agent.SetGatewayTurnContextNotes(note)
}
