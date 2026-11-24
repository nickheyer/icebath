//go:build !icebath_core && !icebath_noextras

package media

import "context"

// Whether the whole utterance is a voice stop phrase of the profile ctx serves
func IsVoiceStopPhrase(ctx context.Context, transcript string) bool {
	return isVoiceStopPhrase(ctx, transcript, nil)
}

// Whether a transcript reads as speaker bleed of the spoken reply at the default similarity
func IsTTSEcho(transcript, spokenText string) bool {
	return isTTSEcho(transcript, spokenText, defaultTTSEchoSimilarityThreshold)
}

// Spoken stop hint from the first stop phrase of the profile ctx serves, empty when disabled
func VoiceStopHint(ctx context.Context) string { return voiceStopHint(ctx) }
