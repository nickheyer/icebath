package media

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pycodecs"
	"github.com/nickheyer/icebath/pkg/internal/pynumpy"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pysounddevice"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
)

const liveSessionsMax = 8

// Live STT session surface a recorder feeds, parks and finalizes
type liveSTTSession interface {
	SetInputRate(rate int)
	PushAudio(pcm []byte)
	EndAudio()
	Finalize(timeout float64) map[string]any
	Cancel()
	Provider() string
}

var (
	voiceMonotonic            = pythread.Monotonic
	voiceTranscribeAudio      = TranscribeAudio
	voiceOpenStreamingSession = func(ctx context.Context, onPartial func(string)) (liveSTTSession, error) {
		session, err := OpenStreamingSession(ctx, onPartial, nil)
		if session == nil {
			return nil, err
		}
		return session, err
	}
	liveMu       sync.Mutex
	liveSessions = map[string]liveSTTSession{}
	liveOrder    []string
)

// VoiceRecorder is the push to talk recorder create_audio_recorder returns
type VoiceRecorder interface {
	Start(ctx context.Context, onSilenceStop func()) error
	Stop() (string, error)
	Cancel()
	Shutdown()
	IsRecording() bool
	ElapsedSeconds() float64
	CurrentRMS() int
	SupportsSilenceAutostop() bool
	SetOnLivePartial(fn func(string))
	SetSilenceThreshold(threshold int)
	SetSilenceDuration(seconds float64)
	SetMaxRecordingSeconds(seconds float64)
}

// Path for a new timestamped recording in the voice temp dir, the dir created on demand
func newRecordingPath(ext string) (string, error) {
	dir := voiceTempDir()
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return "", err
	}
	return pypath.Join(dir, "recording_"+time.Now().Format("20060102_150405")+"."+ext), nil
}

// Ends a live session's audio and parks it under its WAV, cancelling it without a WAV or past the cap
func parkLiveSession(wavPath string, session liveSTTSession) {
	if session == nil {
		return
	}
	if wavPath == "" {
		session.Cancel()
		return
	}
	session.EndAudio()
	liveMu.Lock()
	defer liveMu.Unlock()
	if _, ok := liveSessions[wavPath]; !ok {
		liveOrder = append(liveOrder, wavPath)
	}
	liveSessions[wavPath] = session
	for len(liveSessions) > liveSessionsMax {
		oldest := liveOrder[0]
		liveOrder = liveOrder[1:]
		evicted := liveSessions[oldest]
		delete(liveSessions, oldest)
		evicted.Cancel()
	}
}

func takeLiveSession(wavPath string) liveSTTSession {
	liveMu.Lock()
	defer liveMu.Unlock()
	session, ok := liveSessions[wavPath]
	if !ok {
		return nil
	}
	delete(liveSessions, wavPath)
	for i, k := range liveOrder {
		if k == wavPath {
			liveOrder = append(liveOrder[:i], liveOrder[i+1:]...)
			break
		}
	}
	return session
}

type recorderBase struct {
	mu                  sync.Mutex
	recording           bool
	startTime           float64
	currentRMS          int
	onLivePartial       func(string)
	live                liveSTTSession
	silenceThreshold    int
	silenceDuration     float64
	maxRecordingSeconds float64
}

// Opens a live STT session fed at the capture rate, none when streaming is off or the open fails
func (r *recorderBase) openLiveSession(ctx context.Context, sampleRate int) {
	r.mu.Lock()
	onPartial := r.onLivePartial
	r.mu.Unlock()
	live, err := voiceOpenStreamingSession(ctx, onPartial)
	if err != nil {
		voiceLog().Debug(fmt.Sprintf("Live STT session did not open: %s", err))
		live = nil
	}
	if live != nil {
		live.SetInputRate(sampleRate)
		voiceLog().Info(fmt.Sprintf("Live STT streaming (%s)", live.Provider()))
	}
	r.mu.Lock()
	r.live = live
	r.mu.Unlock()
}

