package media

import (
	"regexp"
	"strings"
)

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
	cleaned := strings.ToLower(strings.TrimSpace(transcript))
	return cleaned == "" || whisperHallucinations[strings.TrimRight(cleaned, ".!")] || hallucinationRepeatRe.MatchString(cleaned)
}
