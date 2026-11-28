package voice

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/speech"
	speechconfig "github.com/nickheyer/icebath/internal/speech/config"
	"github.com/nickheyer/icebath/internal/speech/portaudio"
)

// Folder recordings are written to until they are transcribed
func recordings() string { return filepath.Join(os.TempDir(), "icebath_voice") }

// Path for a new timestamped recording
func recordingPath(ext string) (string, error) {
	dir := recordings()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("The folder recordings are kept in could not be made. The error was: %s.", strings.TrimSuffix(err.Error(), "."))
	}
	return filepath.Join(dir, "recording_"+time.Now().Format("20060102_150405.000")+"."+ext), nil
}

// Longest a recording stays on disk once nothing reads it, a capture removing its own file after the transcription
const keepRecordings = time.Hour

// Removes recordings older than the age that a capture that died left behind
func sweep(age time.Duration) int {
	matches, _ := filepath.Glob(filepath.Join(recordings(), "recording_*"))
	removed := 0
	for _, path := range matches {
		if info, err := os.Stat(path); err == nil && time.Since(info.ModTime()) > age && os.Remove(path) == nil {
			removed++
		}
	}
	if removed > 0 {
		slog.Debug(fmt.Sprintf("Cleaned up %d old voice recordings", removed))
	}
	return removed
}

// What a stopped recording left: its file, empty for one too short or too quiet, and the live transcription that heard it
type capture struct {
	path string
	live *liveSession
	peak int
}

// A microphone that records until stopped or, where it can tell, until the speaker falls silent
type recorder interface {
	start(ctx context.Context, silenced func()) error
	stop() (capture, error)
	cancel()
	shutdown()
	recording() bool
	// Whether it ends a recording on its own after speech and silence
	autostop() bool
	tune(threshold, duration, cap float64)
}

// Settings and live transcription a recording starts with
type limits struct {
	threshold float64
	duration  float64
	cap       float64
	// Opens the live transcription at the capture rate, nil when the recognizer has none
	live func(ctx context.Context, rate int) *liveSession
}

func (l *limits) tune(threshold, duration, cap float64) {
	l.threshold, l.duration, l.cap = threshold, duration, max(0, cap)
}

// Termux:API recorder, without live silence detection
type termuxRecorder struct {
	limits
	mu      sync.Mutex
	active  bool
	path    string
	started time.Time
}

func (r *termuxRecorder) autostop() bool { return false }

func (r *termuxRecorder) recording() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.active
}

func (r *termuxRecorder) start(context.Context, func()) error {
	mic := termuxMic()
	if mic == "" {
		return errors.New("Recording on Termux needs the termux-api package and the Termux:API app. Run pkg install termux-api, " +
			"then install or update the Termux:API app for Android.")
	}
	if !termuxApp() {
		return errors.New("Recording on Termux needs the Termux:API app for Android. Install or update it.")
	}
	r.mu.Lock()
	if r.active {
		r.mu.Unlock()
		return nil
	}
	r.mu.Unlock()
	path, err := recordingPath("aac")
	if err != nil {
		return err
	}
	argv := []string{mic, "-f", path, "-l", "0", "-e", "aac", "-r", strconv.Itoa(sampleRate), "-c", strconv.Itoa(channels)}
	if out, err := output(argv, 15*time.Second); err != nil {
		detail := ""
		if exit, ok := errors.AsType[*exec.ExitError](err); ok {
			detail = strings.TrimSpace(strings.ToValidUTF8(string(exit.Stderr), "�"))
		}
		detail = cmp.Or(detail, strings.TrimSpace(strings.ToValidUTF8(string(out), "�")), err.Error())
		return fmt.Errorf("The Termux microphone did not start: %s.", strings.TrimSuffix(detail, "."))
	}
	r.mu.Lock()
	r.active, r.path, r.started = true, path, time.Now()
	r.mu.Unlock()
	slog.Info("Termux voice recording started")
	return nil
}

