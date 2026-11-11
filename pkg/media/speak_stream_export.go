package media

// Configured TTS provider or the free default, nous answered by the OpenAI path
func TTSProviderName(ttsConfig map[string]any) string { return getTTSProvider(ttsConfig) }

// Input character cap of a TTS provider under the given TTS config
func ResolveMaxTextLength(provider string, ttsConfig map[string]any) int {
	return resolveMaxTextLength(provider, ttsConfig)
}

// Runs ffmpeg headless with captured output, its exit code, stdout and stderr
func FfmpegRun(ffmpeg string, args []string, timeout int) (returnCode int, stdout, stderr []byte, err error) {
	result, err := ffmpegRun(ffmpeg, args, timeout, false, true)
	if err != nil {
		return 0, nil, nil, err
	}
	return result.ReturnCode, result.Stdout, result.Stderr, nil
}

// Text held back for the next sentence, the chunker buf the idle flush inspects
func (c *SentenceChunker) Buffered() string { return c.buf }

// Default wait of a live transcription finalize in seconds
const StreamingFinalizeTimeout = streamingFinalizeTimeout