func (r *recorderBase) detachLive() liveSTTSession {
	r.mu.Lock()
	defer r.mu.Unlock()
	live := r.live
	r.live = nil
	return live
}

func (r *recorderBase) IsRecording() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.recording
}

// Seconds since recording started, zero while idle
func (r *recorderBase) ElapsedSeconds() float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.recording {
		return 0
	}
	return voiceMonotonic() - r.startTime
}

// Level of the latest audio block
func (r *recorderBase) CurrentRMS() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.currentRMS
}

func (r *recorderBase) SetOnLivePartial(fn func(string)) {
	r.mu.Lock()
	r.onLivePartial = fn
	r.mu.Unlock()
}

func (r *recorderBase) SetSilenceThreshold(threshold int) {
	r.mu.Lock()
	r.silenceThreshold = threshold
	r.mu.Unlock()
}

func (r *recorderBase) SetSilenceDuration(seconds float64) {
	r.mu.Lock()
	r.silenceDuration = seconds
	r.mu.Unlock()
}

func (r *recorderBase) SetMaxRecordingSeconds(seconds float64) {
	r.mu.Lock()
	r.maxRecordingSeconds = seconds
	r.mu.Unlock()
}

// TermuxAudioRecorder records through termux-microphone-record without live silence detection
type TermuxAudioRecorder struct {
	recorderBase
	recordingPath string
}

// NewTermuxAudioRecorder is an idle Termux API recorder
func NewTermuxAudioRecorder() *TermuxAudioRecorder {
	return &TermuxAudioRecorder{recorderBase: recorderBase{silenceThreshold: silenceRMSThreshold, silenceDuration: silenceDurationSeconds}}
}

func (r *TermuxAudioRecorder) SupportsSilenceAutostop() bool { return false }

// Starts termux-microphone-record into a new AAC file, failing when the API package or app is missing
func (r *TermuxAudioRecorder) Start(context.Context, func()) error {
	micCmd := termuxMicrophoneCommand()
	if micCmd == "" {
		return errors.New("Termux voice capture requires the termux-api package and app.\n" +
			"Install with: pkg install termux-api\n" +
			"Then install/update the Termux:API Android app.")
	}
	if !termuxAPIAppInstalled() {
		return errors.New("Termux voice capture requires the Termux:API Android app.\n" +
			"Install/update the Termux:API app, then retry /voice on.")
	}
	r.mu.Lock()
	if r.recording {
		r.mu.Unlock()
		return nil
	}
	path, err := newRecordingPath("aac")
	r.recordingPath = path
	r.mu.Unlock()
	if err != nil {
		return err
	}
	command := []string{micCmd, "-f", path, "-l", "0", "-e", "aac", "-r", fmt.Sprint(voiceSampleRate), "-c", fmt.Sprint(voiceChannels)}
	if _, err := runQuiet(command, 15, true); err != nil {
		var called *pysubprocess.CalledProcessError
		if errors.As(err, &called) {
			details, _ := pycodecs.DecodeReplace(called.Stderr, "utf-8")
			if details == "" {
				details, _ = pycodecs.DecodeReplace(called.Stdout, "utf-8")
			}
			if details == "" {
				details = called.Error()
			}
			return fmt.Errorf("Termux microphone start failed: %s", pystr.Strip(details))
		}
		return fmt.Errorf("Termux microphone start failed: %s", err)
	}
	r.mu.Lock()
	r.startTime, r.recording, r.currentRMS = voiceMonotonic(), true, 0
	r.mu.Unlock()
	voiceLog().Info("Termux voice recording started")
	return nil
}

func (r *TermuxAudioRecorder) stopTermuxRecording() error {
	if micCmd := termuxMicrophoneCommand(); micCmd != "" {
		_, err := runQuiet([]string{micCmd, "-q"}, 15, false)
		return err
	}
	return nil
}