// Stops termux-microphone-record, an error only when it could not run
func (r *termuxRecorder) halt() error {
	mic := termuxMic()
	if mic == "" {
		return nil
	}
	if _, err := output([]string{mic, "-q"}, 15*time.Second); err != nil {
		if _, exited := errors.AsType[*exec.ExitError](err); !exited {
			return err
		}
	}
	return nil
}

func (r *termuxRecorder) reset() (bool, string, time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	active, path, started := r.active, r.path, r.started
	r.active, r.path = false, ""
	return active, path, started
}

// The AAC file, none for a tap under 0.3 seconds or an empty file
func (r *termuxRecorder) stop() (capture, error) {
	active, path, started := r.reset()
	if !active {
		return capture{}, nil
	}
	if err := r.halt(); err != nil {
		return capture{}, err
	}
	info, err := os.Stat(path)
	if path == "" || err != nil || !info.Mode().IsRegular() {
		return capture{}, nil
	}
	if time.Since(started) < 300*time.Millisecond || info.Size() <= 0 {
		os.Remove(path)
		return capture{}, nil
	}
	slog.Info(fmt.Sprintf("Termux voice recording stopped: %s", path))
	return capture{path: path, peak: -1}, nil
}

func (r *termuxRecorder) cancel() {
	_, path, _ := r.reset()
	r.halt()
	if path != "" {
		os.Remove(path)
	}
	slog.Info("Termux voice recording cancelled")
}

func (r *termuxRecorder) shutdown() { r.cancel() }

// PortAudio recorder with speech confirmation, silence auto stop and a length cap
type audioRecorder struct {
	limits
	mu       sync.Mutex
	stream   *portaudio.Stream
	frames   [][]int16
	rate     int
	silenced func()
	live     *liveSession
	active   bool
	started  time.Time
	peak     int
	// Speech confirmation and silence clocks
	spoken       bool
	speechStart  time.Time
	dipStart     time.Time
	silenceStart time.Time
	resumeStart  time.Time
	resumeDip    time.Time
}

const (
	minSpeech   = 300 * time.Millisecond
	maxDip      = 300 * time.Millisecond
	waitSpeech  = 15 * time.Second
	shortestRec = 0.3
)

func (r *audioRecorder) autostop() bool { return true }

func (r *audioRecorder) recording() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.active
}

func (r *audioRecorder) resetClocks() {
	r.spoken = false
	r.speechStart, r.dipStart, r.silenceStart, r.resumeStart, r.resumeDip = time.Time{}, time.Time{}, time.Time{}, time.Time{}, time.Time{}
}

// Confirms speech after a sustained level, short dips tolerated, and restarts the silence clock on resumed speech
func (r *audioRecorder) track(rms int, now time.Time) {
	switch {
	case float64(rms) > r.threshold:
		r.dipStart = time.Time{}
		if r.speechStart.IsZero() {
			r.speechStart = now
		} else if !r.spoken && now.Sub(r.speechStart) >= minSpeech {
			r.spoken = true
			slog.Debug(fmt.Sprintf("Speech confirmed (%.2fs above threshold)", now.Sub(r.speechStart).Seconds()))
		}
		if !r.spoken {
			r.silenceStart = time.Time{}
			return
		}
		r.resumeDip = time.Time{}
		if r.resumeStart.IsZero() {
			r.resumeStart = now
		} else if now.Sub(r.resumeStart) >= minSpeech {
			r.silenceStart, r.resumeStart = time.Time{}, time.Time{}
		}
	case r.spoken:
		if !r.resumeStart.IsZero() {
			if r.resumeDip.IsZero() {
				r.resumeDip = now
			} else if now.Sub(r.resumeDip) >= maxDip {
				r.resumeStart, r.resumeDip = time.Time{}, time.Time{}
			}
		}
	case !r.speechStart.IsZero():
		if r.dipStart.IsZero() {
			r.dipStart = now
		} else if now.Sub(r.dipStart) >= maxDip {
			slog.Debug(fmt.Sprintf("Speech attempt reset (dip lasted %.2fs)", now.Sub(r.dipStart).Seconds()))
			r.speechStart, r.dipStart = time.Time{}, time.Time{}
		}
	}
}

