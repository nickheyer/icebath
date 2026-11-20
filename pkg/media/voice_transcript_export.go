//go:build !icebath_core && !icebath_noextras

package media

// Whether the whole utterance is a configured voice stop phrase
func IsVoiceStopPhrase(transcript string) bool { return isVoiceStopPhrase(transcript, nil) }

// Whether a transcript reads as speaker bleed of the spoken reply at the default similarity
func IsTTSEcho(transcript, spokenText string) bool {
	return isTTSEcho(transcript, spokenText, defaultTTSEchoSimilarityThreshold)
}

// Spoken stop hint from the first configured stop phrase, empty when disabled
func VoiceStopHint() string { return voiceStopHint() }
