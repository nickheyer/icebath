package media

// Whether the speech engine behind the sherpa wake word publishes a library for this platform
func SherpaOnnxAvailable() bool { return wakeSpeechSupported() }
