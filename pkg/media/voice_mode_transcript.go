//go:build !icebath_core && !icebath_noextras

package media

import (
	"context"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pydifflib"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/settings"
)

// Configured voice stop phrases lowercased with blanks dropped, empty when disabled
var loadVoiceStopPhrases = func(ctx context.Context) []string {
	phrases := []string{}
	for _, p := range settings.VoiceStopPhrases.Strings(ctx) {
		if p = strings.ToLower(strings.TrimSpace(p)); p != "" {
			phrases = append(phrases, p)
		}
	}
	return phrases
}

// Whether the whole utterance stripped of punctuation equals a configured stop phrase
func isVoiceStopPhrase(ctx context.Context, transcript string, stopPhrases []string) bool {
	cleaned := ""
	if transcript != "" {
		cleaned = strings.Trim(strings.ToLower(strings.TrimSpace(transcript)), ".,!?;: \t\n\"'")
	}
	if cleaned == "" {
		return false
	}
	if stopPhrases == nil {
		stopPhrases = loadVoiceStopPhrases(ctx)
	}
	for _, p := range stopPhrases {
		if p == cleaned {
			return true
		}
	}
	return false
}

const (
	defaultTTSEchoSimilarityThreshold = 0.6
	minFragmentLengthForEcho          = 10
)

// Whitespace runs collapsed to one space, trimmed and lowercased
func normalizeForEchoCompare(text string) string {
	var b strings.Builder
	inSpace := false
	for _, r := range text {
		if pystr.IsSpace(r) {
			if !inSpace {
				b.WriteByte(' ')
			}
			inSpace = true
			continue
		}
		inSpace = false
		b.WriteRune(r)
	}
	return strings.ToLower(strings.TrimSpace(b.String()))
}

// Whether a transcript reads as speaker bleed of the spoken reply by whole or sliding window similarity
func isTTSEcho(transcript, spokenText string, threshold float64) bool {
	a, b := pystr.Runes(normalizeForEchoCompare(transcript)), pystr.Runes(normalizeForEchoCompare(spokenText))
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	similar := func(x, y []rune) bool { return pydifflib.NewSequenceMatcher(x, y, true).Ratio() >= threshold }
	if similar(a, b) {
		return true
	}
	if len(a) < minFragmentLengthForEcho || len(a) >= len(b) {
		return false
	}
	for start := 0; start < len(b)-len(a)+1; start++ {
		if similar(a, b[start:start+len(a)]) {
			return true
		}
	}
	return false
}

// Spoken hint naming the first configured stop phrase, empty when stop phrases are disabled
func voiceStopHint(ctx context.Context) string {
	phrases := loadVoiceStopPhrases(ctx)
	if len(phrases) == 0 {
		return ""
	}
	return `Say "` + phrases[0] + `" to end the voice chat.`
}
