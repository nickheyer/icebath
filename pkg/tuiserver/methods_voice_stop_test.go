package tuiserver

import (
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Capture generation and owner of the voice capture state
func voiceCaptureState() (string, int) {
	voiceSidMu.Lock()
	defer voiceSidMu.Unlock()
	return voiceCaptureSID, voiceCaptureGen
}

func TestVoiceOffOnALocalSessionStopsOnlyItsOwnTuiCapture(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	voiceSidMu.Lock()
	savedSID := voiceCaptureSID
	voiceCaptureSID = "sid-capturing"
	voiceSidMu.Unlock()
	t.Cleanup(func() {
		voiceSidMu.Lock()
		voiceCaptureSID = savedSID
		voiceSidMu.Unlock()
	})
	_, before := voiceCaptureState()
	gateway.LocalVoiceTransition(&gateway.SessionSource{Platform: gateway.PlatformLocal, ChatID: "sid-other"}, "off")
	if _, gen := voiceCaptureState(); gen != before {
		t.Fatalf("another session's capture was stopped: generation %d -> %d", before, gen)
	}
	gateway.LocalVoiceTransition(&gateway.SessionSource{Platform: gateway.PlatformLocal, ChatID: "sid-capturing"}, "on")
	if _, gen := voiceCaptureState(); gen != before {
		t.Fatalf("voice on stopped the capture: generation %d -> %d", before, gen)
	}
	gateway.LocalVoiceTransition(&gateway.SessionSource{Platform: gateway.PlatformLocal, ChatID: "sid-capturing"}, "off")
	if _, gen := voiceCaptureState(); gen != before+1 {
		t.Fatalf("voice off left the capture running: generation %d -> %d", before, gen)
	}
}
