package media

import (
	"context"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pydifflib"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// The voice section of the effective config, empty when missing, malformed or unreadable
var voiceConfig = func(ctx context.Context) map[string]any {
	tree, err := config.LoadContext(ctx, home.Dir(ctx), config.Lookup(os.LookupEnv))
	if err != nil {
		return map[string]any{}
	}
	sec, ok := asStrMap(tree["voice"])
	if !ok {
		return map[string]any{}
	}
	return sec
}

var whisperHallucinations = map[string]bool{
	"thank you": true, "thanks for watching": true, "subscribe to my channel": true, "like and subscribe": true,
	"please subscribe": true, "thank you for watching": true, "bye": true, "you": true, "the end": true,
	"продолжение следует": true, "sous-titres": true, "sous-titres réalisés par la communauté d'amara.org": true,
	"sottotitoli creati dalla comunità amara.org": true, "untertitel von stephanie geiges": true,
	"amara.org": true, "www.mooji.org": true, "ご視聴ありがとうございました": true,
}

var hallucinationRepeatRe = regexp.MustCompile(`(?i)^(?:thank you|thanks|bye|you|ok|okay|the end|\.|[\s\x{0b}\x{1c}-\x{1f}\x{85}\p{Z}]|,|!)+$`)

// Whether a transcript is empty or a known Whisper hallucination, for callers outside the package
func IsWhisperHallucination(transcript string) bool { return isWhisperHallucination(transcript) }

// Whether a transcript is empty or a known Whisper hallucination on silence
func isWhisperHallucination(transcript string) bool {
	cleaned := pystr.Lower(pystr.Strip(transcript))
	return cleaned == "" || whisperHallucinations[strings.TrimRight(cleaned, ".!")] || hallucinationRepeatRe.MatchString(cleaned)
}

var defaultVoiceStopPhrases = []string{"stop"}

// Python str of a YAML scalar a stop phrase list may hold, false for any other type
func stopPhraseText(v any) (string, bool) {
	switch x := v.(type) {
	case string:
		return x, true
	case bool:
		if x {
			return "True", true
		}
		return "False", true
	case int:
		return strconv.Itoa(x), true
	case int64:
		return strconv.FormatInt(x, 10), true
	case uint64:
		return strconv.FormatUint(x, 10), true
	case float64:
		return pyrepr.Float(x), true
	}
	return "", false
}

// Configured voice stop phrases lowercased, the default stop when malformed, empty when disabled
var loadVoiceStopPhrases = func() []string {
	raw, ok := voiceConfig(context.Background())["stop_phrases"]
	if !ok {
		return defaultVoiceStopPhrases
	}
	var items []any
	switch x := raw.(type) {
	case string:
		items = []any{x}
	case []any:
		items = x
	case []string:
		for _, s := range x {
			items = append(items, s)
		}
	default:
		return defaultVoiceStopPhrases
	}
	phrases := []string{}
	for _, p := range items {
		text, ok := stopPhraseText(p)
		if ok && pystr.Strip(text) != "" {
			phrases = append(phrases, pystr.Lower(pystr.Strip(text)))
		}
	}
	return phrases
}

// Whether the whole utterance stripped of punctuation equals a configured stop phrase
func isVoiceStopPhrase(transcript string, stopPhrases []string) bool {
	cleaned := ""
	if transcript != "" {
		cleaned = pystr.StripChars(pystr.Lower(pystr.Strip(transcript)), ".,!?;: \t\n\"'")
	}
	if cleaned == "" {
		return false
	}
	if stopPhrases == nil {
		stopPhrases = loadVoiceStopPhrases()
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
	return pystr.Lower(pystr.Strip(b.String()))
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
func voiceStopHint() string {
	phrases := loadVoiceStopPhrases()
	if len(phrases) == 0 {
		return ""
	}
	return `Say "` + phrases[0] + `" to end the voice chat.`
}
