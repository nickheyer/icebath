package speech

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/speech/commands"
	"github.com/nickheyer/icebath/internal/plugins/defaults/speech/deepinfra"
	"github.com/nickheyer/icebath/internal/plugins/defaults/speech/edge"
	"github.com/nickheyer/icebath/internal/plugins/defaults/speech/elevenlabs"
	"github.com/nickheyer/icebath/internal/plugins/defaults/speech/fasterwhisper"
	"github.com/nickheyer/icebath/internal/plugins/defaults/speech/gemini"
	"github.com/nickheyer/icebath/internal/plugins/defaults/speech/groq"
	"github.com/nickheyer/icebath/internal/plugins/defaults/speech/kittentts"
	"github.com/nickheyer/icebath/internal/plugins/defaults/speech/minimax"
	"github.com/nickheyer/icebath/internal/plugins/defaults/speech/mistral"
	"github.com/nickheyer/icebath/internal/plugins/defaults/speech/neutts"
	"github.com/nickheyer/icebath/internal/plugins/defaults/speech/openai"
	"github.com/nickheyer/icebath/internal/plugins/defaults/speech/openwakeword"
	"github.com/nickheyer/icebath/internal/plugins/defaults/speech/piper"
	"github.com/nickheyer/icebath/internal/plugins/defaults/speech/porcupine"
	"github.com/nickheyer/icebath/internal/plugins/defaults/speech/sherpa"
	"github.com/nickheyer/icebath/internal/plugins/defaults/speech/whispercpp"
	"github.com/nickheyer/icebath/internal/plugins/defaults/speech/xai"
)

// Built in services in the order tts.provider offers them, the first of a name winning so a program of yours under tts.providers or
// stt.providers never shadows one; the order of recognizers stt.provider offers and falls back through is their rank, and so is that of the
// wake engines
func Register(registries *plugins.Registries) {
	edge.Register(registries)
	elevenlabs.Register(registries)
	openai.Register(registries)
	minimax.Register(registries)
	xai.Register(registries)
	mistral.Register(registries)
	gemini.Register(registries)
	deepinfra.Register(registries)
	neutts.Register(registries)
	kittentts.Register(registries)
	piper.Register(registries)
	fasterwhisper.Register(registries)
	whispercpp.Register(registries)
	groq.Register(registries)
	commands.Register(registries)
	openwakeword.Register(registries)
	sherpa.Register(registries)
	porcupine.Register(registries)
}
