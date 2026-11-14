package hostinfo

import "testing"

func TestPinnedFactsAnswerUntilRestored(t *testing.T) {
	gpu, session := GPUClass(), InteractiveSession()
	restoreGPU := PinGPUClass("intel")
	restoreSession := PinInteractiveSession(!session)
	if GPUClass() != "intel" || InteractiveSession() == session {
		t.Fatalf("pinned %q %v", GPUClass(), InteractiveSession())
	}
	restoreSession()
	restoreGPU()
	if GPUClass() != gpu || InteractiveSession() != session {
		t.Fatalf("restored %q %v", GPUClass(), InteractiveSession())
	}
}
