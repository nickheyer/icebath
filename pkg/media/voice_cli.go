package media

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/tools"
)

func voiceCLILogger() *slog.Logger { return slog.Default().With("logger", "hermes_cli.voice") }

// Callbacks of the active continuous loop
type ContinuousCallbacks struct {
	OnTranscript  func(string)
	OnStatus      func(string)
	OnSilentLimit func()
	OnStopPhrase  func(string)
}

// Options of one continuous voice loop
type ContinuousOptions struct {
	ContinuousCallbacks
	SilenceThreshold    float64
	SilenceDuration     float64
	AutoRestart         bool
	MaxRecordingSeconds any
	OnPartial           func(string)
}

// Silent cycles in a row that end a continuous loop
const continuousNoSpeechLimit = 3

var (
	pttRecorderMu sync.Mutex
	pttRecorder   VoiceRecorder

	continuousMu            sync.Mutex
	continuousActive        bool
	continuousStopping      bool
	continuousAutoRestart   = true
	continuousRecorder      VoiceRecorder
	continuousCallbacks     ContinuousCallbacks
	continuousNoSpeechCount int

	ttsPlaying     = setEvent()
	voiceBusyMu    sync.Mutex
	voiceBusyProbe func() bool
)

// Event already set, the not playing state of the TTS guard
func setEvent() *pythread.Event {
	e := &pythread.Event{}
	e.Set()
	return e
}

// Writes a voice breadcrumb to stderr when ICEBATH_VOICE_DEBUG is 1
func voiceDebug(msg string) {
	if strings.TrimSpace(os.Getenv("ICEBATH_VOICE_DEBUG")) == "1" {
		fmt.Fprintf(os.Stderr, "[voice] %s\n", msg)
	}
}

// Whether voice.beep_enabled allows cue beeps, true by default and on any read failure
func voiceBeepsEnabled(ctx context.Context) (enabled bool) {
	defer func() {
		if recover() != nil {
			enabled = true
		}
	}()
	tree, err := config.LoadContext(ctx, home.Dir(ctx), config.Lookup(os.LookupEnv))
	if err != nil {
		return true
	}
	voiceCfg, isDict := tree["voice"].(map[string]any)
	if !isDict {
		return true
	}
	raw, present := voiceCfg["beep_enabled"]
	if !present || raw == nil {
		return true
	}
	if text, isText := raw.(string); isText {
		switch pystr.Lower(pystr.Strip(text)) {
		case "1", "true", "yes", "on":
			return true
		}
		return false
	}
	return pyrepr.Truthy(raw)
}

// Plays a cue beep when beeps are enabled, a failure only logged
func voicePlayBeep(ctx context.Context, frequency, count int) {
	if !voiceBeepsEnabled(ctx) {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			voiceDebug(fmt.Sprintf("beep %dHz failed: %v", frequency, r))
		}
	}()
	PlayBeep(ctx, frequency, 0.12, count)
}

// Calls an optional callback, swallowing its panic and logging it when a warning format is given
func voiceSafeCall(warn string, call func()) {
	defer func() {
		if r := recover(); r != nil && warn != "" {
			voiceCLILogger().Warn(fmt.Sprintf(warn, r))
		}
	}()
	call()
}

// Transcribes and removes a capture, the cleaned transcript or empty for silence, hallucinations and failures
func transcribeWav(ctx context.Context, wavPath, failMsg, debugPrefix string) (text string) {
	defer func() {
		if st, err := os.Stat(wavPath); err == nil && st.Mode().IsRegular() {
			_ = os.Remove(wavPath)
		}
	}()
	result, err := TranscribeRecording(ctx, wavPath, "")
	if err != nil {
		voiceCLILogger().Warn(fmt.Sprintf(failMsg, err))
		if debugPrefix != "" {
			voiceDebug(fmt.Sprintf("%s: transcribe raised %s", debugPrefix, err))
		}
		return ""
	}
	success := pyrepr.Truthy(result["success"])
	transcript := ""
	if raw := result["transcript"]; pyrepr.Truthy(raw) {
		transcript = pystr.Strip(pyrepr.Str(raw))
	}
	if debugPrefix != "" {
		voiceDebug(fmt.Sprintf("%s: transcribe -> success=%s text=%s err=%s", debugPrefix, pyrepr.Repr(success), pyrepr.Repr(transcript), pyrepr.Repr(result["error"])))
	}
	if success && transcript != "" && !isWhisperHallucination(transcript) {
		return transcript
	}
	return ""
}

