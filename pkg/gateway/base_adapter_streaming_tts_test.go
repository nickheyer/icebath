package gateway

import "testing"

func TestStreamingTTSSupportsStreamingTTSDefaultsFalse(t *testing.T) {
	if newBaseFake(PlatformTelegram).SupportsStreamingTTS("chat1", DefaultAudioFormat()) {
		t.Fatal("base adapter claims streaming TTS")
	}
}

func TestStreamingTTSDuplicateSuppressionIsPerTurn(t *testing.T) {
	a := newBaseFake(PlatformTelegram)
	one, ok1 := a.streamingTTSTurnKey("chat-1", 101, nil)
	two, ok2 := a.streamingTTSTurnKey("chat-1", 102, nil)
	if !ok1 || !ok2 || one == two {
		t.Fatalf("turn keys %q %q", one, two)
	}
	a.markStreamingTTSCompletedTurn("chat-1", 101, nil)
	if !StreamingTTSShouldSkipWholeFile(a.streamingTTSCompletedTurns, "chat-1", 101, nil) ||
		StreamingTTSShouldSkipWholeFile(a.streamingTTSCompletedTurns, "chat-1", 102, nil) {
		t.Fatal("whole file skip is not per turn")
	}
	if !a.streamingTTSTurnCompleted("chat-1", 101, nil) || a.streamingTTSTurnCompleted("chat-1", 102, nil) ||
		a.streamingTTSTurnCompleted("chat-2", 101, nil) {
		t.Fatal("completed turn lookup is not per chat and turn")
	}
}