// Whether silence after speech, no speech within the wait, or the length cap ends the recording
func (r *audioRecorder) over(rms int, now time.Time) bool {
	elapsed := now.Sub(r.started)
	if r.spoken && float64(rms) <= r.threshold {
		if r.silenceStart.IsZero() {
			r.silenceStart = now
		} else if now.Sub(r.silenceStart).Seconds() >= r.duration {
			slog.Info(fmt.Sprintf("Silence detected (%.1fs), auto-stopping", r.duration))
			return true
		}
	} else if !r.spoken && elapsed >= waitSpeech {
		slog.Info(fmt.Sprintf("No speech within %.0fs, auto-stopping", waitSpeech.Seconds()))
		return true
	}
	if r.cap > 0 && elapsed.Seconds() >= r.cap {
		slog.Info(fmt.Sprintf("Max recording length reached (%.0fs), auto-stopping", r.cap))
		return true
	}
	return false
}

// Level of a block
func rms(block []int16) float64 {
	if len(block) == 0 {
		return 0
	}
	sum := 0.0
	for _, v := range block {
		sum += float64(v) * float64(v)
	}
	return math.Sqrt(sum / float64(len(block)))
}

// Keeps a block, feeds the live transcription and tracks the level
func (r *audioRecorder) block(data []int16, _ int, status portaudio.CallbackFlags) portaudio.CallbackResult {
	if status != 0 {
		slog.Debug(fmt.Sprintf("PortAudio input status: %s", status))
	}
	r.mu.Lock()
	if !r.active {
		r.mu.Unlock()
		return portaudio.Continue
	}
	r.frames = append(r.frames, data)
	live := r.live
	r.mu.Unlock()
	if live != nil {
		live.push(speech.PCMBytes(data))
	}
	if len(data) == 0 {
		return portaudio.Abort
	}
	level := int(rms(data))
	r.mu.Lock()
	r.peak = max(r.peak, level)
	var fire func()
	if r.silenced != nil {
		now := time.Now()
		r.track(level, now)
		if r.over(level, now) {
			fire, r.silenced = r.silenced, nil
		}
	}
	r.mu.Unlock()
	if fire != nil {
		go func() {
			defer func() {
				if p := recover(); p != nil {
					slog.Error(fmt.Sprintf("Silence callback failed: %v", p))
				}
			}()
			fire()
		}()
	}
	return portaudio.Continue
}

// Keeps one live input stream, rebuilding one that stopped and retrying a start that timed out once
func (r *audioRecorder) ensure(pa portaudio.Audio) error {
	r.mu.Lock()
	stream, rate := r.stream, r.rate
	r.mu.Unlock()
	if stream != nil {
		active, err := stream.Active()
		if err == nil && active {
			return nil
		}
		if err != nil {
			slog.Debug(fmt.Sprintf("Audio input stream liveness probe failed: %s", err))
		}
		slog.Debug("Rebuilding inactive audio input stream")
		r.close(3 * time.Second)
	}
	var opened *portaudio.Stream
	var err error
	for attempt := range 2 {
		if opened, err = pa.OpenInput(portaudio.StreamParams{SampleRate: float64(rate), Channels: channels}, r.block); err == nil {
			if err = opened.Start(); err == nil {
				break
			}
			opened.Close()
		}
		opened = nil
		if attempt > 0 || !strings.Contains(strings.ToLower(err.Error()), "timed out") {
			return fmt.Errorf("The microphone did not open: %s. Check that a microphone is plugged in and that icebath may use it.", strings.TrimSuffix(err.Error(), "."))
		}
		slog.Info("Audio input stream start timed out; retrying once")
	}
	r.mu.Lock()
	r.stream = opened
	r.mu.Unlock()
	return nil
}

