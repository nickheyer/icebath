package media

import "github.com/nickheyer/icebath/pkg/plugins"

// Registers every media tool into the default registry and the media provider kinds plugins load into
func init() {
	registerImageGenerateTool()
	registerVideoGenerateTools()
	registerTextToSpeechTool()
	registerTranscriptionAPI()
	plugins.RegisterProviderRegistry("image_gen_provider", ImageGenScopedRegistry)
	plugins.RegisterProviderRegistry("video_gen_provider", VideoGenScopedRegistry)
	plugins.RegisterProviderRegistry("tts_provider", TTSScopedRegistry)
	plugins.RegisterProviderRegistry("transcription_provider", TranscriptionScopedRegistry)
}
