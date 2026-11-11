package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/media"
)

// Recorder double that records every call the voice mixin makes on it
type fakeVoiceRecorder struct {
	mu          sync.Mutex
	starts      int
	stopPath    string
	maxSeconds  *float64
	autostop    bool
	startSignal chan struct{}
}

func (r *fakeVoiceRecorder) Start(context.Context, func()) error {
	r.mu.Lock()
	r.starts++
	r.mu.Unlock()
	if r.startSignal != nil {
		select {
		case r.startSignal <- struct{}{}:
		default:
		}
	}
	return nil
}
func (r *fakeVoiceRecorder) Stop() (string, error)         { return r.stopPath, nil }
func (r *fakeVoiceRecorder) Cancel()                       {}
func (r *fakeVoiceRecorder) Shutdown()                     {}
func (r *fakeVoiceRecorder) IsRecording() bool             { return false }
func (r *fakeVoiceRecorder) ElapsedSeconds() float64       { return 0 }
func (r *fakeVoiceRecorder) CurrentRMS() int               { return 0 }
func (r *fakeVoiceRecorder) SupportsSilenceAutostop() bool { return r.autostop }
func (r *fakeVoiceRecorder) SetOnLivePartial(func(string)) {}
func (r *fakeVoiceRecorder) SetSilenceThreshold(int)       {}
func (r *fakeVoiceRecorder) SetSilenceDuration(float64)    {}
func (r *fakeVoiceRecorder) SetMaxRecordingSeconds(seconds float64) {
	r.mu.Lock()
	r.maxSeconds = &seconds
	r.mu.Unlock()
}

// Minimal CLI holding only the voice state the mixin reads, with prints recorded
func voiceCLI(t *testing.T) (*CLI, *printRecorder) {
	t.Helper()
	rec := recordPrints(t)
	c := bareCLI()
	c.voiceTTSDone = newSyncEvent()
	c.voiceTTSDone.Set()
	c.voiceBargeCapture = newSyncEvent()
	c.pendingInput = &inputQueue{}
	t.Cleanup(func() {
		c.voiceLock.Lock()
		c.voiceRecording = false
		c.voiceLock.Unlock()
	})
	return c, rec
}

