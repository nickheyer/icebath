package agent

import (
	"context"
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/tools"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// SpeechInterruptedNote is prepended to the model bound message after a barge in on a spoken reply
const SpeechInterruptedNote = "[Note: the user interrupted your previous spoken reply before it finished.]"

// Model bound note of a committed surface, the barge in note first then the HUD or spoken delegation note
func SurfaceNote(committed *jsonutil.Object, validToolNames map[string]bool) string {
	var notes []string
	if anyutil.IsTrue(committed.ValueOr("interrupted", nil)) {
		notes = append(notes, SpeechInterruptedNote)
	}
	switch committed.ValueOr("surface", nil) {
	case "hud":
		notes = append(notes, HudSurfaceNote(validToolNames, nil))
	case "voice-live":
		voiceContext := committed.ValueOr("voice_context", nil)
		text := ""
		if anyutil.IsTrue(voiceContext) {
			text = anyutil.String(voiceContext)
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
	if !anyutil.IsTrue(committed) {
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
