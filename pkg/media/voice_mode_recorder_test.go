package media

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pysounddevice"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
)

type fakeClock struct {
	mu  sync.Mutex
	now float64
}

func (c *fakeClock) advance(seconds float64) {
	c.mu.Lock()
	c.now += seconds
	c.mu.Unlock()
}

func withFakeClock(t *testing.T) *fakeClock {
	clock := &fakeClock{now: 1000.0}
	swapVoice(t, &voiceMonotonic, func() float64 {
		clock.mu.Lock()
		defer clock.mu.Unlock()
		return clock.now
	})
	return clock
}

func fullFrame(n int, v int16) []int16 {
	out := make([]int16, n)
	for i := range out {
		out[i] = v
	}
	return out
}

// The input callback the recorder handed to the last opened stream
func lastCallback(t *testing.T, sd *fakeSD) pysounddevice.InputCallback {
	t.Helper()
	sd.mu.Lock()
	defer sd.mu.Unlock()
	if len(sd.inputs) == 0 {
		t.Fatal("no input stream was opened")
	}
	return sd.inputs[len(sd.inputs)-1].callback
}

func startedRecorder(t *testing.T) (*AudioRecorder, *fakeSD) {
	withVoiceHome(t, "")
	sd := withFakeSD(t)
	r := NewAudioRecorder()
	return r, sd
}

func TestAudioRecorderStartRaisesWithoutAudioLibs(t *testing.T) {
	withAudioImportFailure(t, &audioImportError{msg: "no dynamic library loader"})
	err := NewAudioRecorder().Start(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "requires the PortAudio library") || !strings.Contains(err.Error(), "no dynamic library loader") || strings.Contains(err.Error(), "sounddevice") {
		t.Fatalf("err %v", err)
	}
}

func TestAudioRecorderStartOserrorPointsAtPortaudioNotPip(t *testing.T) {
	withAudioImportFailure(t, &pysounddevice.LibraryError{Msg: "PortAudio library not found"})
	swapVoice(t, &isTermuxEnvironment, func() bool { return false })
	err := NewAudioRecorder().Start(context.Background(), nil)
	if err == nil {
		t.Fatal("start succeeded")
	}
	msg := err.Error()
	if !strings.Contains(msg, "PortAudio system library not found") || !strings.Contains(msg, "libportaudio2") || strings.Contains(msg, "pip install") {
		t.Fatalf("message %q", msg)
	}
}

