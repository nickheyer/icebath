//go:build !darwin

package system

// Only macOS gates the microphone behind a media access prompt
func askForMicrophoneAccess() bool { return true }
