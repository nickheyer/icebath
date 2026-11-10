package media

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pythread"
)

func resetThinking(t *testing.T) {
	StopThinkingSound()
	audioOutputMu.Lock()
	audioOutputActive = 0
	audioOutputMu.Unlock()
	t.Cleanup(StopThinkingSound)
}

func currentThinkingStop() *pythread.Event {
	thinkingMu.Lock()
	defer thinkingMu.Unlock()
	return thinkingStop
}

func TestConfigGateStartRefusesWhenDisabled(t *testing.T) {
	resetThinking(t)
	withVoiceHome(t, "voice:\n  thinking_sound: false\n")
	if StartThinkingSound(context.Background(), nil) {
		t.Fatal("start must refuse when voice.thinking_sound is false")
	}
	if currentThinkingStop() != nil {
		t.Fatal("no loop may be registered")
	}
}

func TestBlipSynthesisBlipIsInt16LowVolume(t *testing.T) {
	withVoiceHome(t, "")
	blip := synthThinkingBlip(context.Background(), 392.0)
	if len(blip) != int(voiceSampleRate*0.16) {
		t.Fatalf("length %d", len(blip))
	}
	peak := 0
	for _, v := range blip {
		peak = max(peak, abs(int(v)))
	}
	volume := 0.3
	if peak > int(volume*0.5*32767)+1 || peak <= 0 {
		t.Fatalf("peak %d", peak)
	}
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func TestBlipSynthesisNoClickSmoothAttack(t *testing.T) {
	withVoiceHome(t, "")
	if blip := synthThinkingBlip(context.Background(), 392.0); abs(int(blip[0])) >= 200 {
		t.Fatalf("first sample %d", blip[0])
	}
}

func TestLoopLifecycleLoopPlaysBlipsAndStopsInstantly(t *testing.T) {
	resetThinking(t)
	withVoiceHome(t, "")
	swapVoice(t, &soundDeviceOutputAllowed, func() bool { return true })
	sd := withFakeSD(t)
	stop := &pythread.Event{}
	done := make(chan struct{})
	go func() {
		thinkingSoundLoop(context.Background(), stop, nil)
		close(done)
	}()
	deadline := time.Now().Add(3 * time.Second)
	for sd.playCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	stop.Set()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("loop still alive after stop")
	}
	if sd.playCount() == 0 {
		t.Fatal("loop never played a blip")
	}
}

func TestLoopLifecycleStartIsIdempotentAndStopClears(t *testing.T) {
	resetThinking(t)
	withVoiceHome(t, "")
	gated := make(chan struct{}, 1)
	swapVoice(t, &soundDeviceOutputAllowed, func() bool {
		gated <- struct{}{}
		return false
	})
	if !StartThinkingSound(context.Background(), nil) {
		t.Fatal("first start refused")
	}
	first := currentThinkingStop()
	if !StartThinkingSound(context.Background(), nil) || currentThinkingStop() != first {
		t.Fatal("second start must reuse the running loop")
	}
	StopThinkingSound()
	if currentThinkingStop() != nil || !first.IsSet() {
		t.Fatal("stop must clear and signal the loop")
	}
	StopThinkingSound()
	<-gated
}

func TestAudioOutputRefcountRefcountTracksNestedPlayback(t *testing.T) {
	resetThinking(t)
	if IsAudioOutputActive() {
		t.Fatal("idle output reported active")
	}
	MarkAudioOutputActive(true)
	MarkAudioOutputActive(true)
	if !IsAudioOutputActive() {
		t.Fatal("two playbacks not counted")
	}
	MarkAudioOutputActive(false)
	if !IsAudioOutputActive() {
		t.Fatal("nested playback dropped early")
	}
	MarkAudioOutputActive(false)
	if IsAudioOutputActive() {
		t.Fatal("count did not return to zero")
	}
	MarkAudioOutputActive(false)
	if IsAudioOutputActive() {
		t.Fatal("count went negative")
	}
}

func TestAudioOutputRefcountPlayAudioFileBracketsRefcount(t *testing.T) {
	resetThinking(t)
	var seen []bool
	swapVoice(t, &playAudioFileImpl, func(string) bool {
		seen = append(seen, IsAudioOutputActive())
		return true
	})
	PlayAudioFile(t.TempDir() + "/x.wav")
	if !slices.Equal(seen, []bool{true}) || IsAudioOutputActive() {
		t.Fatalf("seen %v", seen)
	}
}