// Clears the recording state, returning what it was
func (r *TermuxAudioRecorder) resetState() (bool, string, float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	wasRecording, path, startedAt := r.recording, r.recordingPath, r.startTime
	r.recording, r.recordingPath, r.currentRMS = false, "", 0
	return wasRecording, path, startedAt
}

// Stops the microphone and returns the AAC path, empty for sub 0.3 second taps and empty files
func (r *TermuxAudioRecorder) Stop() (string, error) {
	wasRecording, path, startedAt := r.resetState()
	if !wasRecording {
		return "", nil
	}
	if err := r.stopTermuxRecording(); err != nil {
		return "", err
	}
	info, err := os.Stat(path)
	if path == "" || err != nil || !info.Mode().IsRegular() {
		return "", nil
	}
	if voiceMonotonic()-startedAt < 0.3 || info.Size() <= 0 {
		unlinkQuietly(path)
		return "", nil
	}
	voiceLog().Info(fmt.Sprintf("Termux voice recording stopped: %s", path))
	return path, nil
}

// Stops the microphone and deletes the partial recording
func (r *TermuxAudioRecorder) Cancel() {
	_, path, _ := r.resetState()
	_ = r.stopTermuxRecording()
	unlinkQuietly(path)
	voiceLog().Info("Termux voice recording cancelled")
}

func (r *TermuxAudioRecorder) Shutdown() { r.Cancel() }

// AudioRecorder captures through PortAudio with speech confirmation, silence auto stop and a length cap
type AudioRecorder struct {
	recorderBase
	stream            pysounddevice.InputStream
	frames            [][]int16
	sampleRate        int
	onSilenceStop     func()
	minSpeechDuration float64
	maxDipTolerance   float64
	maxWait           float64
	peakRMS           int
	hasSpoken         bool
	speechStart       float64
	dipStart          float64
	silenceStart      float64
	resumeStart       float64
	resumeDipStart    float64
}

// NewAudioRecorder is an idle recorder with the default silence parameters
func NewAudioRecorder() *AudioRecorder {
	return &AudioRecorder{recorderBase: recorderBase{silenceThreshold: silenceRMSThreshold, silenceDuration: silenceDurationSeconds},
		sampleRate: voiceSampleRate, minSpeechDuration: 0.3, maxDipTolerance: 0.3, maxWait: 15.0}
}

func (r *AudioRecorder) SupportsSilenceAutostop() bool { return true }

func (r *AudioRecorder) resetDetectionState() {
	r.hasSpoken = false
	r.speechStart, r.dipStart, r.silenceStart = 0, 0, 0
	r.resumeStart, r.resumeDipStart = 0, 0
}

func (r *AudioRecorder) maxDurationReached(elapsed float64) bool {
	limit := r.maxRecordingSeconds
	return limit != 0 && limit > 0 && elapsed >= limit
}

// Confirms speech after sustained level, tolerating short dips, and resets the silence clock on resumed speech
func (r *AudioRecorder) trackSpeech(rms int, now float64) {
	switch {
	case rms > r.silenceThreshold:
		r.dipStart = 0
		if r.speechStart == 0 {
			r.speechStart = now
		} else if !r.hasSpoken && now-r.speechStart >= r.minSpeechDuration {
			r.hasSpoken = true
			voiceLog().Debug(fmt.Sprintf("Speech confirmed (%.2fs above threshold)", now-r.speechStart))
		}
		if !r.hasSpoken {
			r.silenceStart = 0
		} else {
			r.resumeDipStart = 0
			if r.resumeStart == 0 {
				r.resumeStart = now
			} else if now-r.resumeStart >= r.minSpeechDuration {
				r.silenceStart = 0
				r.resumeStart = 0
			}
		}
	case r.hasSpoken:
		if r.resumeStart > 0 {
			if r.resumeDipStart == 0 {
				r.resumeDipStart = now
			} else if now-r.resumeDipStart >= r.maxDipTolerance {
				r.resumeStart = 0
				r.resumeDipStart = 0
			}
		}
	case r.speechStart > 0:
		if r.dipStart == 0 {
			r.dipStart = now
		} else if now-r.dipStart >= r.maxDipTolerance {
			voiceLog().Debug(fmt.Sprintf("Speech attempt reset (dip lasted %.2fs)", now-r.dipStart))
			r.speechStart = 0
			r.dipStart = 0
		}
	}
}