// Writes config.yaml of the test home
func writeVoiceConfig(t *testing.T, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(os.Getenv("ICEBATH_HOME"), "config.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Voice requirements with audio and STT both available
func readyVoiceRequirements(context.Context) (media.VoiceRequirements, error) {
	return media.VoiceRequirements{Available: true, AudioAvailable: true, STTAvailable: true, Details: "OK", MissingPackages: []string{}}, nil
}

// Installs a recorder double and requirement, beep and level fakes for a recording start
func startRecordingFakes(t *testing.T, recorder *fakeVoiceRecorder) *int {
	t.Helper()
	beeps := 0
	swap(t, &voiceCheckRequirements, readyVoiceRequirements)
	swap(t, &voiceCreateAudioRecorder, func() media.VoiceRecorder { return recorder })
	swap(t, &voicePlayBeep, func(context.Context, int, float64, int) { beeps++ })
	return &beeps
}

// Transcriber fake answering every recording with the result
func transcribeAs(t *testing.T, result map[string]any) *[][2]string {
	t.Helper()
	calls := &[][2]string{}
	var mu sync.Mutex
	swap(t, &voiceTranscribeRecording, func(_ context.Context, path, model string) (map[string]any, error) {
		mu.Lock()
		*calls = append(*calls, [2]string{path, model})
		mu.Unlock()
		return result, nil
	})
	return calls
}

func TestHandleVoiceCommandRealOnCallsEnable(t *testing.T) {
	c, _ := voiceCLI(t)
	detects := 0
	swap(t, &voiceDetectAudioEnvironment, func() (media.AudioEnvironment, error) {
		detects++
		return media.AudioEnvironment{Available: false, Warnings: []string{}}, nil
	})
	c.handleVoiceCommand("/voice on")
	if detects != 1 {
		t.Fatalf("enable ran %d times", detects)
	}
}

func TestHandleVoiceCommandRealUnknownSubcommand(t *testing.T) {
	c, rec := voiceCLI(t)
	detects := 0
	swap(t, &voiceDetectAudioEnvironment, func() (media.AudioEnvironment, error) {
		detects++
		return media.AudioEnvironment{}, nil
	})
	c.voiceMode = true
	c.handleVoiceCommand("/voice foobar")
	if detects != 0 || !c.voiceMode {
		t.Fatalf("enable or disable ran: detects %d voice mode %v", detects, c.voiceMode)
	}
	found := false
	for _, line := range rec.plain() {
		if strings.Contains(line, "Unknown") || strings.Contains(line, "unknown") {
			found = true
		}
	}
	if !found {
		t.Fatalf("usage not printed %q", rec.plain())
	}
}

// Audio environment and requirement fakes that let voice mode enable
func enableVoiceFakes(t *testing.T) {
	t.Helper()
	swap(t, &voiceDetectAudioEnvironment, func() (media.AudioEnvironment, error) {
		return media.AudioEnvironment{Available: true, Warnings: []string{}}, nil
	})
	swap(t, &voiceCheckRequirements, func(context.Context) (media.VoiceRequirements, error) {
		return media.VoiceRequirements{Available: true, Details: "OK"}, nil
	})
}

func TestEnableVoiceModeRealSuccessSetsVoiceMode(t *testing.T) {
	c, _ := voiceCLI(t)
	enableVoiceFakes(t)
	writeVoiceConfig(t, "voice: {}\n")
	c.enableVoiceMode()
	if !c.voiceMode {
		t.Fatal("voice mode not enabled")
	}
}

func TestEnableVoiceModeRealConfigExceptionStillEnables(t *testing.T) {
	c, _ := voiceCLI(t)
	enableVoiceFakes(t)
	writeVoiceConfig(t, "voice: [unclosed\n  - : :\n")
	c.enableVoiceMode()
	if !c.voiceMode {
		t.Fatal("voice mode not enabled after a config failure")
	}
}

func TestVoiceBeepConfigRealBeepsCanBeDisabled(t *testing.T) {
	c, _ := voiceCLI(t)
	writeVoiceConfig(t, "voice:\n  beep_enabled: false\n")
	if c.voiceBeepsEnabled() {
		t.Fatal("beeps enabled")
	}
}

func TestVoiceBeepConfigRealStartRecordingSkipsBeepWhenDisabled(t *testing.T) {
	c, _ := voiceCLI(t)
	writeVoiceConfig(t, "voice:\n  beep_enabled: false\n  silence_threshold: 200\n  silence_duration: 3.0\n")
	recorder := &fakeVoiceRecorder{autostop: true}
	beeps := startRecordingFakes(t, recorder)
	if err := c.voiceStartRecording(); err != nil {
		t.Fatal(err)
	}
	if recorder.starts != 1 || *beeps != 0 {
		t.Fatalf("starts %d beeps %d", recorder.starts, *beeps)
	}
}

// Starts recording under the voice config and returns the recorder it configured
func startWithVoiceCfg(t *testing.T, voiceCfg string) *fakeVoiceRecorder {
	t.Helper()
	c, _ := voiceCLI(t)
	writeVoiceConfig(t, "voice:\n  "+voiceCfg+"\n")
	recorder := &fakeVoiceRecorder{autostop: true}
	startRecordingFakes(t, recorder)
	if err := c.voiceStartRecording(); err != nil {
		t.Fatal(err)
	}
	return recorder
}

func TestMaxRecordingSecondsConfigRealConfiguredCapReachesRecorder(t *testing.T) {
	recorder := startWithVoiceCfg(t, "max_recording_seconds: 45")
	if recorder.maxSeconds == nil || *recorder.maxSeconds != 45 {
		t.Fatalf("cap %v", recorder.maxSeconds)
	}
}

func TestMaxRecordingSecondsConfigRealBoolFallsBackToDocumentedDefault(t *testing.T) {
	recorder := startWithVoiceCfg(t, "max_recording_seconds: true")
	want := config.Defaults()["voice"].(map[string]any)["max_recording_seconds"]
	if recorder.maxSeconds == nil || *recorder.maxSeconds != float64(want.(int)) {
		t.Fatalf("cap %v want %v", recorder.maxSeconds, want)
	}
}

func TestDisableVoiceModeRealAllFlagsReset(t *testing.T) {
	c, _ := voiceCLI(t)
	swap(t, &voiceStopPlayback, func() {})
	c.voiceMode, c.voiceTTS, c.voiceContinuous = true, true, true
	c.disableVoiceMode()
	if c.voiceMode || c.voiceTTS || c.voiceContinuous {
		t.Fatalf("flags mode %v tts %v continuous %v", c.voiceMode, c.voiceTTS, c.voiceContinuous)
	}
}

func TestDisableVoiceModeRealStopPlaybackExceptionSwallowed(t *testing.T) {
	c, _ := voiceCLI(t)
	swap(t, &voiceStopPlayback, func() { panic("RuntimeError: boom") })
	c.voiceMode = true
	c.disableVoiceMode()
	if c.voiceMode {
		t.Fatal("voice mode still on")
	}
}

// TTS fake writing its result as the tool JSON and recording every requested output path
func ttsAs(t *testing.T, result func(outputPath string) map[string]any) *[]string {
	t.Helper()
	t.Setenv("TMPDIR", t.TempDir())
	paths := &[]string{}
	var mu sync.Mutex
	swap(t, &voiceTextToSpeech, func(_ context.Context, _, outputPath string) (string, error) {
		mu.Lock()
		*paths = append(*paths, outputPath)
		mu.Unlock()
		raw, _ := json.Marshal(result(outputPath))
		return string(raw), nil
	})
	return paths
}

func TestVoiceSpeakResponseRealAsyncSchedulingClearsDoneBeforeThreadStart(t *testing.T) {
	c, _ := voiceCLI(t)
	c.voiceTTS = true
	release := make(chan struct{})
	entered := make(chan bool, 1)
	t.Setenv("TMPDIR", t.TempDir())
	swap(t, &voiceTextToSpeech, func(context.Context, string, string) (string, error) {
		entered <- c.voiceTTSDone.IsSet()
		<-release
		return `{"success": false}`, nil
	})
	c.voiceSpeakResponseAsync("Hello")
	if c.voiceTTSDone.IsSet() {
		t.Fatal("done still set after scheduling")
	}
	if <-entered {
		t.Fatal("done set when the speak thread started")
	}
	close(release)
	if !c.voiceTTSDone.Wait(2 * time.Second) {
		t.Fatal("speak thread never marked done")
	}
}

func TestVoiceSpeakResponseRealEarlyReturnWhenTtsOff(t *testing.T) {
	c, _ := voiceCLI(t)
	calls := ttsAs(t, func(string) map[string]any { return map[string]any{"success": true} })
	c.voiceSpeakResponse("Hello")
	if len(*calls) != 0 {
		t.Fatalf("tts called %q", *calls)
	}
}

func TestVoiceSpeakResponseRealPlayAudioUsesReturnedFilePaths(t *testing.T) {
	c, _ := voiceCLI(t)
	c.voiceTTS = true
	calls := ttsAs(t, func(mp3 string) map[string]any {
		ogg := strings.TrimSuffix(mp3, filepath.Ext(mp3)) + ".ogg"
		if err := os.WriteFile(ogg, make([]byte, 1000), 0o600); err != nil {
			t.Error(err)
		}
		return map[string]any{"success": true, "file_path": ogg, "file_paths": []any{ogg}}
	})
	var played []string
	swap(t, &voicePlayAudioFile, func(p string) bool { played = append(played, p); return true })
	c.voiceSpeakResponse("Hello world")
	if len(*calls) != 1 {
		t.Fatalf("tts calls %q", *calls)
	}
	want := strings.TrimSuffix((*calls)[0], filepath.Ext((*calls)[0])) + ".ogg"
	if len(played) != 1 || played[0] != want {
		t.Fatalf("played %q want %q", played, want)
	}
}

func TestVoiceStopAndTranscribeRealGuardNotRecording(t *testing.T) {
	c, _ := voiceCLI(t)
	calls := transcribeAs(t, map[string]any{"success": true, "transcript": "x"})
	c.voiceStopAndTranscribe()
	if len(*calls) != 0 {
		t.Fatalf("transcribed %q", *calls)
	}
}

func TestVoiceStopAndTranscribeRealNoSpeechDetected(t *testing.T) {
	c, _ := voiceCLI(t)
	swap(t, &voicePlayBeep, func(context.Context, int, float64, int) {})
	c.voiceRecording, c.voiceRecorder = true, &fakeVoiceRecorder{}
	c.voiceStopAndTranscribe()
	if !c.pendingInput.Empty() {
		t.Fatal("input queued")
	}
}

func TestVoiceStopAndTranscribeRealSuccessfulTranscriptionQueuesInput(t *testing.T) {
	c, _ := voiceCLI(t)
	writeVoiceConfig(t, "stt: {}\n")
	swap(t, &voicePlayBeep, func(context.Context, int, float64, int) {})
	transcribeAs(t, map[string]any{"success": true, "transcript": "hello world"})
	wav := filepath.Join(t.TempDir(), "test.wav")
	if err := os.WriteFile(wav, []byte("RIFF"), 0o600); err != nil {
		t.Fatal(err)
	}
	c.voiceRecording, c.voiceRecorder = true, &fakeVoiceRecorder{stopPath: wav}
	c.voiceStopAndTranscribe()
	queued, ok := c.pendingInput.GetNoWait()
	msg, isVoice := queued.(*voiceInputMessage)
	if !ok || !isVoice || msg.Text != "hello world" {
		t.Fatalf("queued %#v", queued)
	}
	if _, err := os.Stat(wav); !os.IsNotExist(err) {
		t.Fatal("recording kept after a successful transcription")
	}
}

func TestVoiceStopAndTranscribeRealNonLocalSttKeepsGenericTranscribingStatus(t *testing.T) {
	c, rec := voiceCLI(t)
	writeVoiceConfig(t, "stt:\n  provider: openai\n  model: whisper-1\n")
	swap(t, &voicePlayBeep, func(context.Context, int, float64, int) {})
	calls := transcribeAs(t, map[string]any{"success": true, "transcript": "hello"})
	wav := filepath.Join(t.TempDir(), "test.wav")
	c.voiceRecording, c.voiceRecorder = true, &fakeVoiceRecorder{stopPath: wav}
	c.voiceStopAndTranscribe()
	transcribing := c.t("cli.voice.transcribing", nil)
	sawStatus := false
	for _, line := range rec.plain() {
		if strings.Contains(line, transcribing) {
			sawStatus = true
		}
		if strings.Contains(line, "Hugging Face") {
			t.Fatalf("local model status printed %q", line)
		}
	}
	if !sawStatus {
		t.Fatalf("no transcribing status in %q", rec.plain())
	}
	if len(*calls) != 1 || (*calls)[0] != [2]string{wav, "whisper-1"} {
		t.Fatalf("transcribe calls %q", *calls)
	}
}

// Barge capture recording written under a temp dir
func bargeWAV(t *testing.T) string {
	t.Helper()
	wav := filepath.Join(t.TempDir(), "barge.wav")
	if err := os.WriteFile(wav, []byte("RIFF"), 0o600); err != nil {
		t.Fatal(err)
	}
	return wav
}

func TestVoiceBargeCaptureSubmitTranscriptIsQueuedAndWavRemoved(t *testing.T) {
	c, _ := voiceCLI(t)
	c.voiceBargeCapture.Set()
	wav := bargeWAV(t)
	transcribeAs(t, map[string]any{"success": true, "transcript": "stop, do it differently"})
	c.voiceSubmitBargeUtterance(wav)
	queued, _ := c.pendingInput.GetNoWait()
	if msg, ok := queued.(*voiceInputMessage); !ok || msg.Text != "stop, do it differently" {
		t.Fatalf("queued %#v", queued)
	}
	if c.voiceBargeCapture.IsSet() {
		t.Fatal("barge capture still latched")
	}
	if _, err := os.Stat(wav); !os.IsNotExist(err) {
		t.Fatal("barge recording kept")
	}
}

// Recorder double whose start signals that continuous listening resumed
func restartRecorder(t *testing.T) chan struct{} {
	t.Helper()
	restarted := make(chan struct{}, 1)
	startRecordingFakes(t, &fakeVoiceRecorder{autostop: true, startSignal: restarted})
	return restarted
}

func TestVoiceBargeCaptureSubmitNoSpeechHandsMicBackWithoutQueueing(t *testing.T) {
	c, _ := voiceCLI(t)
	c.voiceMode, c.voiceContinuous = true, true
	c.voiceBargeCapture.Set()
	wav := bargeWAV(t)
	restarted := restartRecorder(t)
	transcribeAs(t, map[string]any{"success": true, "transcript": "", "no_speech": true})
	c.voiceSubmitBargeUtterance(wav)
	if !c.pendingInput.Empty() || c.voiceBargeCapture.IsSet() {
		t.Fatal("queued input or kept the barge latch")
	}
	select {
	case <-restarted:
	case <-time.After(2 * time.Second):
		t.Fatal("continuous mode did not resume listening")
	}
}

func TestVoiceBargeCaptureSubmitPlaybackPhaseEchoOfOwnTtsIsDropped(t *testing.T) {
	c, _ := voiceCLI(t)
	c.voiceMode, c.voiceContinuous = true, true
	c.voiceBargeCapture.Set()
	c.voiceBargePhase = "playback"
	c.voiceLastTTSText = "네, 방금도 제 답변이 그대로 다시 입력됐어요."
	wav := bargeWAV(t)
	restarted := restartRecorder(t)
	transcribeAs(t, map[string]any{"success": true, "transcript": "네 방금 네 방금도 제 답변이 그대로 다시 입력됐어요."})
	c.voiceSubmitBargeUtterance(wav)
	if !c.pendingInput.Empty() || c.voiceBargeCapture.IsSet() {
		t.Fatal("echo queued or barge latch kept")
	}
	select {
	case <-restarted:
	case <-time.After(2 * time.Second):
		t.Fatal("mic not handed back")
	}
}

func TestVoiceBargeCaptureSubmitPlaybackPhaseGenuineInterjectionIsStillQueued(t *testing.T) {
	c, _ := voiceCLI(t)
	c.voiceBargeCapture.Set()
	c.voiceBargePhase = "playback"
	c.voiceLastTTSText = "The weather today is sunny with a light breeze."
	transcribeAs(t, map[string]any{"success": true, "transcript": "actually can you check my calendar for tomorrow"})
	c.voiceSubmitBargeUtterance(bargeWAV(t))
	queued, _ := c.pendingInput.GetNoWait()
	if msg, ok := queued.(*voiceInputMessage); !ok || msg.Text != "actually can you check my calendar for tomorrow" {
		t.Fatalf("queued %#v", queued)
	}
}

func TestVoiceBargeCaptureSubmitGenerationPhaseTranscriptNotEchoChecked(t *testing.T) {
	c, _ := voiceCLI(t)
	c.voiceBargeCapture.Set()
	c.voiceBargePhase = "generation"
	c.voiceLastTTSText = "stop, do it differently"
	transcribeAs(t, map[string]any{"success": true, "transcript": "stop, do it differently"})
	c.voiceSubmitBargeUtterance(bargeWAV(t))
	queued, _ := c.pendingInput.GetNoWait()
	if msg, ok := queued.(*voiceInputMessage); !ok || msg.Text != "stop, do it differently" {
		t.Fatalf("queued %#v", queued)
	}
}

// Continuous voice CLI with the duplex listen, output probe and playback stop faked
func duplexCLI(t *testing.T, listen func(func() bool, media.DuplexOptions) string, voiceCfg string) *CLI {
	t.Helper()
	c, _ := voiceCLI(t)
	c.voiceMode, c.voiceContinuous = true, true
	if voiceCfg == "" {
		voiceCfg = "voice:\n  barge_in: true\n"
	}
	writeVoiceConfig(t, voiceCfg)
	swap(t, &voiceFullDuplexListen, listen)
	swap(t, &voiceIsAudioOutputActive, func() bool { return false })
	swap(t, &voiceStopPlayback, func() {})
	return c
}

func TestVoiceFullDuplexListenerGenerationTripInterruptsAgentAndSubmits(t *testing.T) {
	wav := bargeWAV(t)
	c := duplexCLI(t, func(_ func() bool, o media.DuplexOptions) string {
		o.OnTrigger("generation")
		return wav
	}, "")
	c.agentRunning = true
	c.agent = &agent.AIAgent{}
	pipeStop := newSyncEvent()
	c.voiceTTSStop = pipeStop
	transcribeAs(t, map[string]any{"success": true, "transcript": "actually wait"})
	c.voiceFullDuplexListener()
	if !c.agent.IsInterrupted() || !pipeStop.IsSet() {
		t.Fatalf("interrupted %v pipe stop %v", c.agent.IsInterrupted(), pipeStop.IsSet())
	}
	queued, _ := c.pendingInput.GetNoWait()
	if msg, ok := queued.(*voiceInputMessage); !ok || msg.Text != "actually wait" {
		t.Fatalf("queued %#v", queued)
	}
	if c.voiceBargeCapture.IsSet() {
		t.Fatal("barge latch kept")
	}
}

func TestVoiceFullDuplexListenerListenerArmsAtSubmitAndSurvivesIntoPlayback(t *testing.T) {
	probes := map[string]bool{}
	var c *CLI
	c = duplexCLI(t, func(shouldStop func() bool, _ media.DuplexOptions) string {
		probes["generation"] = shouldStop()
		c.agentRunning = false
		c.voiceTTSDone.Clear()
		probes["playback_pending"] = shouldStop()
		c.voiceTTSDone.Set()
		probes["done"] = shouldStop()
		return ""
	}, "")
	c.agentRunning = true
	c.voiceFullDuplexListener()
	if probes["generation"] || probes["playback_pending"] || !probes["done"] {
		t.Fatalf("probes %v", probes)
	}
}

func TestVoiceFullDuplexListenerStopPhraseMidGenerationInterruptsAndEndsChat(t *testing.T) {
	wav := bargeWAV(t)
	c := duplexCLI(t, func(_ func() bool, o media.DuplexOptions) string {
		o.OnTrigger("generation")
		return wav
	}, "voice:\n  barge_in: true\n  stop_phrases: [stop]\n")
	c.agentRunning = true
	c.agent = &agent.AIAgent{}
	transcribeAs(t, map[string]any{"success": true, "transcript": "stop"})
	c.voiceFullDuplexListener()
	if !c.agent.IsInterrupted() {
		t.Fatal("turn not interrupted at trip")
	}
	if c.voiceMode || c.voiceContinuous {
		t.Fatal("voice chat not ended by the stop phrase")
	}
	if !c.pendingInput.Empty() {
		t.Fatal("stop phrase reached the agent")
	}
}

func TestTypedVoiceStopTypedStopEndsVoiceChatWhenVoiceOn(t *testing.T) {
	c, rec := voiceCLI(t)
	writeVoiceConfig(t, "voice:\n  stop_phrases: [stop]\n")
	swap(t, &voiceStopPlayback, func() {})
	c.voiceMode = true
	if !c.typedVoiceStop("stop") {
		t.Fatal("typed stop passed through")
	}
	disabled := c.t("cli.voice.disabled", nil)
	count := 0
	for _, line := range rec.plain() {
		if strings.Contains(line, disabled) {
			count++
		}
	}
	if c.voiceMode || count != 1 {
		t.Fatalf("voice mode %v disable prints %d", c.voiceMode, count)
	}
}

func TestTypedVoiceStopLongerTypedMessagePassesThroughInVoiceMode(t *testing.T) {
	c, _ := voiceCLI(t)
	writeVoiceConfig(t, "voice:\n  stop_phrases: [stop]\n")
	c.voiceMode = true
	if c.typedVoiceStop("stop the docker container") {
		t.Fatal("longer message treated as a stop phrase")
	}
	if !c.voiceMode {
		t.Fatal("voice chat disabled")
	}
}

// Continuous voice CLI whose duplex listen signals each arming and whose speech synthesis fails fast
func fallbackSpeakCLI(t *testing.T, continuous bool) (*CLI, chan struct{}) {
	t.Helper()
	c, _ := voiceCLI(t)
	c.voiceMode, c.voiceTTS, c.voiceContinuous = true, true, continuous
	armed := make(chan struct{}, 4)
	swap(t, &voiceFullDuplexListen, func(func() bool, media.DuplexOptions) string {
		armed <- struct{}{}
		return ""
	})
	ttsAs(t, func(string) map[string]any { return map[string]any{"success": false} })
	return c, armed
}

func TestFallbackSpeakArmsBargeMonitorMonitorArmedInContinuousVoiceMode(t *testing.T) {
	c, armed := fallbackSpeakCLI(t, true)
	c.voiceSpeakResponseAsync("a reply")
	select {
	case <-armed:
	case <-time.After(5 * time.Second):
		t.Fatal("listener was never armed")
	}
	c.voiceTTSDone.Wait(2 * time.Second)
	if len(armed) != 0 {
		t.Fatalf("listener armed %d extra times", len(armed))
	}
}

func TestFallbackSpeakArmsBargeMonitorNoMonitorOutsideContinuousMode(t *testing.T) {
	c, armed := fallbackSpeakCLI(t, false)
	c.voiceSpeakResponseAsync("a reply")
	select {
	case <-armed:
		t.Fatal("listener armed outside continuous mode")
	case <-time.After(50 * time.Millisecond):
	}
	c.voiceTTSDone.Wait(2 * time.Second)
}