// Marks the continuous loop inactive and reports idle
func voiceDeactivate(onStatus func(string)) {
	continuousMu.Lock()
	continuousActive = false
	continuousMu.Unlock()
	if onStatus != nil {
		voiceSafeCall("", func() { onStatus("idle") })
	}
}

// SetVoiceBusyProbe registers the agent busy probe that holds silent cycles, nil clearing it
func SetVoiceBusyProbe(probe func() bool) {
	voiceBusyMu.Lock()
	voiceBusyProbe = probe
	voiceBusyMu.Unlock()
}

// Whether silent cycles must not count, TTS playing or the agent mid turn, false when the probe fails
func voiceActivityHeld() (held bool) {
	if !ttsPlaying.IsSet() {
		return true
	}
	voiceBusyMu.Lock()
	probe := voiceBusyProbe
	voiceBusyMu.Unlock()
	defer func() {
		if recover() != nil {
			held = false
		}
	}()
	return probe != nil && probe()
}

// Deliverable transcript of a finished capture, whether it was a bare stop phrase and that phrase
func turnTranscript(ctx context.Context, wavPath, failMsg, where, tail string, trace bool) (string, bool, string) {
	transcript := ""
	if wavPath != "" {
		prefix := ""
		if trace {
			prefix = where
		}
		transcript = transcribeWav(ctx, wavPath, failMsg, prefix)
	}
	if transcript == "" || !IsVoiceStopPhrase(transcript) {
		return transcript, false, ""
	}
	voiceDebug(fmt.Sprintf("%s: stop phrase %s — %s", where, pyrepr.Repr(transcript), tail))
	return "", true, transcript
}

// Fires the stop phrase signal when wired, else the silent limit fallback
func signalHalt(stopPhrase bool, stopText string, cb ContinuousCallbacks) {
	if stopPhrase && cb.OnStopPhrase != nil {
		voiceSafeCall("", func() { cb.OnStopPhrase(stopText) })
	} else if cb.OnSilentLimit != nil {
		voiceSafeCall("", cb.OnSilentLimit)
	}
}

// Updates the no speech counter under the loop lock, reporting whether the limit was hit and the count
func tallySilence(spoke, held bool, where string) (bool, int) {
	if spoke {
		continuousNoSpeechCount = 0
		return false, 0
	}
	if held {
		voiceDebug(where + ": silent cycle ignored (agent busy or TTS playing)")
		return false, continuousNoSpeechCount
	}
	continuousNoSpeechCount++
	count := continuousNoSpeechCount
	if count >= continuousNoSpeechLimit {
		continuousNoSpeechCount = 0
		return true, count
	}
	return false, count
}

// StartRecording begins a push to talk capture from the default input device
func StartRecording(ctx context.Context) error {
	pttRecorderMu.Lock()
	defer pttRecorderMu.Unlock()
	if pttRecorder != nil && pttRecorder.IsRecording() {
		return nil
	}
	rec := CreateAudioRecorder()
	if err := rec.Start(ctx, nil); err != nil {
		return err
	}
	pttRecorder = rec
	return nil
}

// StopAndTranscribe stops the push to talk capture and returns its transcript, empty when none
func StopAndTranscribe(ctx context.Context) (string, error) {
	pttRecorderMu.Lock()
	rec := pttRecorder
	pttRecorder = nil
	pttRecorderMu.Unlock()
	if rec == nil {
		return "", nil
	}
	wavPath, err := rec.Stop()
	if err != nil {
		return "", err
	}
	if wavPath == "" {
		return "", nil
	}
	return transcribeWav(ctx, wavPath, "voice transcription failed: %s", ""), nil
}

