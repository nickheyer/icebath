package media

// Registers every media tool into the default registry once the remaining modules exist
func init() {
	registerImageGenerateTool()
	registerVideoGenerateTools()
	registerTextToSpeechTool()
	registerTranscriptionAPI()
}
