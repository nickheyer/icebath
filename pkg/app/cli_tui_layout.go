package app

// voiceRecorder is the audio recorder the voice prompt drives
type voiceRecorder interface {
	CurrentRMS() int
	Cancel()
	Shutdown()
}