// Starts capture at the microphone's own rate, a PortAudio that can't load reported with its remedy
func (r *audioRecorder) start(ctx context.Context, silenced func()) error {
	pa, err := audio()
	switch {
	case err == nil:
	case missingLibrary(err):
		return errors.New(portaudioProblem().Sentence())
	case unloadable(err):
		return errors.New(captureProblem(err).Sentence())
	default:
		return fmt.Errorf("The sound system did not start. The error was: %s.", strings.TrimSuffix(err.Error(), "."))
	}
	r.mu.Lock()
	if r.active {
		r.mu.Unlock()
		return nil
	}
	r.frames, r.started, r.peak, r.silenced = nil, time.Now(), 0, silenced
	r.resetClocks()
	r.rate = inputRate(pa)
	rate := r.rate
	r.mu.Unlock()
	if err := r.ensure(pa); err != nil {
		return err
	}
	var live *liveSession
	if r.limits.live != nil {
		live = r.limits.live(ctx, rate)
	}
	r.mu.Lock()
	r.live, r.active = live, true
	r.mu.Unlock()
	slog.Info(fmt.Sprintf("Voice recording started (rate=%d, channels=%d)", rate, channels))
	return nil
}

// Stops and closes the stream on a helper goroutine, moving on after the timeout
func (r *audioRecorder) close(timeout time.Duration) {
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
		stream.Stop()
		stream.Close()
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		slog.Warn(fmt.Sprintf("Audio stream close timed out after %.1fs — forcing ahead", timeout.Seconds()))
	}
}

// The recording as a WAV at the capture rate, none for one too short or too quiet, with its live transcription
func (r *audioRecorder) stop() (capture, error) {
	r.mu.Lock()
	if !r.active {
		r.mu.Unlock()
		return capture{}, nil
	}
	r.active = false
	frames, rate, peak, live := r.frames, r.rate, r.peak, r.live
	r.frames, r.live = nil, nil
	started := r.started
	r.mu.Unlock()
	if len(frames) == 0 {
		return r.discarded(live, peak)
	}
	var data []int16
	for _, frame := range frames {
		data = append(data, frame...)
	}
	slog.Info(fmt.Sprintf("Voice recording stopped (%.1fs, %d samples)", time.Since(started).Seconds(), len(data)))
	if len(data) < int(float64(rate)*shortestRec) {
		slog.Debug(fmt.Sprintf("Recording too short (%d samples), discarding", len(data)))
		return r.discarded(live, peak)
	}
	if float64(peak) < r.threshold {
		slog.Info(fmt.Sprintf("Recording too quiet (peak RMS=%d < %d), discarding", peak, int(r.threshold)))
		return r.discarded(live, peak)
	}
	path, err := recordingPath("wav")
	if err == nil {
		err = os.WriteFile(path, speech.WAV(speech.PCMBytes(data), rate, channels, sampleBits), 0o600)
	}
	if err != nil {
		if live != nil {
			live.drop()
		}
		return capture{}, err
	}
	if live != nil {
		live.end()
	}
	slog.Info(fmt.Sprintf("WAV written: %s (%d bytes)", path, 44+2*len(data)))
	return capture{path: path, live: live, peak: peak}, nil
}

func (r *audioRecorder) discarded(live *liveSession, peak int) (capture, error) {
	if live != nil {
		live.drop()
	}
	return capture{peak: peak}, nil
}

func (r *audioRecorder) discard() {
	r.mu.Lock()
	r.active, r.frames, r.silenced = false, nil, nil
	live := r.live
	r.live = nil
	r.mu.Unlock()
	if live != nil {
		live.drop()
	}
}

func (r *audioRecorder) cancel() {
	r.discard()
	slog.Info("Voice recording cancelled")
}

// Discards the recording and frees the microphone
func (r *audioRecorder) shutdown() {
	r.discard()
	r.close(3 * time.Second)
	slog.Info("AudioRecorder shut down")
}

// The Termux:API recorder where it can capture, else PortAudio's
func newRecorder(live func(ctx context.Context, rate int) *liveSession) recorder {
	base := limits{threshold: speechconfig.DefaultSilence, duration: speechconfig.DefaultSilenceSecs, live: live}
	if termuxCapture() {
		return &termuxRecorder{limits: base}
	}
	return &audioRecorder{limits: base, rate: sampleRate}
}