// StartContinuous starts the VAD driven loop, false while a stop is still transcribing
func StartContinuous(ctx context.Context, o ContinuousOptions) (bool, error) {
	continuousMu.Lock()
	if continuousActive {
		continuousMu.Unlock()
		voiceDebug("start_continuous: already active — no-op")
		return true, nil
	}
	if continuousStopping {
		continuousMu.Unlock()
		voiceDebug("start_continuous: stop/transcribe in progress — busy")
		return false, nil
	}
	continuousActive = true
	continuousAutoRestart = o.AutoRestart
	continuousCallbacks = o.ContinuousCallbacks
	if o.AutoRestart {
		continuousNoSpeechCount = 0
	}
	if continuousRecorder == nil {
		continuousRecorder = CreateAudioRecorder()
	}
	rec := continuousRecorder
	rec.SetOnLivePartial(o.OnPartial)
	rec.SetSilenceThreshold(o.SilenceThreshold)
	rec.SetSilenceDuration(o.SilenceDuration)
	maxSeconds := 0.0
	switch n := o.MaxRecordingSeconds.(type) {
	case int:
		maxSeconds = float64(n)
	case int64:
		maxSeconds = float64(n)
	case float64:
		maxSeconds = n
	}
	rec.SetMaxRecordingSeconds(max(0, maxSeconds))
	continuousMu.Unlock()
	voiceDebug(fmt.Sprintf("start_continuous: begin (threshold=%s, duration=%ss)", pyrepr.Repr(o.SilenceThreshold), pyrepr.Repr(o.SilenceDuration)))
	voicePlayBeep(ctx, 880, 1)
	if err := rec.Start(ctx, func() { continuousOnSilence(ctx) }); err != nil {
		voiceCLILogger().Error(fmt.Sprintf("failed to start continuous recording: %s", err))
		voiceDebug(fmt.Sprintf("start_continuous: rec.start raised %s", err))
		voiceDeactivate(nil)
		return false, err
	}
	if o.OnStatus != nil {
		voiceSafeCall("", func() { o.OnStatus("listening") })
	}
	return true, nil
}

// StopContinuous ends the loop and releases the mic, transcribing the live capture first when forced
func StopContinuous(ctx context.Context, forceTranscribe bool) {
	continuousMu.Lock()
	if !continuousActive {
		continuousMu.Unlock()
		return
	}
	continuousActive = false
	rec := continuousRecorder
	callbacks := continuousCallbacks
	trackNoSpeech := forceTranscribe && !continuousAutoRestart
	continuousStopping = rec != nil
	continuousCallbacks = ContinuousCallbacks{}
	if !trackNoSpeech {
		continuousNoSpeechCount = 0
	}
	continuousMu.Unlock()
	if rec != nil {
		if forceTranscribe && callbacks.OnTranscript != nil {
			if callbacks.OnStatus != nil {
				voiceSafeCall("", func() { callbacks.OnStatus("transcribing") })
			}
			wavPath, err := rec.Stop()
			if err != nil {
				voiceCLILogger().Warn(fmt.Sprintf("failed to stop recorder: %s", err))
				voiceSafeCall("failed to cancel recorder: %v", rec.Cancel)
				wavPath = ""
			}
			go finishForcedStop(ctx, wavPath, callbacks, trackNoSpeech)
			return
		}
		voiceSafeCall("failed to cancel recorder: %v", rec.Cancel)
	}
	finishStop(ctx, callbacks.OnStatus)
}

// Background tail of a forced stop that transcribes, delivers and tallies
func finishForcedStop(ctx context.Context, wavPath string, callbacks ContinuousCallbacks, trackNoSpeech bool) {
	transcript, stopPhrase, stopText := turnTranscript(ctx, wavPath, "failed to stop/transcribe recorder: %s", "stop_continuous", "ending voice chat", false)
	if stopPhrase {
		signalHalt(true, stopText, callbacks)
	}
	if transcript != "" && callbacks.OnTranscript != nil {
		voiceSafeCall("on_transcript callback raised: %v", func() { callbacks.OnTranscript(transcript) })
	}
	if trackNoSpeech {
		held := voiceActivityHeld()
		continuousMu.Lock()
		shouldHalt, _ := tallySilence(transcript != "" || stopPhrase, held, "stop_continuous")
		continuousMu.Unlock()
		if shouldHalt && callbacks.OnSilentLimit != nil {
			voiceSafeCall("", callbacks.OnSilentLimit)
		}
	}
	finishStop(ctx, callbacks.OnStatus)
}

