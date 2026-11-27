package speech

import (
	"regexp"
	"strings"
	"unicode"

	"github.com/nickheyer/icebath/internal/config"
	speechconfig "github.com/nickheyer/icebath/internal/speech/config"
	"github.com/nickheyer/icebath/internal/tools/diff"
)

// What Whisper writes for silence or noise, which counts as nothing said
var hallucinations = map[string]bool{
	"thank you": true, "thanks for watching": true, "subscribe to my channel": true, "like and subscribe": true, "please subscribe": true,
	"thank you for watching": true, "bye": true, "you": true, "the end": true, "продолжение следует": true, "sous-titres": true,
	"sous-titres réalisés par la communauté d'amara.org": true, "sottotitoli creati dalla comunità amara.org": true,
	"untertitel von stephanie geiges": true, "amara.org": true, "www.mooji.org": true, "ご視聴ありがとうございました": true,
}

var hallucinationRepeat = regexp.MustCompile(`(?i)^(?:thank you|thanks|bye|you|ok|okay|the end|\.|[\s\x{0b}\x{1c}-\x{1f}\x{85}\p{Z}]|,|!)+$`)

// Whether a transcript is empty or what Whisper is known to hear in silence
func Hallucination(transcript string) bool {
	cleaned := strings.ToLower(strings.TrimSpace(transcript))
	return cleaned == "" || hallucinations[strings.TrimRight(cleaned, ".!")] || hallucinationRepeat.MatchString(cleaned)
}

// Stop phrases of a profile, lower case with blanks dropped
func StopPhrases(cfg *config.Config) []string {
	var phrases []string
	for _, phrase := range cfg.GetStringSlice(speechconfig.StopPhrases) {
		if phrase = strings.ToLower(strings.TrimSpace(phrase)); phrase != "" {
			phrases = append(phrases, phrase)
		}
	}
	return phrases
}

// Whether the whole utterance, its punctuation trimmed, is one of the stop phrases
func StopPhrase(phrases []string, transcript string) bool {
	cleaned := strings.Trim(strings.ToLower(strings.TrimSpace(transcript)), ".,!?;: \t\n\"'")
	if cleaned == "" {
		return false
	}
	for _, phrase := range phrases {
		if phrase == cleaned {
			return true
		}
	}
	return false
}

// Spoken hint naming the first stop phrase, empty when there are none
func StopHint(phrases []string) string {
	if len(phrases) == 0 {
		return ""
	}
	return `Say "` + phrases[0] + `" to end the voice chat.`
}

// How alike a capture must be to what was spoken for it to count as the speaker's own sound
const echoSimilarity = 0.6

// Shortest capture compared piece by piece with what was spoken
const echoFragment = 10

func echoForm(text string) string {
	return strings.ToLower(strings.Join(strings.FieldsFunc(text, unicode.IsSpace), " "))
}

// Whether a transcript reads as the speaker's own sound picked up again: alike with what was spoken as a whole, or with a stretch of it
func Echo(transcript, spoken string) bool {
	a, b := []rune(echoForm(transcript)), []rune(echoForm(spoken))
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	if diff.Ratio(string(a), string(b)) >= echoSimilarity {
		return true
	}
	if len(a) < echoFragment || len(a) >= len(b) {
		return false
	}
	for start := 0; start+len(a) <= len(b); start++ {
		if diff.Ratio(string(a), string(b[start:start+len(a)])) >= echoSimilarity {
			return true
		}
	}
	return false
}
