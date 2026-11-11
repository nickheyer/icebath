package tuiserver

import (
	"context"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/media"
)

// Push to talk gate, a shared owner session's own voice mode when an owner here holds it, else the env flag
func sessionVoiceEnabled(sessionID any) (bool, error) {
	held, err := gateway.LocalVoiceEnabled(sessionID)
	if err != nil {
		return false, err
	}
	if held == nil {
		return voiceModeEnabled(), nil
	}
	return *held, nil
}

// Whether text is a voice stop phrase, false and logged when the voice config cannot be read
func typedStopPhrase(text string) (stop bool) {
	defer func() {
		if r := recover(); r != nil {
			logger().Debug("typed stop-phrase check failed; submitting the text", "error", r)
			stop = false
		}
	}()
	return media.IsVoiceStopPhrase(text)
}

// Ends the session's voice chat when text is a bare stop phrase typed while its voice mode is on
func consumeTypedStopPhrase(ctx context.Context, text any, sessionID string, emit bool) (bool, error) {
	typed, isStr := text.(string)
	if !isStr {
		return false, nil
	}
	enabled, err := sessionVoiceEnabled(sessionID)
	if err != nil || !enabled {
		return false, err
	}
	if !typedStopPhrase(typed) {
		return false, nil
	}
	endSessionVoiceChat(ctx, sessionID, true, true)
	if emit {
		voiceEmit(ctx, "voice.transcript", map[string]any{"stop_phrase": true, "typed": true})
	}
	logger().Info("prompt.submit: typed stop phrase \u2014 voice chat ended")
	return true, nil
}

// Typed stop check of the canonical prompt submit, the submitter answered in the reply instead of an event
func ConsumeTypedStopPhrase(ctx context.Context, text, sessionID string) (bool, error) {
	return consumeTypedStopPhrase(ctx, text, sessionID, false)
}