// Clears the stopping flag, plays the stopped cue and reports idle
func finishStop(ctx context.Context, onStatus func(string)) {
	continuousMu.Lock()
	continuousStopping = false
	continuousMu.Unlock()
	voicePlayBeep(ctx, 660, 2)
	if onStatus != nil {
		voiceSafeCall("", func() { onStatus("idle") })
	}
}

// IsContinuousActive reports whether a continuous voice loop is running
func IsContinuousActive() bool {
	continuousMu.Lock()
	defer continuousMu.Unlock()
	return continuousActive
}

// Silence callback that transcribes the capture, delivers it and rearms or ends the loop
func continuousOnSilence(ctx context.Context) {
	voiceDebug("_continuous_on_silence: fired")
	continuousMu.Lock()
	if !continuousActive {
		continuousMu.Unlock()
		voiceDebug("_continuous_on_silence: loop inactive — abort")
		return
	}
	rec, callbacks := continuousRecorder, continuousCallbacks
	continuousMu.Unlock()
	if rec == nil {
		voiceDebug("_continuous_on_silence: no recorder — abort")
		return
	}
	if callbacks.OnStatus != nil {
		voiceSafeCall("", func() { callbacks.OnStatus("transcribing") })
	}
	wavPath, err := rec.Stop()
	if err != nil {
		voiceCLILogger().Warn(fmt.Sprintf("continuous silence callback failed: %s", err))
		return
	}
	peak := -1
	if withPeak, hasPeak := rec.(interface{ PeakRMS() int }); hasPeak {
		peak = withPeak.PeakRMS()
	}
	var wavRepr any
	if wavPath != "" {
		wavRepr = wavPath
	}
	voiceDebug(fmt.Sprintf("_continuous_on_silence: rec.stop -> %s (peak_rms=%d)", pyrepr.Repr(wavRepr), peak))
	voicePlayBeep(ctx, 660, 2)
	transcript, stopPhrase, stopText := turnTranscript(ctx, wavPath, "continuous transcription failed: %s", "_continuous_on_silence", "ending loop", true)
	held := transcript == "" && !stopPhrase && voiceActivityHeld()
	continuousMu.Lock()
	if !continuousActive {
		continuousMu.Unlock()
		voiceDebug("_continuous_on_silence: stopped during transcribe — no restart")
		return
	}
	limitHit, noSpeech := tallySilence(transcript != "" || stopPhrase, held, "_continuous_on_silence")
	continuousMu.Unlock()
	if transcript != "" && callbacks.OnTranscript != nil {
		voiceSafeCall("on_transcript callback raised: %v", func() { callbacks.OnTranscript(transcript) })
	}
	if stopPhrase || limitHit {
		reason := fmt.Sprintf("%d silent cycles", noSpeech)
		if stopPhrase {
			reason = "stop phrase"
		}
		voiceDebug("_continuous_on_silence: halting (" + reason + ")")
		continuousMu.Lock()
		continuousActive = false
		continuousNoSpeechCount = 0
		continuousMu.Unlock()
		signalHalt(stopPhrase, stopText, callbacks)
		voiceSafeCall("", rec.Cancel)
		if callbacks.OnStatus != nil {
			voiceSafeCall("", func() { callbacks.OnStatus("idle") })
		}
		return
	}
	rearmAfterTurn(ctx, rec, callbacks.OnStatus, noSpeech)
}

