package tuiserver

import (
	"context"
	"time"

	"github.com/nickheyer/icebath/pkg/tuihost"
)

// Voice mode operations of the extras tier the turn and session code reaches, each nil in a build without voice mode
var voiceModeOps struct {
	endSessionChat     func(ctx context.Context, sessionID string, stopLoop, stopTTS bool)
	resumeWakeIfOwner  func(owner tuihost.Transport, retry, interval time.Duration) bool
	stopTTSStream      func(userBarge bool)
	armFullDuplex      func(ctx context.Context)
	beginTTSStream     func(ctx context.Context) *ttsTurnQueue
	continuousActive   func() bool
	speakWithBarge     func(ctx context.Context, text string)
	isStopPhrase       func(ctx context.Context, text string) bool
	stopListening      func(owner tuihost.Transport)
	startThinkingSound func(ctx context.Context) bool
	stopThinkingSound  func()
	markInterrupted    func()
	takeInterrupted    func() bool
	releaseTransport   func(transport tuihost.Transport)
}

// Ends the voice chat of a session, nothing to end without voice mode
func endSessionVoiceChat(ctx context.Context, sessionID string, stopLoop, stopTTS bool) {
	if voiceModeOps.endSessionChat != nil {
		voiceModeOps.endSessionChat(ctx, sessionID, stopLoop, stopTTS)
	}
}

// Resumes wake listening when owner still holds it, false without voice mode
func wakeResumeIfOwner(owner tuihost.Transport, retry, interval time.Duration) bool {
	return voiceModeOps.resumeWakeIfOwner != nil && voiceModeOps.resumeWakeIfOwner(owner, retry, interval)
}

// Cuts in flight streaming TTS, nothing streams without voice mode
func ttsStreamStop(userBarge bool) {
	if voiceModeOps.stopTTSStream != nil {
		voiceModeOps.stopTTSStream(userBarge)
	}
}

// Arms the full duplex barge listener of a voice turn
func armFullDuplexListener(ctx context.Context) {
	if voiceModeOps.armFullDuplex != nil {
		voiceModeOps.armFullDuplex(ctx)
	}
}

// Whether the continuous voice loop is running, false without voice mode
func voiceContinuousActive() bool {
	return voiceModeOps.continuousActive != nil && voiceModeOps.continuousActive()
}

// Whether text is a voice stop phrase, false without voice mode
func isVoiceStopPhrase(ctx context.Context, text string) bool {
	return voiceModeOps.isStopPhrase != nil && voiceModeOps.isStopPhrase(ctx, text)
}

// Stops the wake listener a transport owns
func stopWakeListening(owner tuihost.Transport) {
	if voiceModeOps.stopListening != nil {
		voiceModeOps.stopListening(owner)
	}
}

// Starts the thinking sound of a voice turn, false when it did not start or voice mode is not linked
func startThinkingSound(ctx context.Context) bool {
	return voiceModeOps.startThinkingSound != nil && voiceModeOps.startThinkingSound(ctx)
}

// Stops the thinking sound of a voice turn
func stopThinkingSound() {
	if voiceModeOps.stopThinkingSound != nil {
		voiceModeOps.stopThinkingSound()
	}
}

// Forgets the speech playback and speaker claims of a window that left, nothing to forget without voice mode
func releaseVoiceForTransport(transport tuihost.Transport) {
	if voiceModeOps.releaseTransport != nil {
		voiceModeOps.releaseTransport(transport)
	}
}
