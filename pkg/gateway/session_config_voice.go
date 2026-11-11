package gateway

import "sync"

var (
	sessionVoiceCaptureStopMu sync.Mutex
	sessionVoiceCaptureStop   func(sessionID string)
)

// Installs the TUI server's session voice capture stopper, the module tui_gateway.server loading in this process
func RegisterSessionVoiceCaptureStop(stop func(sessionID string)) {
	sessionVoiceCaptureStopMu.Lock()
	defer sessionVoiceCaptureStopMu.Unlock()
	sessionVoiceCaptureStop = stop
}

// Ends the push to talk capture of a local session turning voice off, when the TUI server runs here
func LocalVoiceTransition(source *SessionSource, mode string) {
	sessionVoiceCaptureStopMu.Lock()
	stop := sessionVoiceCaptureStop
	sessionVoiceCaptureStopMu.Unlock()
	if mode == "off" && stop != nil && source != nil && source.Platform == PlatformLocal {
		stop(source.ChatID)
	}
}
