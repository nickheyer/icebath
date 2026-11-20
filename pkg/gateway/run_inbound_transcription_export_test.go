package gateway

import "testing"

func TestReplaceMessageEventDropsPendingSTTMemo(t *testing.T) {
	transcript := "hello"
	event := &MessageEvent{Text: "voice", Source: &SessionSource{Platform: PlatformTelegram, ChatID: "1"}}
	event.PendingSTTClips()["/tmp/voice.ogg"] = STTClip{Transcript: &transcript}
	event.SetPendingSTTEchoed(2)
	copied := replaceMessageEvent(event)
	if copied.pendingSTTClips != nil || copied.pendingSTTEchoed != 0 {
		t.Fatalf("copy kept the pending stt memo %v %d", copied.pendingSTTClips, copied.pendingSTTEchoed)
	}
	if len(event.PendingSTTClips()) != 1 || event.PendingSTTEchoed() != 2 {
		t.Fatalf("original lost its memo")
	}
	if copied.Text != "voice" || copied.Source == event.Source || copied.Source.ChatID != "1" {
		t.Fatalf("copy fields %q %v", copied.Text, copied.Source)
	}
}