func TestAudioRecorderStartCreatesAndStartsStream(t *testing.T) {
	r, sd := startedRecorder(t)
	if err := r.Start(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if !r.IsRecording() || len(sd.inputs) != 1 || sd.inputs[0].starts != 1 {
		t.Fatalf("recording %v inputs %d", r.IsRecording(), len(sd.inputs))
	}
}

func TestAudioRecorderEnsureStreamRebuildsInactiveStream(t *testing.T) {
	r, sd := startedRecorder(t)
	inactive := &fakeStream{stopErr: errors.New("stream already stopped")}
	r.stream = inactive
	if err := r.ensureStream(); err != nil {
		t.Fatal(err)
	}
	if inactive.closes != 1 || len(sd.inputs) != 1 || sd.inputs[0].starts != 1 || r.stream != sd.inputs[0] {
		t.Fatalf("closes %d inputs %d", inactive.closes, len(sd.inputs))
	}
}

func TestAudioRecorderEnsureStreamRebuildsWhenLivenessProbeFails(t *testing.T) {
	r, sd := startedRecorder(t)
	broken := &fakeStream{activeErr: errors.New("CoreAudio stream state unavailable")}
	r.stream = broken
	if err := r.ensureStream(); err != nil {
		t.Fatal(err)
	}
	if broken.closes != 1 || len(sd.inputs) != 1 || sd.inputs[0].starts != 1 || r.stream != sd.inputs[0] {
		t.Fatalf("closes %d inputs %d", broken.closes, len(sd.inputs))
	}
}

func TestAudioRecorderStopStopWritesWavFile(t *testing.T) {
	r, _ := startedRecorder(t)
	withVoiceTempDir(t)
	if err := r.Start(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	r.frames = [][]int16{fullFrame(voiceSampleRate, 1000)}
	r.peakRMS = 1000
	r.mu.Unlock()
	path, err := r.Stop()
	if err != nil || path == "" || !strings.HasSuffix(path, ".wav") || r.IsRecording() {
		t.Fatalf("path %q err %v", path, err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if channels, width := int(raw[22])|int(raw[23])<<8, int(raw[34])|int(raw[35])<<8; channels != 1 || width != 16 {
		t.Fatalf("channels %d bits %d", channels, width)
	}
	if _, rate, err := readWAVFrames(path); err != nil || rate != voiceSampleRate {
		t.Fatalf("rate %d %v", rate, err)
	}
}

func TestAudioRecorderStopStopReturnsNoneForSilentRecording(t *testing.T) {
	r, _ := startedRecorder(t)
	withVoiceTempDir(t)
	if err := r.Start(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	r.frames = [][]int16{fullFrame(voiceSampleRate, 10)}
	r.peakRMS = 10
	r.mu.Unlock()
	if path, err := r.Stop(); path != "" || err != nil {
		t.Fatalf("path %q err %v", path, err)
	}
}

func TestAudioRecorderCancelCancelDiscardsFrames(t *testing.T) {
	r, sd := startedRecorder(t)
	if err := r.Start(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	r.frames = [][]int16{{1}}
	r.mu.Unlock()
	r.Cancel()
	if r.IsRecording() || r.frames != nil {
		t.Fatal("cancel kept the recording")
	}
	if sd.inputs[0].stops != 0 || sd.inputs[0].closes != 0 {
		t.Fatal("cancel must keep the stream open")
	}
}

func TestTranscribeRecordingFiltersWhisperHallucination(t *testing.T) {
	withVoiceHome(t, "")
	swapVoice(t, &voiceTranscribeAudio, func(context.Context, string, string, string) (map[string]any, error) {
		return map[string]any{"success": true, "transcript": "Thank you."}, nil
	})
	result, err := TranscribeRecording(context.Background(), "/tmp/test.wav", "")
	if err != nil || result["success"] != true || result["transcript"] != "" || result["filtered"] != true {
		t.Fatalf("%v %v", result, err)
	}
}

func TestTranscribeRecordingOtherErrorDoesNotTriggerChunk(t *testing.T) {
	withVoiceHome(t, "")
	wavPath := t.TempDir() + "/record.wav"
	if err := os.WriteFile(wavPath, wrapPCMAsWAV(int16Bytes(fullFrame(50000, 1000)), 16000, 1, 2), 0o600); err != nil {
		t.Fatal(err)
	}
	calls := 0
	swapVoice(t, &voiceTranscribeAudio, func(_ context.Context, _, model, _ string) (map[string]any, error) {
		calls++
		return map[string]any{"success": false, "transcript": "", "error": "STT is disabled in config.yaml"}, nil
	})
	result, err := TranscribeRecording(context.Background(), wavPath, "base")
	if err != nil || result["success"] != false || !strings.Contains(result["error"].(string), "STT is disabled") || calls != 1 {
		t.Fatalf("%v %v calls %d", result, err, calls)
	}
}

func TestWhisperHallucinationFilterKnownHallucinations(t *testing.T) {
	for _, text := range []string{"Thank you.", "thank you", "Thanks for watching.", "Bye.", "  Thank you.  ", "you"} {
		if !isWhisperHallucination(text) {
			t.Fatalf("%q not filtered", text)
		}
	}
}

func TestWhisperHallucinationFilterRealSpeechNotFiltered(t *testing.T) {
	for _, text := range []string{"Hello, how are you?", "Thank you for your help with the project."} {
		if isWhisperHallucination(text) {
			t.Fatalf("%q filtered", text)
		}
	}
}

func TestSilenceDetectionSilenceCallbackFiresAfterSpeechThenSilence(t *testing.T) {
	r, sd := startedRecorder(t)
	clock := withFakeClock(t)
	r.silenceDuration = 0.05
	r.minSpeechDuration = 0.05
	fired := &pythread.Event{}
	if err := r.Start(context.Background(), fired.Set); err != nil {
		t.Fatal(err)
	}
	cb := lastCallback(t, sd)
	cb(fullFrame(1600, 5000), 1600, 0)
	clock.advance(0.06)
	cb(fullFrame(1600, 5000), 1600, 0)
	if !r.hasSpoken {
		t.Fatal("speech not confirmed")
	}
	cb(make([]int16, 1600), 1600, 0)
	clock.advance(0.06)
	cb(make([]int16, 1600), 1600, 0)
	if !fired.Wait(5.0) {
		t.Fatal("silence callback never fired")
	}
	r.Cancel()
}

func TestSilenceDetectionMicroPauseToleranceDuringSpeech(t *testing.T) {
	r, sd := startedRecorder(t)
	clock := withFakeClock(t)
	r.silenceDuration = 0.05
	r.minSpeechDuration = 0.15
	r.maxDipTolerance = 0.1
	fired := &pythread.Event{}
	if err := r.Start(context.Background(), fired.Set); err != nil {
		t.Fatal(err)
	}
	cb := lastCallback(t, sd)
	cb(fullFrame(1600, 5000), 1600, 0)
	clock.advance(0.05)
	cb(fullFrame(1600, 50), 1600, 0)
	clock.advance(0.05)
	cb(fullFrame(1600, 5000), 1600, 0)
	if !(r.speechStart > 0) {
		t.Fatal("speech start should be preserved across brief dips")
	}
	clock.advance(0.06)
	cb(fullFrame(1600, 5000), 1600, 0)
	if !r.hasSpoken {
		t.Fatal("speech should be confirmed after tolerating micro-pause")
	}
	r.Cancel()
}

func TestMaxRecordingCapCapFiresOneShotCallbackDuringContinuousSpeech(t *testing.T) {
	r, sd := startedRecorder(t)
	clock := withFakeClock(t)
	r.maxRecordingSeconds = 0.1
	r.silenceDuration = 60.0
	r.maxWait = 60.0
	var mu sync.Mutex
	fires := 0
	fired := &pythread.Event{}
	if err := r.Start(context.Background(), func() {
		mu.Lock()
		fires++
		mu.Unlock()
		fired.Set()
	}); err != nil {
		t.Fatal(err)
	}
	cb := lastCallback(t, sd)
	cb(fullFrame(1600, 5000), 1600, 0)
	if fired.IsSet() {
		t.Fatal("cap must not fire before the limit elapses")
	}
	clock.advance(0.12)
	cb(fullFrame(1600, 5000), 1600, 0)
	if !fired.Wait(5.0) {
		t.Fatal("cap never fired")
	}
	r.mu.Lock()
	cleared := r.onSilenceStop == nil
	r.mu.Unlock()
	if !cleared {
		t.Fatal("callback must be one shot")
	}
	cb(fullFrame(1600, 5000), 1600, 0)
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if fires != 1 {
		t.Fatalf("fired %d times", fires)
	}
	r.Cancel()
}

func TestMaxRecordingCapDisabledCapNeverFiresOnDuration(t *testing.T) {
	r, sd := startedRecorder(t)
	clock := withFakeClock(t)
	r.maxRecordingSeconds = 0.0
	r.silenceDuration = 60.0
	r.maxWait = 60.0
	fired := &pythread.Event{}
	if err := r.Start(context.Background(), fired.Set); err != nil {
		t.Fatal(err)
	}
	cb := lastCallback(t, sd)
	cb(fullFrame(1600, 5000), 1600, 0)
	clock.advance(0.12)
	cb(fullFrame(1600, 5000), 1600, 0)
	if fired.Wait(0.1) {
		t.Fatal("disabled cap fired")
	}
	r.Cancel()
}

func TestContinuousModeFlowContinuousRestartOnNoSpeech(t *testing.T) {
	r, sd := startedRecorder(t)
	withVoiceTempDir(t)
	if err := r.Start(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	cb := lastCallback(t, sd)
	for range 10 {
		cb(fullFrame(1600, 10), 1600, 0)
	}
	if path, err := r.Stop(); path != "" || err != nil {
		t.Fatalf("silence kept %q %v", path, err)
	}
	if err := r.Start(context.Background(), nil); err != nil || !r.IsRecording() {
		t.Fatalf("restart %v", err)
	}
	cb = lastCallback(t, sd)
	for range 10 {
		cb(fullFrame(1600, 5000), 1600, 0)
	}
	if path, err := r.Stop(); path == "" || err != nil {
		t.Fatalf("speech dropped %v", err)
	}
	r.Cancel()
}

func TestAudioLevelIndicatorPeakRmsTracksMaximum(t *testing.T) {
	r, sd := startedRecorder(t)
	if err := r.Start(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	cb := lastCallback(t, sd)
	for _, v := range []int16{100, 8000, 500, 3000} {
		cb(fullFrame(1600, v), 1600, 0)
	}
	if r.peakRMS != 8000 || r.CurrentRMS() != 3000 {
		t.Fatalf("peak %d current %d", r.peakRMS, r.CurrentRMS())
	}
	r.Cancel()
}

func TestConfigurableSilenceParamsCustomThresholdAndDuration(t *testing.T) {
	r, sd := startedRecorder(t)
	clock := withFakeClock(t)
	r.SetSilenceThreshold(5000)
	r.SetSilenceDuration(0.05)
	r.minSpeechDuration = 0.05
	fired := &pythread.Event{}
	if err := r.Start(context.Background(), fired.Set); err != nil {
		t.Fatal(err)
	}
	cb := lastCallback(t, sd)
	for range 5 {
		cb(fullFrame(1600, 1000), 1600, 0)
		clock.advance(0.02)
	}
	if r.hasSpoken || fired.Wait(0.2) {
		t.Fatal("moderate level must stay below the custom threshold")
	}
	cb(fullFrame(1600, 8000), 1600, 0)
	clock.advance(0.06)
	cb(fullFrame(1600, 8000), 1600, 0)
	if !r.hasSpoken {
		t.Fatal("loud speech not confirmed")
	}
	r.Cancel()
}

func TestStreamLeakOnStartFailureStreamClosedOnStartFailure(t *testing.T) {
	r, sd := startedRecorder(t)
	busy := &fakeStream{startErr: errors.New("Audio device busy")}
	sd.inputFactory = func(pysounddevice.StreamParams, pysounddevice.InputCallback) (pysounddevice.InputStream, error) {
		return busy, nil
	}
	err := r.ensureStream()
	if err == nil || !strings.Contains(err.Error(), "Failed to open audio input stream") || busy.closes != 1 {
		t.Fatalf("err %v closes %d", err, busy.closes)
	}
}

func TestStreamStartTimeoutRetryTimedOutStartRetriesOnceAndSucceeds(t *testing.T) {
	r, sd := startedRecorder(t)
	cold := &fakeStream{startErr: errors.New("Error starting stream: Wait timed out [PaErrorCode -9987]")}
	warm := &fakeStream{}
	streams := []*fakeStream{cold, warm}
	sd.inputFactory = func(pysounddevice.StreamParams, pysounddevice.InputCallback) (pysounddevice.InputStream, error) {
		s := streams[0]
		streams = streams[1:]
		return s, nil
	}
	if err := r.ensureStream(); err != nil {
		t.Fatal(err)
	}
	if r.stream != warm || cold.closes != 1 || warm.closes != 0 {
		t.Fatalf("cold closes %d warm closes %d", cold.closes, warm.closes)
	}
}

func TestStreamStartTimeoutRetryPersistentTimeoutRaisesAfterSecondAttempt(t *testing.T) {
	r, sd := startedRecorder(t)
	stuck := &fakeStream{startErr: errors.New("Wait timed out [PaErrorCode -9987]")}
	opens := 0
	sd.inputFactory = func(pysounddevice.StreamParams, pysounddevice.InputCallback) (pysounddevice.InputStream, error) {
		opens++
		return stuck, nil
	}
	err := r.ensureStream()
	if err == nil || !strings.Contains(err.Error(), "Wait timed out") || opens != 2 || stuck.closes != 2 || r.stream != nil {
		t.Fatalf("err %v opens %d closes %d", err, opens, stuck.closes)
	}
}

func TestDefaultInputSamplerateWavWrittenAtCaptureRate(t *testing.T) {
	r, sd := startedRecorder(t)
	withVoiceTempDir(t)
	sd.inputDevice = pysounddevice.Device{DefaultSamplerate: 48000.0}
	if err := r.Start(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	r.frames = [][]int16{fullFrame(48000, 1000)}
	r.peakRMS = 1000
	r.mu.Unlock()
	path, err := r.Stop()
	if err != nil || path == "" {
		t.Fatalf("path %q %v", path, err)
	}
	if _, rate, err := readWAVFrames(path); err != nil || rate != 48000 {
		t.Fatalf("rate %d %v", rate, err)
	}
}

func TestCapEnforcedWhenSet(t *testing.T) {
	r := NewAudioRecorder()
	r.maxRecordingSeconds = 120
	if r.maxDurationReached(119.9) || !r.maxDurationReached(120.0) || !r.maxDurationReached(300.0) {
		t.Fatal("cap not enforced at 120 s")
	}
}

// Arms a recorder as if a capture of frames with the peak level just ended
func armRecorder(r *AudioRecorder, frames [][]int16, peak int) {
	r.mu.Lock()
	r.recording, r.frames, r.peakRMS = true, frames, peak
	r.mu.Unlock()
}

func TestStopUsesConfiguredSilenceThresholdStopKeepsRecordingAboveConfiguredThreshold(t *testing.T) {
	withVoiceTempDir(t)
	r := NewAudioRecorder()
	r.SetSilenceThreshold(80)
	armRecorder(r, [][]int16{make([]int16, 16000)}, 160)
	if path, err := r.Stop(); path == "" || err != nil {
		t.Fatalf("recording above the configured threshold discarded: %v", err)
	}
}

func TestStopUsesConfiguredSilenceThresholdStopDiscardsRecordingBelowConfiguredThreshold(t *testing.T) {
	withVoiceTempDir(t)
	r := NewAudioRecorder()
	r.SetSilenceThreshold(80)
	armRecorder(r, [][]int16{make([]int16, 16000)}, 40)
	if path, _ := r.Stop(); path != "" {
		t.Fatal("recording below the configured threshold kept")
	}
}

func TestStopUsesConfiguredSilenceThresholdStopThresholdTracksReconfiguredThresholdBetweenRecordings(t *testing.T) {
	withVoiceTempDir(t)
	r := NewAudioRecorder()
	r.SetSilenceThreshold(80)
	armRecorder(r, [][]int16{make([]int16, 16000)}, 70)
	if path, _ := r.Stop(); path != "" {
		t.Fatal("below 80 must be discarded")
	}
	r.SetSilenceThreshold(60)
	armRecorder(r, [][]int16{make([]int16, 16000)}, 70)
	if path, _ := r.Stop(); path == "" {
		t.Fatal("above 60 must be kept")
	}
}

func TestStopUsesConfiguredSilenceThresholdStopStillDiscardsBelowDefaultThresholdWithoutConfig(t *testing.T) {
	withVoiceTempDir(t)
	r := NewAudioRecorder()
	if r.silenceThreshold != silenceRMSThreshold {
		t.Fatalf("default threshold %v", r.silenceThreshold)
	}
	armRecorder(r, [][]int16{make([]int16, 16000)}, 160)
	if path, _ := r.Stop(); path != "" {
		t.Fatal("below the default threshold kept")
	}
}

func transcribeWithStopPhrases(t *testing.T, text string, phrases []string) map[string]any {
	withStopPhrases(t, phrases)
	swapVoice(t, &voiceTranscribeAudio, func(context.Context, string, string, string) (map[string]any, error) {
		return map[string]any{"success": true, "transcript": text}, nil
	})
	result, err := TranscribeRecording(context.Background(), "/tmp/fake.wav", "")
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestStopPhraseSurvivesHallucinationFilterConfiguredStopPhraseSurvivesBlocklist(t *testing.T) {
	result := transcribeWithStopPhrases(t, "Bye.", []string{"stop", "bye"})
	if result["success"] != true || result["transcript"] != "Bye." || pyTruthy(result["filtered"]) {
		t.Fatalf("%v", result)
	}
}

func TestStopPhraseSurvivesHallucinationFilterUnconfiguredHallucinationStillFiltered(t *testing.T) {
	result := transcribeWithStopPhrases(t, "Bye.", []string{"stop"})
	if result["success"] != true || result["transcript"] != "" || result["filtered"] != true {
		t.Fatalf("%v", result)
	}
}

func TestStopPhraseSurvivesHallucinationFilterDefaultStopSurvivesRepeatRegexAdjacentPhrases(t *testing.T) {
	result := transcribeWithStopPhrases(t, "Stop.", []string{"stop"})
	if result["transcript"] != "Stop." || pyTruthy(result["filtered"]) {
		t.Fatalf("%v", result)
	}
}

func TestVoiceCaptureHintsNameTheNativeLibraryTheBinaryLoads(t *testing.T) {
	swapVoice(t, &isTermuxEnvironment, func() bool { return true })
	termux := voiceCaptureInstallHint(&audioImportError{msg: "no loader"})
	if !strings.Contains(termux, "pkg install termux-api") || strings.Contains(termux, "pip") || strings.Contains(termux, "numpy") {
		t.Fatalf("termux hint %q", termux)
	}
	swapVoice(t, &isTermuxEnvironment, func() bool { return false })
	if got := voiceCaptureInstallHint(&audioImportError{msg: "no loader"}); got != "no loader" {
		t.Fatalf("hint %q", got)
	}
	missing := portaudioMissingMessage()
	for _, want := range []string{"libportaudio2", "brew install portaudio", "portaudio.dll", "libportaudio64bit.dll"} {
		if !strings.Contains(missing, want) {
			t.Fatalf("missing %q in %q", want, missing)
		}
	}
}