// Whether silence after speech, no speech within the wait, or the length cap ends the recording
func (r *AudioRecorder) shouldAutoStop(rms int, now float64) bool {
	elapsed := now - r.startTime
	if r.hasSpoken && rms <= r.silenceThreshold {
		if r.silenceStart == 0 {
			r.silenceStart = now
		} else if now-r.silenceStart >= r.silenceDuration {
			voiceLog().Info(fmt.Sprintf("Silence detected (%.1fs), auto-stopping", r.silenceDuration))
			return true
		}
	} else if !r.hasSpoken && elapsed >= r.maxWait {
		voiceLog().Info(fmt.Sprintf("No speech within %.0fs, auto-stopping", r.maxWait))
		return true
	}
	if r.maxDurationReached(elapsed) {
		voiceLog().Info(fmt.Sprintf("Max recording length reached (%.0fs), auto-stopping", r.maxRecordingSeconds))
		return true
	}
	return false
}

// Runs the silence callback once on its own goroutine, logging a panic
func (r *AudioRecorder) fireSilenceCallback() {
	r.mu.Lock()
	cb := r.onSilenceStop
	r.onSilenceStop = nil
	r.mu.Unlock()
	if cb == nil {
		return
	}
	go func() {
		defer func() {
			if p := recover(); p != nil {
				voiceLog().Error(fmt.Sprintf("Silence callback failed: %v", p))
			}
		}()
		cb()
	}()
}

// Keeps the block, feeds the live session and tracks the level, false for an empty block numpy cannot average
func (r *AudioRecorder) onAudioBlock(indata []int16) bool {
	r.mu.Lock()
	r.frames = append(r.frames, append([]int16(nil), indata...))
	live := r.live
	r.mu.Unlock()
	if live != nil {
		live.PushAudio(int16Bytes(indata))
	}
	if len(indata) == 0 {
		return false
	}
	rms := int(pynumpy.RMSInt16(indata))
	r.mu.Lock()
	r.currentRMS = rms
	r.peakRMS = max(r.peakRMS, rms)
	fire := false
	if r.onSilenceStop != nil {
		now := voiceMonotonic()
		r.trackSpeech(rms, now)
		fire = r.shouldAutoStop(rms, now)
	}
	r.mu.Unlock()
	if fire {
		r.fireSilenceCallback()
	}
	return true
}

func (r *AudioRecorder) callback(indata []int16, _ int, status pysounddevice.CallbackFlags) pysounddevice.CallbackResult {
	if status != 0 {
		voiceLog().Debug(fmt.Sprintf("sounddevice status: %s", status))
	}
	if r.IsRecording() && !r.onAudioBlock(indata) {
		return pysounddevice.Abort
	}
	return pysounddevice.Continue
}

