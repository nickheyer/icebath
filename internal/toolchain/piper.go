package toolchain

import "strings"

const piperVoicePrefix = "piper-voice-"

func PiperVoice(voice string) string { return piperVoicePrefix + voice }

func PiperVoiceDir(voice string) string { return "vits-piper-" + voice }

func piperVoiceRecipe(voice string) *recipe {
	return &recipe{name: PiperVoice(voice), kind: Data, files: []string{PiperVoiceDir(voice)}}
}

func (s *Store) PiperVoices() []string {
	var voices []string
	for _, name := range s.order {
		if voice, ok := strings.CutPrefix(name, piperVoicePrefix); ok {
			voices = append(voices, voice)
		}
	}
	return voices
}