// Waits out playing TTS then restarts capture, or stops a client driven loop
func rearmAfterTurn(ctx context.Context, rec VoiceRecorder, onStatus func(string), noSpeech int) {
	if !ttsPlaying.IsSet() {
		voiceDebug("_continuous_on_silence: waiting for TTS to finish")
		ttsPlaying.Wait(60)
		time.Sleep(300 * time.Millisecond)
		continuousMu.Lock()
		active := continuousActive
		continuousMu.Unlock()
		if !active {
			voiceDebug("_continuous_on_silence: stopped while waiting for TTS")
			return
		}
	}
	continuousMu.Lock()
	autoRestart := continuousAutoRestart
	continuousMu.Unlock()
	if !autoRestart {
		voiceDebug("_continuous_on_silence: auto_restart=False, stopping loop")
		voiceDeactivate(onStatus)
		return
	}
	voiceDebug(fmt.Sprintf("_continuous_on_silence: restarting loop (no_speech=%d)", noSpeech))
	voicePlayBeep(ctx, 880, 1)
	if err := rec.Start(ctx, func() { continuousOnSilence(ctx) }); err != nil {
		voiceCLILogger().Error(fmt.Sprintf("failed to restart continuous recording: %s", err))
		voiceDebug(fmt.Sprintf("_continuous_on_silence: restart raised %s", err))
		voiceDeactivate(onStatus)
		return
	}
	if onStatus != nil {
		voiceSafeCall("", func() { onStatus("listening") })
	}
}

// Markdown stripper of the whole file path when the shared cleaner fails
var legacyTTSStrip = []struct {
	re   *regexp.Regexp
	repl string
}{
	{regexp.MustCompile("```[\\s\\S]*?```"), " "},
	{regexp.MustCompile(`\[([^\]]+)\]\([^)]+\)`), "$1"},
	{regexp.MustCompile(`https?://\S+`), ""},
	{regexp.MustCompile(`\*\*(.+?)\*\*`), "$1"},
	{regexp.MustCompile(`\*(.+?)\*`), "$1"},
	{regexp.MustCompile("`(.+?)`"), "$1"},
	{regexp.MustCompile(`(?m)^#+\s*`), ""},
	{regexp.MustCompile(`(?m)^\s*[-*]\s+`), ""},
	{regexp.MustCompile(`---+`), ""},
	{regexp.MustCompile(`\n{3,}`), "\n\n"},
}

// Queue holding one reply delta and the end of text sentinel
type replyTextQueue struct{ items chan any }

func (q *replyTextQueue) Get(timeout time.Duration) (any, bool) {
	select {
	case item := <-q.items:
		return item, true
	case <-time.After(timeout):
		return nil, false
	}
}

// Threading event over pythread for the speaker pipeline
type speakEvent struct{ *pythread.Event }

func (e speakEvent) Wait(timeout time.Duration) bool { return e.Event.Wait(timeout.Seconds()) }

// Speaks through the streaming pipeline when a chunked streamer resolves, false sending the caller to the whole file path
func speakStreaming(ctx context.Context, text string, stop *pythread.Event) (bool, error) {
	env := &tools.Env{Home: home.Dir(ctx)}
	provider, err := ResolveStreamingProvider(ctx, env, LoadTTSConfig(ctx, env), "")
	if err != nil {
		return false, err
	}
	if provider == nil {
		return false, nil
	}
	queue := &replyTextQueue{items: make(chan any, 2)}
	queue.items <- text
	queue.items <- nil
	done := &pythread.Event{}
	if stop == nil {
		stop = &pythread.Event{}
	}
	StreamTTSToSpeaker(ctx, queue, speakEvent{stop}, speakEvent{done}, nil, "")
	return done.IsSet(), nil
}