// Keeps one live input stream, rebuilding an inactive one and retrying a timed out start once
func (r *AudioRecorder) ensureStream() error {
	r.mu.Lock()
	stream := r.stream
	r.mu.Unlock()
	if stream != nil {
		active, err := stream.Active()
		if err == nil && active {
			return nil
		}
		if err != nil {
			voiceLog().Debug(fmt.Sprintf("Audio input stream liveness probe failed: %s", err))
		}
		voiceLog().Debug("Rebuilding inactive audio input stream")
		r.closeStreamWithTimeout(3.0)
	}
	sd, err := importAudio()
	if err != nil {
		return err
	}
	r.mu.Lock()
	rate := r.sampleRate
	r.mu.Unlock()
	var opened pysounddevice.InputStream
	for attempt := 0; attempt < 2; attempt++ {
		opened, err = sd.InputStream(pysounddevice.StreamParams{Samplerate: float64(rate), Channels: voiceChannels}, r.callback)
		if err == nil {
			if err = opened.Start(); err == nil {
				break
			}
		}
		if opened != nil {
			_ = opened.Close(true)
		}
		opened = nil
		if attempt > 0 || !strings.Contains(pystr.Lower(err.Error()), "timed out") {
			return fmt.Errorf("Failed to open audio input stream: %s. Check that a microphone is connected and accessible.", err)
		}
		voiceLog().Info("Audio input stream start timed out; retrying once")
	}
	r.mu.Lock()
	r.stream = opened
	r.mu.Unlock()
	return nil
}

// Starts capture at the input device rate, a missing PortAudio or audio extra reported as the remedy
func (r *AudioRecorder) Start(ctx context.Context, onSilenceStop func()) error {
	sd, err := importAudio()
	switch {
	case err == nil:
	case isAudioOSError(err):
		return errors.New(portaudioMissingMessage())
	case isAudioImportError(err):
		return errors.New("Voice mode requires sounddevice and numpy.\n" + voiceCaptureInstallHint(err))
	default:
		return err
	}
	r.mu.Lock()
	if r.recording {
		r.mu.Unlock()
		return nil
	}
	r.frames = nil
	r.startTime = voiceMonotonic()
	r.resetDetectionState()
	r.peakRMS = 0
	r.currentRMS = 0
	r.onSilenceStop = onSilenceStop
	r.mu.Unlock()
	rate := defaultInputSamplerate(sd)
	r.mu.Lock()
	r.sampleRate = rate
	r.mu.Unlock()
	if err := r.ensureStream(); err != nil {
		return err
	}
	r.openLiveSession(ctx, rate)
	r.mu.Lock()
	r.recording = true
	r.mu.Unlock()
	voiceLog().Info(fmt.Sprintf("Voice recording started (rate=%d, channels=%d)", rate, voiceChannels))
	return nil
}

// Stops and closes the stream on a helper goroutine, moving on after the timeout
func (r *AudioRecorder) closeStreamWithTimeout(timeout float64) {
	r.mu.Lock()
	stream := r.stream
	r.stream = nil
	r.mu.Unlock()
	if stream == nil {
		return
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		quietStream(func() { _ = stream.Stop(true) })
		quietStream(func() { _ = stream.Close(true) })
	}()
	select {
	case <-done:
	case <-time.After(time.Duration(timeout * float64(time.Second))):
		voiceLog().Warn(fmt.Sprintf("Audio stream close timed out after %.1fs — forcing ahead", timeout))
	}
}

// Runs one stream call, swallowing a panic like suppress(Exception)
func quietStream(call func()) {
	defer func() { _ = recover() }()
	call()
}

// Ends capture and returns the WAV path, parking the live session under it
func (r *AudioRecorder) Stop() (string, error) {
	wavPath, err := r.stopCapture()
	if err != nil {
		return "", err
	}
	parkLiveSession(wavPath, r.detachLive())
	return wavPath, nil
}

