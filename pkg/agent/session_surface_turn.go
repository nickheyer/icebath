package agent

import (
	"context"
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/media"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Model bound note of a committed surface, the barge in note first then the HUD or spoken delegation note
func SurfaceNote(committed *pyjson.Object, validToolNames map[string]bool) string {
	var notes []string
	if pyrepr.Truthy(committed.GetOr("interrupted", nil)) {
		notes = append(notes, media.SpeechInterruptedNote)
	}
	switch committed.GetOr("surface", nil) {
	case "hud":
		notes = append(notes, HudSurfaceNote(validToolNames, nil))
	case "voice-live":
		voiceContext := committed.GetOr("voice_context", nil)
		text := ""
		if pyrepr.Truthy(voiceContext) {
			text = pyrepr.Str(voiceContext)
		}
		notes = append(notes, tools.VoiceLiveTurnNoteFor(text))
	}
	return joinNotes(notes)
}

// Agent fields a surface turn arms, the one turn voice marker and user message notes
type surfaceTurnAgent interface {
	SetVoiceTurnPending(pending bool)
	SetGatewayTurnContextNotes(notes string)
	ValidToolNames() map[string]bool
}

// Executing admission's surface note gated on the agent's tools, empty outside a surface turn
func SurfaceTurnNote(ctx context.Context, a surfaceTurnAgent) string {
	committed := gateway.SurfaceTurn(ctx)
	if !pyrepr.Truthy(committed) {
		return ""
	}
	return SurfaceNote(committed, a.ValidToolNames())
}

// Projects the executing admission's surface onto the agent's one turn voice route marker and user message notes
func ArmSurfaceTurn(ctx context.Context, a surfaceTurnAgent, notes []string, voiceTurn bool) {
	a.SetVoiceTurnPending(voiceTurn || gateway.SurfaceVoiceTurn(ctx))
	a.SetGatewayTurnContextNotes(joinNotes(append(append([]string(nil), notes...), SurfaceTurnNote(ctx, a))))
}

// Non empty notes joined by blank lines
func joinNotes(notes []string) string {
	kept := make([]string, 0, len(notes))
	for _, note := range notes {
		if note != "" {
			kept = append(kept, note)
		}
	}
	return strings.Join(kept, "\n\n")
}