// Cleans, synthesizes, plays and removes the reply audio through the TTS tool
func speakWholeFile(ctx context.Context, text string) error {
	ttsText := ""
	if failure := func() (failure any) {
		defer func() { failure = recover() }()
		ttsText = tools.PrepareSpokenText(text, nil)
		return nil
	}(); failure != nil {
		ttsText = text
		for _, rule := range legacyTTSStrip {
			ttsText = rule.re.ReplaceAllString(ttsText, rule.repl)
		}
		ttsText = pystr.Strip(ttsText)
	}
	if ttsText == "" {
		return nil
	}
	tmp, err := pypath.Gettempdir()
	if err != nil {
		return err
	}
	voiceDir := filepath.Join(tmp, "icebath_voice")
	if err := os.MkdirAll(voiceDir, 0o777); err != nil {
		return err
	}
	mp3Path := filepath.Join(voiceDir, "tts_"+time.Now().Format("20060102_150405")+".mp3")
	voiceDebug(fmt.Sprintf("speak_text: synthesizing %d chars -> %s", pystr.Len(ttsText), mp3Path))
	raw, err := TextToSpeechTool(ctx, ttsText, mp3Path)
	if err != nil {
		return err
	}
	var result map[string]any
	if json.Unmarshal([]byte(raw), &result) != nil {
		result = map[string]any{}
	}
	var playPaths []string
	if paths, isList := result["file_paths"].([]any); isList && len(paths) > 0 {
		for _, p := range paths {
			playPaths = append(playPaths, pyrepr.Str(p))
		}
	} else if single := result["file_path"]; pyrepr.Truthy(single) {
		playPaths = []string{pyrepr.Str(single)}
	} else {
		playPaths = []string{mp3Path}
	}
	playedAny := false
	if pyrepr.Truthy(result["success"]) {
		for _, playPath := range playPaths {
			if st, statErr := os.Stat(playPath); statErr == nil && st.Mode().IsRegular() && st.Size() > 0 {
				voiceDebug(fmt.Sprintf("speak_text: playing %s (%d bytes)", playPath, st.Size()))
				PlayAudioFile(playPath)
				playedAny = true
			}
		}
	}
	stem := mp3Path
	if i := strings.LastIndex(mp3Path, "."); i >= 0 {
		stem = mp3Path[:i]
	}
	for _, path := range append(playPaths, mp3Path, stem+".ogg") {
		if st, statErr := os.Stat(path); statErr == nil && st.Mode().IsRegular() {
			_ = os.Remove(path)
		}
	}
	if !playedAny {
		voiceDebug("speak_text: TTS tool produced no audio at " + mp3Path)
	}
	return nil
}

// SpeakText synthesizes and plays text, pausing live capture so the reply never feeds back into the mic
func SpeakText(ctx context.Context, text string, stop *pythread.Event) {
	if pystr.Strip(text) == "" {
		return
	}
	pausedRecording := false
	continuousMu.Lock()
	if continuousActive && continuousRecorder != nil && continuousRecorder.IsRecording() {
		voiceSafeCall("failed to pause recorder for TTS: %v", func() {
			continuousRecorder.Cancel()
			pausedRecording = true
		})
	}
	continuousMu.Unlock()
	ttsPlaying.Clear()
	voiceDebug(fmt.Sprintf("speak_text: TTS begin (paused_recording=%s)", pyrepr.Repr(pausedRecording)))
	defer func() {
		ttsPlaying.Set()
		voiceDebug("speak_text: TTS done")
		if pausedRecording {
			time.Sleep(300 * time.Millisecond)
			continuousMu.Lock()
			defer continuousMu.Unlock()
			if continuousActive && continuousRecorder != nil {
				if err := continuousRecorder.Start(ctx, func() { continuousOnSilence(ctx) }); err != nil {
					voiceCLILogger().Warn(fmt.Sprintf("failed to resume recorder after TTS: %s", err))
				} else {
					voiceDebug("speak_text: recording resumed after TTS")
				}
			}
		}
	}()
	err := func() (err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("%v", r)
			}
		}()
		streamed, streamErr := speakStreaming(ctx, text, stop)
		if streamErr != nil {
			voiceDebug(fmt.Sprintf("speak_text: streaming dispatch unavailable (%s); using sync path", streamErr))
		} else if streamed {
			return nil
		}
		return speakWholeFile(ctx, text)
	}()
	if err != nil {
		voiceCLILogger().Warn(fmt.Sprintf("Voice TTS playback failed: %s", err))
		voiceDebug(fmt.Sprintf("speak_text raised %s", err))
	}
}
