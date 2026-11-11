package gateway

import (
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Surfaces one turn may name
var turnSurfaces = map[string]bool{"hud": true, "voice-live": true}

// Most characters of a spoken transcript a live voice delegation commits
const voiceContextLimit = 6000

// True for a wire flag that is absent, None or a real boolean
func optionalBool(value any) (bool, bool) {
	if value == nil {
		return false, true
	}
	flag, ok := value.(bool)
	return flag, ok
}

// Committed surface fields of a submission, empty when it names none
func admitSurface(params map[string]any) (*pyjson.Object, error) {
	surface, context := params["surface"], params["voice_context"]
	name, isName := surface.(string)
	if surface != nil && (!isName || !turnSurfaces[name]) {
		return nil, runtimeRefusal("invalid_params")
	}
	spoken, isSpoken := context.(string)
	if context != nil && (name != "voice-live" || !isSpoken) {
		return nil, runtimeRefusal("invalid_params")
	}
	interrupted, okInterrupted := optionalBool(params["interrupted"])
	voiceTurn, okVoiceTurn := optionalBool(params["voice_turn"])
	if !okInterrupted || !okVoiceTurn {
		return nil, runtimeRefusal("invalid_params")
	}
	committed := pyjson.NewObject()
	if name != "" {
		committed.Set("surface", name)
	}
	if spoken != "" {
		committed.Set("voice_context", pystr.Slice(spoken, 0, voiceContextLimit))
	}
	if interrupted {
		committed.Set("interrupted", true)
	}
	if voiceTurn {
		committed.Set("voice_turn", true)
	}
	if committed.Len() == 0 {
		return pyjson.NewObject(), nil
	}
	return pyjson.ObjectOf("surface_v1", committed), nil
}