// Writes the captured frames as a WAV at the capture rate, nothing for short or too quiet recordings
func (r *AudioRecorder) stopCapture() (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.recording {
		return "", nil
	}
	r.recording, r.currentRMS = false, 0
	if len(r.frames) == 0 {
		return "", nil
	}
	var audio []int16
	for _, frame := range r.frames {
		audio = append(audio, frame...)
	}
	r.frames = nil
	voiceLog().Info(fmt.Sprintf("Voice recording stopped (%.1fs, %d samples)", voiceMonotonic()-r.startTime, len(audio)))
	if len(audio) < int(float64(r.sampleRate)*0.3) {
		voiceLog().Debug(fmt.Sprintf("Recording too short (%d samples), discarding", len(audio)))
		return "", nil
	}
	if r.peakRMS < r.silenceThreshold {
		voiceLog().Info(fmt.Sprintf("Recording too quiet (peak RMS=%d < %d), discarding", r.peakRMS, r.silenceThreshold))
		return "", nil
	}
	return voiceWriteWAV(audio, r.sampleRate)
}

// Drops the capture state and cancels the live session
func (r *AudioRecorder) discard() {
	r.mu.Lock()
	r.recording, r.frames, r.onSilenceStop, r.currentRMS = false, nil, nil, 0
	r.mu.Unlock()
	if live := r.detachLive(); live != nil {
		live.Cancel()
	}
}

func (r *AudioRecorder) Cancel() {
	r.discard()
	voiceLog().Info("Voice recording cancelled")
}

// Discards the recording and releases the input stream
func (r *AudioRecorder) Shutdown() {
	r.discard()
	r.closeStreamWithTimeout(3.0)
	voiceLog().Info("AudioRecorder shut down")
}

// Writes int16 mono PCM as a new recording WAV
func writeRecordingWAV(audio []int16, sampleRate int) (string, error) {
	wavPath, err := newRecordingPath("wav")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(wavPath, wrapPCMAsWAV(int16Bytes(audio), sampleRate, voiceChannels, voiceSampleWidth), 0o666); err != nil {
		return "", err
	}
	info, err := os.Stat(wavPath)
	if err != nil {
		return "", err
	}
	voiceLog().Info(fmt.Sprintf("WAV written: %s (%d bytes)", wavPath, info.Size()))
	return wavPath, nil
}

// CreateAudioRecorder picks the Termux API recorder when it can capture, else the PortAudio recorder
func CreateAudioRecorder() VoiceRecorder {
	if termuxVoiceCaptureAvailable() {
		return NewTermuxAudioRecorder()
	}
	return NewAudioRecorder()
}

// Final transcript of the live session parked under the WAV, nil when there is none or it came back empty
func liveResult(wavPath string) map[string]any {
	session := takeLiveSession(wavPath)
	if session == nil {
		return nil
	}
	result := session.Finalize(streamingFinalizeTimeout)
	if pyTruthy(result["success"]) && pyTruthy(result["transcript"]) && pystr.Strip(pyrepr.Str(result["transcript"])) != "" {
		return result
	}
	reason := "empty"
	if pyTruthy(result["error"]) {
		reason = pyrepr.Str(result["error"])
	}
	voiceLog().Info(fmt.Sprintf("Live STT gave no transcript (%s); transcribing the recording", reason))
	return nil
}

// TranscribeRecording returns the live transcript or transcribes the WAV, filtering Whisper hallucinations
func TranscribeRecording(ctx context.Context, wavPath, model string) (map[string]any, error) {
	result := liveResult(wavPath)
	if result == nil {
		transcribed, err := voiceTranscribeAudio(ctx, wavPath, model, "voice_mode")
		if err != nil {
			return nil, err
		}
		result = transcribed
	}
	if pyTruthy(result["success"]) {
		raw := ""
		if v, ok := result["transcript"]; ok {
			raw = pyrepr.Str(v)
		}
		if isWhisperHallucination(raw) && !isVoiceStopPhrase(raw, nil) {
			voiceLog().Info(fmt.Sprintf("Filtered Whisper hallucination: %s", pyrepr.Repr(result["transcript"])))
			return map[string]any{"success": true, "transcript": "", "filtered": true}, nil
		}
	}
	if pyTruthy(result["no_speech"]) {
		return map[string]any{"success": true, "transcript": "", "no_speech": true}, nil
	}
	return result, nil
}
