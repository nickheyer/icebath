package media

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"math"
	mrand "math/rand/v2"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/pkg/hostinfo"
	"github.com/nickheyer/icebath/pkg/internal/pycodecs"
	"github.com/nickheyer/icebath/pkg/internal/pydeadline"
	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pynumpy"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyplatform"
	"github.com/nickheyer/icebath/pkg/internal/pyshlex"
	"github.com/nickheyer/icebath/pkg/internal/pyshutil"
	"github.com/nickheyer/icebath/pkg/internal/pysounddevice"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	voiceSampleRate            = 16000
	voiceChannels              = 1
	voiceSampleWidth           = 2
	silenceRMSThreshold        = 200
	silenceDurationSeconds     = 3.0
	defaultBeepVolume          = 0.3
	termuxAPIAppMissingWarning = "Termux:API Android app is not installed. Install/update the Termux:API app to use termux-microphone-record."
)

var termuxAPIPackageProbes = [][]string{
	{"pm", "list", "packages", "com.termux.api"},
	{"cmd", "package", "list", "packages", "com.termux.api"},
}

var nativeSTTLabels = map[string]string{
	"local": "local faster-whisper", "local_command": "local command", "groq": "Groq", "openai": "OpenAI",
	"mistral": "Mistral Voxtral", "xai": "xAI Grok STT", "elevenlabs": "ElevenLabs Scribe", "deepinfra": "DeepInfra",
}

func voiceLog() *slog.Logger { return slog.Default().With("logger", "tools.voice_mode") }

// Audio import failure where this build has no dynamic library loader for PortAudio
type audioImportError struct{ msg string }

func (e *audioImportError) Error() string { return e.msg }

var (
	voiceTempDir = sync.OnceValue(func() string {
		dir, err := pypath.Gettempdir()
		if err != nil {
			panic(err)
		}
		return pypath.Join(dir, "icebath_voice")
	})
	importAudio = func() (pysounddevice.Module, error) {
		sd, err := pysounddevice.Import()
		var gate *pysounddevice.UnsupportedError
		if errors.As(err, &gate) {
			return nil, &audioImportError{msg: gate.Msg}
		}
		return sd, err
	}
	audioAvailable           = audioImportable
	voiceGetSTTProvider      = getProvider
	voiceGetSTTPlugin        = GetTranscriptionProvider
	detectAudioEnv           = DetectAudioEnvironment
	playAudioFileImpl        = playAudioFileFromDisk
	isTermuxEnvironment      = hostinfo.IsTermux
	voiceIsWSL               = hostinfo.IsWSL
	voiceIsContainer         = hostinfo.IsContainer
	voicePlatformSystem      = pyplatform.System
	voiceWhich               = func(cmd string) (string, bool) { return pyshutil.Which(cmd, nil) }
	voiceCheckOutput         = checkOutputQuiet
	runQuiet                 = runQuietCaptured
	startPlayerProcess       = startPlayer
	playInt16ViaTempfile     = playInt16ThroughFile
	soundDeviceOutputAllowed = func() bool { return voicePlatformSystem() != "Darwin" }
)

func isAudioImportError(err error) bool {
	var e *audioImportError
	return errors.As(err, &e)
}

func isAudioOSError(err error) bool {
	var e *pysounddevice.LibraryError
	return errors.As(err, &e)
}

// Little endian bytes of int16 samples, the numpy tobytes layout
func int16Bytes(samples []int16) []byte {
	out := make([]byte, 2*len(samples))
	for i, v := range samples {
		binary.LittleEndian.PutUint16(out[2*i:], uint16(v))
	}
	return out
}

// Plays int16 mono PCM through a temp WAV and the file players, the macOS path that avoids the TCC prompt
func playInt16ThroughFile(audio []int16, sampleRate int) {
	tmpPath := ""
	defer func() { unlinkQuietly(tmpPath) }()
	dir, err := pypath.Gettempdir()
	var f *os.File
	if err == nil {
		f, err = os.CreateTemp(dir, "tmp*.wav")
	}
	if err == nil {
		tmpPath = f.Name()
		err = writeWAVFrames(f, int16Bytes(audio), sampleRate)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}
	if err != nil {
		voiceLog().Debug(fmt.Sprintf("Tone tempfile playback failed: %s", err))
		return
	}
	PlayAudioFile(tmpPath)
}

// Writes raw 16 bit mono PCM frames as a WAV
func writeWAVFrames(w io.Writer, frames []byte, sampleRate int) error {
	_, err := w.Write(wrapPCMAsWAV(frames, sampleRate, voiceChannels, voiceSampleWidth))
	return err
}

// Removes a file, ignoring a missing or undeletable one
func unlinkQuietly(path string) {
	if path != "" {
		_ = os.Remove(path)
	}
}

// Why capture is unavailable, empty when the audio layer imports, an error for any other import failure
func audioUnavailableReason() (string, error) {
	_, err := importAudio()
	switch {
	case err == nil:
		return "", nil
	case isAudioImportError(err):
		return voiceCaptureInstallHint(err), nil
	case isAudioOSError(err):
		return strings.SplitN(portaudioMissingMessage(), "\n", 2)[0], nil
	}
	return "", err
}

// Whether the audio layer imports, an error for a failure that is neither an import nor a library error
func audioImportable() (bool, error) {
	_, err := importAudio()
	if err == nil {
		return true, nil
	}
	if isAudioImportError(err) || isAudioOSError(err) {
		return false, nil
	}
	return false, err
}

// Default input device rate rounded half to even, else the Whisper friendly 16 kHz
func defaultInputSamplerate(sd pysounddevice.Module) int {
	info, err := sd.QueryDevice(nil, "input")
	if err == nil && info.DefaultSamplerate > 0 && !math.IsInf(info.DefaultSamplerate, 1) {
		return int(math.RoundToEven(info.DefaultSamplerate))
	}
	return voiceSampleRate
}

// Why this build cannot load PortAudio, the Termux:API capture route on Termux
func voiceCaptureInstallHint(err error) string {
	if isTermuxEnvironment() {
		return "this build cannot load PortAudio on Termux, record through Termux:API instead: pkg install termux-api, then install the Termux:API Android app"
	}
	return err.Error()
}

// PortAudio install instructions for a missing shared library, one line per host the loader searches
func portaudioMissingMessage() string {
	hint := "  Linux:  sudo apt-get install libportaudio2\n  macOS:  brew install portaudio\n" +
		"  Windows: put portaudio.dll in a PATH directory or libportaudio64bit.dll beside icebath.exe"
	if isTermuxEnvironment() {
		hint = "  Termux: pkg install portaudio"
	}
	return "PortAudio system library not found -- install it first:\n" + hint + "\nThen retry /voice on."
}

// Path of termux-microphone-record on Termux, empty elsewhere
func termuxMicrophoneCommand() string {
	if !isTermuxEnvironment() {
		return ""
	}
	path, _ := voiceWhich("termux-microphone-record")
	return path
}

// Runs a command with captured UTF-8 output decoded with replacement and no stdin
func runQuietCaptured(cmd []string, timeout float64, check bool) (*pysubprocess.Completed, error) {
	result, err := pysubprocess.RunWith(context.Background(), cmd, pysubprocess.Options{
		Timeout: time.Duration(timeout * float64(time.Second)), Check: check})
	if result != nil {
		out, _ := pycodecs.DecodeReplace(result.Stdout, "utf-8")
		errOut, _ := pycodecs.DecodeReplace(result.Stderr, "utf-8")
		result.Stdout, result.Stderr = []byte(out), []byte(errOut)
	}
	return result, err
}

// Whether the Termux API app answers the package probes, trusting the recorder binary when both are inconclusive
func termuxAPIAppInstalled() bool {
	if !isTermuxEnvironment() {
		return false
	}
	inconclusive := false
	for _, cmd := range termuxAPIPackageProbes {
		result, err := runQuiet(append([]string(nil), cmd...), 5, false)
		if err != nil || result == nil || result.Returncode != 0 {
			inconclusive = true
			continue
		}
		if strings.Contains(pystr.Lower(string(result.Stdout)), "package:com.termux.api") {
			return true
		}
	}
	if _, ok := voiceWhich("termux-microphone-record"); inconclusive && ok {
		voiceLog().Debug("Termux package-manager probes inconclusive; trusting termux-microphone-record binary on PATH (issue #31015).")
		return true
	}
	return false
}

func termuxVoiceCaptureAvailable() bool {
	return termuxMicrophoneCommand() != "" && termuxAPIAppInstalled()
}

// Unix socket paths a PulseAudio or PipeWire server may listen on, from PULSE_SERVER and the runtime dirs
func pulseSocketCandidates() []string {
	var candidates []string
	for _, part := range strings.Split(os.Getenv("PULSE_SERVER"), ";") {
		if p := pystr.Strip(part); strings.HasPrefix(p, "unix:") {
			candidates = append(candidates, p[len("unix:"):])
		}
	}
	if runtimePath := os.Getenv("PULSE_RUNTIME_PATH"); runtimePath != "" {
		candidates = append(candidates, pypath.Join(runtimePath, "native"))
	}
	if xdg := os.Getenv("XDG_RUNTIME_DIR"); xdg != "" {
		candidates = append(candidates, pypath.Join(xdg, "pulse", "native"), pypath.Join(xdg, "pipewire-0"))
	}
	out := []string{}
	for _, c := range candidates {
		if c != "" {
			out = append(out, c)
		}
	}
	return out
}

// Whether a candidate path is a socket that accepts a connection within half a second
func pulseSocketReachable() bool {
	for _, path := range pulseSocketCandidates() {
		info, err := os.Stat(path)
		if err != nil || info.Mode()&fs.ModeSocket == 0 {
			continue
		}
		conn, err := net.DialTimeout("unix", path, 500*time.Millisecond)
		if err != nil {
			continue
		}
		conn.Close()
		return true
	}
	return false
}

// Appends the audio library verdict, Termux capture or host forwarding softening a failure into a notice
func probeAudioLibraries(warnings, notices *[]string, hasForwardedAudio bool, termuxMicCmd string, termuxAppInstalled bool) error {
	termuxCapture := termuxMicCmd != "" && termuxAppInstalled
	outcome := func(termuxNotice, warning, forwardedNotice string, importFailed bool) {
		switch {
		case forwardedNotice != "" && hasForwardedAudio:
			*notices = append(*notices, forwardedNotice)
		case termuxCapture:
			*notices = append(*notices, termuxNotice)
		case importFailed && termuxMicCmd != "" && !termuxAppInstalled:
			*warnings = append(*warnings, termuxAPIAppMissingWarning)
		default:
			*warnings = append(*warnings, warning)
		}
	}
	sd, err := importAudio()
	if err != nil {
		switch {
		case isAudioImportError(err):
			outcome("Termux:API microphone recording available (PortAudio not required)",
				"PortAudio cannot be loaded ("+voiceCaptureInstallHint(err)+")", "", true)
			return nil
		case isAudioOSError(err):
			outcome("Termux:API microphone recording available (PortAudio not required)", portaudioMissingMessage(), "", true)
			return nil
		}
		return err
	}
	devices, err := sd.QueryDevices()
	if err != nil {
		outcome("PortAudio device query failed, but Termux:API microphone capture is available",
			"Audio subsystem error (PortAudio cannot query devices)",
			"Audio device query failed but host audio forwarding is configured -- continuing", false)
		return nil
	}
	if len(devices) > 0 {
		return nil
	}
	outcome("No PortAudio devices detected, but Termux:API microphone capture is available",
		"No audio input/output devices detected",
		"No PortAudio devices detected but host audio forwarding is configured -- continuing", false)
	return nil
}

// AudioEnvironment is the detect_audio_environment verdict
type AudioEnvironment struct {
	Available bool     `json:"available"`
	Warnings  []string `json:"warnings"`
	Notices   []string `json:"notices"`
}

// Whether this host can record and play audio, with blocking warnings and informational notices
func DetectAudioEnvironment() (AudioEnvironment, error) {
	warnings, notices := []string{}, []string{}
	termuxMicCmd := termuxMicrophoneCommand()
	termuxAppInstalled := termuxAPIAppInstalled()
	hasForwardedAudio := os.Getenv("PULSE_SERVER") != "" || os.Getenv("PIPEWIRE_REMOTE") != "" || pulseSocketReachable()
	report := func(notice, warning string) {
		if hasForwardedAudio {
			notices = append(notices, notice)
		} else {
			warnings = append(warnings, warning)
		}
	}
	if os.Getenv("SSH_CLIENT") != "" || os.Getenv("SSH_TTY") != "" || os.Getenv("SSH_CONNECTION") != "" {
		report("Running over SSH with a reachable PulseAudio/PipeWire sound server",
			"Running over SSH -- no audio devices available.\n"+
				"  If a sound server (PulseAudio/PipeWire) is running on this host,\n"+
				"  point Icebath at it, e.g.:\n"+
				"    export XDG_RUNTIME_DIR=/run/user/$(id -u)\n"+
				"    # or: export PULSE_SERVER=unix:$XDG_RUNTIME_DIR/pulse/native")
	}
	if voiceIsContainer() {
		report("Running inside container (Docker/Podman/LXC) with host audio forwarding",
			"Running inside container (Docker/Podman/LXC) -- no audio devices.\n"+
				"  Forward host audio with one of (substitute $XDG_RUNTIME_DIR for your runtime dir,\n"+
				"  typically /run/user/$UID):\n"+
				"    PulseAudio:  -v $XDG_RUNTIME_DIR/pulse/native:$XDG_RUNTIME_DIR/pulse/native \\\n"+
				"                 -e PULSE_SERVER=unix:$XDG_RUNTIME_DIR/pulse/native\n"+
				"    PipeWire:    -e PIPEWIRE_REMOTE=$XDG_RUNTIME_DIR/pipewire-0")
	}
	if voiceIsWSL() {
		switch {
		case hasForwardedAudio:
			notices = append(notices, "Running in WSL with a reachable PulseAudio/PipeWire sound server")
		case wslPowershellTTSAvailable():
			notices = append(notices, "Running in WSL without a PulseAudio bridge -- TTS playback "+
				"will use the PowerShell/Media.SoundPlayer fallback. "+
				"Voice INPUT (recording) still requires a PulseAudio bridge:\n"+
				"  1. Set PULSE_SERVER=unix:/mnt/wslg/PulseServer\n"+
				"  2. Create ~/.asoundrc pointing ALSA at PulseAudio\n"+
				"  3. Verify with: arecord -d 3 test.wav && aplay test.wav")
		default:
			warnings = append(warnings, "Running in WSL -- audio requires a forwarded sound server.\n"+
				"  PulseAudio: export PULSE_SERVER=unix:/mnt/wslg/PulseServer\n"+
				"  PipeWire:   export PIPEWIRE_REMOTE=$XDG_RUNTIME_DIR/pipewire-0\n"+
				"  Then verify: arecord -d 3 test.wav && aplay test.wav")
		}
	}
	if err := probeAudioLibraries(&warnings, &notices, hasForwardedAudio, termuxMicCmd, termuxAppInstalled); err != nil {
		return AudioEnvironment{}, err
	}
	return AudioEnvironment{Available: len(warnings) == 0, Warnings: warnings, Notices: notices}, nil
}

// Configured voice.beep_volume in 0..1, the default for a missing, non numeric, boolean or out of range value
func getBeepVolume(ctx context.Context) float64 {
	raw, ok := voiceConfig(ctx)["beep_volume"]
	if !ok {
		raw = defaultBeepVolume
	}
	volume, err := pyint.Float(raw)
	if err != nil {
		return defaultBeepVolume
	}
	if _, isBool := raw.(bool); isBool || volume < 0.0 || volume > 1.0 || math.IsNaN(volume) {
		return defaultBeepVolume
	}
	return volume
}

// Plays through sounddevice and polls until the stream goes idle or the timeout passes, then stops it
func sdPlayBlocking(sd pysounddevice.Module, audio []int16, sampleRate int, timeout float64, blocksize int) error {
	if err := sd.Play(audio, float64(sampleRate), blocksize); err != nil {
		return err
	}
	var pollErr error
	pydeadline.PollUntil(func() (bool, bool) {
		stream, err := sd.GetStream()
		if err != nil {
			pollErr = err
			return true, true
		}
		active, err := stream.Active()
		if err != nil {
			pollErr = err
			return true, true
		}
		return !active, !active
	}, time.Duration(timeout*float64(time.Second)), 10*time.Millisecond)
	if pollErr != nil {
		return pollErr
	}
	sd.Stop()
	return nil
}

// Beep tones with a short fade at both ends and 60 ms gaps, at the configured beep volume
func beepSamples(frequency int, duration float64, count int, beepVolume float64) ([]int16, error) {
	rate := float64(voiceSampleRate)
	samplesPerBeep := int(rate * duration)
	gap := make([]int16, int(rate*0.06))
	var audio []int16
	for i := 0; i < count; i++ {
		t, err := pynumpy.Linspace(0, duration, samplesPerBeep, false)
		if err != nil {
			return nil, err
		}
		omega := float64(2*math.Pi) * float64(frequency)
		tone := make([]float64, len(t))
		for j, v := range t {
			tone[j] = math.Sin(float64(omega * v))
		}
		fadeLen := min(int(rate*0.01), pyFloorDiv(samplesPerBeep, 4))
		up, err := pynumpy.Linspace(0, 1, fadeLen, true)
		if err != nil {
			return nil, err
		}
		down, _ := pynumpy.Linspace(1, 0, fadeLen, true)
		if err := mulInPlace(tone[:min(fadeLen, len(tone))], up); err != nil {
			return nil, err
		}
		tail := tone[max(0, len(tone)-fadeLen):]
		if fadeLen == 0 {
			tail = tone
		}
		if err := mulInPlace(tail, down); err != nil {
			return nil, err
		}
		for _, v := range tone {
			audio = append(audio, int16(float64(float64(v*beepVolume)*32767)))
		}
		if i < count-1 {
			audio = append(audio, gap...)
		}
	}
	if audio == nil && count <= 0 {
		return nil, &pynumpy.ValueError{Msg: "need at least one array to concatenate"}
	}
	return audio, nil
}

// Python floor division of two ints
func pyFloorDiv(a, b int) int {
	q := a / b
	if (a%b != 0) && ((a < 0) != (b < 0)) {
		q--
	}
	return q
}

// In place numpy multiply of a slice window by factors, a shape mismatch failing like broadcasting
func mulInPlace(window, factors []float64) error {
	switch {
	case len(window) == len(factors):
		for i := range factors {
			window[i] *= factors[i]
		}
	case len(factors) == 1:
		for i := range window {
			window[i] *= factors[0]
		}
	case len(window) == 1:
		return &pynumpy.ValueError{Msg: fmt.Sprintf("non-broadcastable output operand with shape (1,) doesn't match the broadcast shape (%d,)", len(factors))}
	default:
		return &pynumpy.ValueError{Msg: fmt.Sprintf("operands could not be broadcast together with shapes (%d,) (%d,) (%d,) ", len(window), len(factors), len(window))}
	}
	return nil
}

// PlayBeep plays count sine beeps at the configured volume, through a temp file on macOS
func PlayBeep(ctx context.Context, frequency int, duration float64, count int) {
	audio, err := beepSamples(frequency, duration, count, getBeepVolume(ctx))
	if err != nil {
		voiceLog().Debug(fmt.Sprintf("Beep playback failed: %s", err))
		return
	}
	if !soundDeviceOutputAllowed() {
		playInt16ViaTempfile(audio, voiceSampleRate)
		return
	}
	sd, err := importAudio()
	if err != nil {
		if !isAudioImportError(err) && !isAudioOSError(err) {
			voiceLog().Debug(fmt.Sprintf("Beep playback failed: %s", err))
		}
		return
	}
	if err := sdPlayBlocking(sd, audio, voiceSampleRate, 2.0, 0); err != nil {
		voiceLog().Debug(fmt.Sprintf("Beep playback failed: %s", err))
	}
}

var (
	audioOutputMu     sync.Mutex
	audioOutputActive int
	thinkingMu        sync.Mutex
	thinkingStop      *pythread.Event
)

// MarkAudioOutputActive counts one real speaker playback in or out, never below zero
func MarkAudioOutputActive(active bool) {
	audioOutputMu.Lock()
	defer audioOutputMu.Unlock()
	delta := -1
	if active {
		delta = 1
	}
	audioOutputActive = max(0, audioOutputActive+delta)
}

// IsAudioOutputActive reports whether any real speaker playback is in flight
func IsAudioOutputActive() bool {
	audioOutputMu.Lock()
	defer audioOutputMu.Unlock()
	return audioOutputActive > 0
}

// Whether voice.thinking_sound allows the ambient thinking blips, true by default
func thinkingSoundEnabled(ctx context.Context) (enabled bool) {
	defer func() {
		if recover() != nil {
			enabled = true
		}
	}()
	value, ok := voiceConfig(ctx)["thinking_sound"]
	if !ok || value == nil {
		return true
	}
	return provider.IsTruthyValue(value)
}

// A soft falling water drop blip at a quieter volume than the beeps
func synthThinkingBlip(ctx context.Context, frequency float64) []int16 {
	rate := float64(voiceSampleRate)
	duration := 0.16
	n := int(rate * duration)
	t, _ := pynumpy.Linspace(0, duration, n, false)
	glide, _ := pynumpy.Linspace(1.0, 0.72, n, true)
	tau := float64(2 * math.Pi)
	cumulative := 0.0
	tone := make([]float64, n)
	for i := range tone {
		cumulative += float64(frequency * glide[i])
		phase := float64(tau*cumulative) / rate
		tone[i] = float64(0.8*math.Sin(phase)) + float64(0.2*math.Sin(phase/2.0))
	}
	attack := int(0.02 * rate)
	env := make([]float64, n)
	for i := range env {
		env[i] = 1
	}
	ramp, _ := pynumpy.Linspace(0.0, 1.0, attack, true)
	copy(env, ramp)
	for i := range env {
		env[i] *= math.Exp(float64(-t[i] * 14.0))
	}
	volume := getBeepVolume(ctx) * 0.5
	out := make([]int16, n)
	for i := range out {
		out[i] = int16(float64(float64(float64(tone[i]*env[i])*volume) * 32767))
	}
	return out
}

// Alternates the two blips until stopped, each blip gated by shouldPlay, silent on macOS
func thinkingSoundLoop(ctx context.Context, stop *pythread.Event, shouldPlay func() bool) {
	if !soundDeviceOutputAllowed() {
		return
	}
	sd, err := importAudio()
	if err != nil {
		if !isAudioImportError(err) && !isAudioOSError(err) {
			voiceLog().Error(fmt.Sprintf("Exception in thread voice-thinking-sound: %s", err))
		}
		return
	}
	blips := [][]int16{synthThinkingBlip(ctx, 392.0), synthThinkingBlip(ctx, 329.6)}
	i := 0
	for !stop.IsSet() {
		if err := playThinkingBlip(sd, stop, shouldPlay, blips, &i); err != nil {
			voiceLog().Debug(fmt.Sprintf("Thinking sound blip failed: %s", err))
			return
		}
		stop.Wait(0.8 + mrand.Float64()*0.4)
	}
}

// Plays the next blip when shouldPlay allows, a panic in the gate or player returned as the error
func playThinkingBlip(sd pysounddevice.Module, stop *pythread.Event, shouldPlay func() bool, blips [][]int16, i *int) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%v", r)
		}
	}()
	if shouldPlay != nil && !shouldPlay() {
		return nil
	}
	blip := blips[*i%len(blips)]
	*i++
	if err := sd.Play(blip, voiceSampleRate, 0); err != nil {
		return err
	}
	stop.Wait(float64(len(blip))/voiceSampleRate + 0.02)
	sd.Stop()
	return nil
}

// StartThinkingSound starts the ambient blip loop once, false when voice.thinking_sound disables it
func StartThinkingSound(ctx context.Context, shouldPlay func() bool) bool {
	if !thinkingSoundEnabled(ctx) {
		return false
	}
	thinkingMu.Lock()
	if thinkingStop != nil && !thinkingStop.IsSet() {
		thinkingMu.Unlock()
		return true
	}
	stop := &pythread.Event{}
	thinkingStop = stop
	thinkingMu.Unlock()
	go thinkingSoundLoop(ctx, stop, shouldPlay)
	return true
}

// StopThinkingSound stops the blip loop at once, a no op when none runs
func StopThinkingSound() {
	thinkingMu.Lock()
	stop := thinkingStop
	thinkingStop = nil
	thinkingMu.Unlock()
	if stop != nil {
		stop.Set()
	}
}

// A player process started by the file playback chain
type playbackProcess interface {
	Poll() (int, bool)
	Terminate() error
	Kill() error
	Wait(timeout time.Duration) (int, error)
}

type playerProc struct {
	cmd  *exec.Cmd
	argv []string
	done chan struct{}
	code int
}

// Starts a player with no stdio and the given environment
func startPlayer(argv []string, env map[string]string) (playbackProcess, error) {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = make([]string, 0, len(env))
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	p := &playerProc{cmd: cmd, argv: argv, done: make(chan struct{})}
	go func() {
		err := cmd.Wait()
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			p.code = pysubprocess.Returncode(exitErr)
		} else if err != nil {
			p.code = -1
		}
		close(p.done)
	}()
	return p, nil
}

func (p *playerProc) Poll() (int, bool) {
	select {
	case <-p.done:
		return p.code, true
	default:
		return 0, false
	}
}

// SIGTERM on POSIX, TerminateProcess on Windows
func (p *playerProc) Terminate() error {
	if runtime.GOOS == "windows" {
		return p.cmd.Process.Kill()
	}
	return p.cmd.Process.Signal(syscall.SIGTERM)
}

func (p *playerProc) Kill() error { return p.cmd.Process.Kill() }

// Exit code once the player ends, a TimeoutExpired error when a positive timeout passes first
func (p *playerProc) Wait(timeout time.Duration) (int, error) {
	if timeout <= 0 {
		<-p.done
		return p.code, nil
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-p.done:
		return p.code, nil
	case <-timer.C:
		secs := timeout.Seconds()
		return 0, &pysubprocess.TimeoutExpired{Cmd: p.argv, Timeout: int(secs), TimeoutFloat: &secs}
	}
}

var (
	playbackMu     sync.Mutex
	activePlayback playbackProcess
)

func setActivePlayback(proc playbackProcess) {
	playbackMu.Lock()
	activePlayback = proc
	playbackMu.Unlock()
}

// StopPlayback terminates the running player process and stops sounddevice playback
func StopPlayback() {
	playbackMu.Lock()
	proc := activePlayback
	activePlayback = nil
	playbackMu.Unlock()
	if proc != nil {
		if _, exited := proc.Poll(); !exited && proc.Terminate() == nil {
			voiceLog().Info("Audio playback interrupted")
		}
	}
	if sd, err := importAudio(); err == nil {
		sd.Stop()
	}
}

// Whether WSL can play TTS through PowerShell and ffmpeg without a PulseAudio bridge
func wslPowershellTTSAvailable() bool {
	if !voiceIsWSL() {
		return false
	}
	_, ps := voiceWhich("powershell.exe")
	_, ff := voiceWhich("ffmpeg")
	return ps && ff
}

// PlayAudioFile plays a file through sounddevice or the system players, counted as speaker output throughout
func PlayAudioFile(filePath string) bool {
	MarkAudioOutputActive(true)
	defer MarkAudioOutputActive(false)
	return playAudioFileImpl(filePath)
}

// WAV frames and frame rate read the way the wave module reads a PCM file
func readWAVFrames(path string) ([]byte, int, error) {
	w, err := ReadWave(path)
	if err != nil {
		return nil, 0, err
	}
	return w.Frames, w.FrameRate, nil
}

// Plays a WAV through sounddevice, faded in with silence padding and 4096 frame blocks under WSL
func playWAVViaSoundDevice(filePath string) bool {
	sd, err := importAudio()
	if err == nil {
		var frames []byte
		var sampleRate int
		frames, sampleRate, err = readWAVFrames(filePath)
		if err == nil {
			err = playWAVFrames(sd, frames, sampleRate)
		}
	}
	if err == nil {
		return true
	}
	var pathErr *fs.PathError
	if !isAudioImportError(err) && !isAudioOSError(err) && !errors.As(err, &pathErr) {
		voiceLog().Debug(fmt.Sprintf("sounddevice playback failed: %s", err))
	}
	return false
}

// Plays decoded int16 frames, padded and faded in under WSL
func playWAVFrames(sd pysounddevice.Module, frames []byte, sampleRate int) error {
	if len(frames)%2 != 0 {
		return errors.New("buffer size must be a multiple of element size")
	}
	audio := make([]int16, len(frames)/2)
	for i := range audio {
		audio[i] = int16(binary.LittleEndian.Uint16(frames[2*i:]))
	}
	rate := float64(sampleRate)
	blocksize := 0
	if voiceIsWSL() {
		fadeSamples := int(0.1 * rate)
		fade, err := pynumpy.Linspace(0.0, 1.0, fadeSamples, true)
		if err != nil {
			return err
		}
		audioFloat := make([]float64, len(audio))
		for i, v := range audio {
			audioFloat[i] = float64(v)
		}
		if err := mulInPlace(audioFloat[:min(fadeSamples, len(audioFloat))], fade); err != nil {
			return err
		}
		padded := make([]int16, int(0.1*rate), int(0.1*rate)+len(audio)+int(0.05*rate))
		for _, v := range audioFloat {
			padded = append(padded, int16(v))
		}
		audio = append(padded, make([]int16, int(0.05*rate))...)
		blocksize = 4096
	}
	if sampleRate == 0 {
		return errors.New("division by zero")
	}
	return sdPlayBlocking(sd, audio, sampleRate, float64(len(audio))/rate+2.0, blocksize)
}

// Captured stdout of a helper command, an error for a failure, a nonzero exit or a timeout
func checkOutputQuiet(cmd []string, timeout float64) ([]byte, error) {
	result, err := pysubprocess.RunWith(context.Background(), cmd, pysubprocess.Options{
		Timeout: time.Duration(timeout * float64(time.Second)), Check: true})
	if err != nil {
		return nil, err
	}
	return result.Stdout, nil
}

// The /bin/sh pipeline converting to a Windows temp WAV and playing it through Media.SoundPlayer, nil off WSL
func wslPowershellPlayerCmd(filePath string) []string {
	_, ps := voiceWhich("powershell.exe")
	_, ff := voiceWhich("ffmpeg")
	if !(ps && ff && voiceIsWSL()) {
		return nil
	}
	out := func(cmd []string) (string, error) {
		raw, err := voiceCheckOutput(cmd, 3)
		if err != nil {
			return "", err
		}
		text, _ := pycodecs.DecodeReplace(raw, "utf-8")
		return pystr.Strip(text), nil
	}
	winTemp, err := out([]string{"cmd.exe", "/c", "echo %TEMP%"})
	if err != nil {
		return nil
	}
	winTmpWSL, err := out([]string{"wslpath", "-u", winTemp})
	if err != nil || winTmpWSL == "" {
		return nil
	}
	var id [4]byte
	if _, err := rand.Read(id[:]); err != nil {
		return nil
	}
	wslWAV := pypath.Join(winTmpWSL, "icebath-tts-"+hex.EncodeToString(id[:])+".wav")
	winWAV, err := out([]string{"wslpath", "-w", wslWAV})
	if err != nil || winWAV == "" {
		return nil
	}
	psScript := fmt.Sprintf("(New-Object Media.SoundPlayer '%s').PlaySync()", strings.ReplaceAll(winWAV, "'", "''"))
	psCmd := strings.Join([]string{
		pyshlex.Join([]string{"ffmpeg", "-i", filePath, "-f", "wav", wslWAV, "-loglevel", "quiet", "-y"}),
		pyshlex.Join([]string{"powershell.exe", "-NoProfile", "-Command", psScript})}, " && ")
	return []string{"/bin/sh", "-c", fmt.Sprintf("( %s ); rc=$?; %s; exit $rc", psCmd, pyshlex.Join([]string{"rm", "-f", wslWAV}))}
}

// Player commands in preference order for this platform
func systemPlayerCandidates(filePath string) [][]string {
	system := voicePlatformSystem()
	var players [][]string
	if system == "Darwin" {
		players = append(players, []string{"afplay", filePath})
	}
	if system == "Linux" {
		if ps := wslPowershellPlayerCmd(filePath); ps != nil {
			players = append(players, ps)
		}
	}
	players = append(players, []string{"ffplay", "-nodisp", "-autoexit", "-loglevel", "quiet", filePath})
	if system == "Linux" {
		players = append(players, []string{"aplay", "-q", filePath})
	}
	return players
}

// Credential scrubbed child environment, the damaged runtime record error returned instead of raised
func playerEnv() (env map[string]string, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%v", r)
		}
	}()
	return tools.IcebathSubprocessEnv(nil, false, nil), nil
}

// Runs one player to completion as the interruptible active playback, true only for a zero exit
func runSystemPlayer(cmd []string) bool {
	defer setActivePlayback(nil)
	env, err := playerEnv()
	var proc playbackProcess
	if err == nil {
		proc, err = startPlayerProcess(cmd, env)
	}
	if err != nil {
		voiceLog().Debug(fmt.Sprintf("System player %s failed: %s", cmd[0], err))
		return false
	}
	setActivePlayback(proc)
	rc, err := proc.Wait(300 * time.Second)
	var timeout *pysubprocess.TimeoutExpired
	switch {
	case errors.As(err, &timeout):
		voiceLog().Warn(fmt.Sprintf("System player %s timed out, killing process", cmd[0]))
		_ = proc.Kill()
		_, _ = proc.Wait(0)
		return false
	case err != nil:
		voiceLog().Debug(fmt.Sprintf("System player %s failed: %s", cmd[0], err))
		return false
	case rc == 0:
		return true
	}
	voiceLog().Debug(fmt.Sprintf("System player %s exited with code %d, trying next", cmd[0], rc))
	return false
}

// Plays an existing file through sounddevice for WAV, then each available system player in turn
func playAudioFileFromDisk(filePath string) bool {
	if info, err := os.Stat(filePath); err != nil || !info.Mode().IsRegular() {
		voiceLog().Warn(fmt.Sprintf("Audio file not found: %s", filePath))
		return false
	}
	if strings.HasSuffix(filePath, ".wav") && soundDeviceOutputAllowed() && playWAVViaSoundDevice(filePath) {
		return true
	}
	for _, cmd := range systemPlayerCandidates(filePath) {
		if _, ok := voiceWhich(cmd[0]); ok && runSystemPlayer(cmd) {
			return true
		}
	}
	voiceLog().Warn(fmt.Sprintf("No audio player available for %s", filePath))
	return false
}

// Whether a plugin STT provider of that name is registered and available, discovery retried once
func checkPluginSTTProvider(ctx context.Context, name string) bool {
	key := pystr.Strip(pystr.Lower(name))
	if key == "" || key == "none" {
		return false
	}
	var p TranscriptionProvider
	if err := pluginCall(func() error {
		sttEnsurePluginsDiscovered(ctx, false)
		p = voiceGetSTTPlugin(ctx, key, "")
		if p == nil {
			sttEnsurePluginsDiscovered(ctx, true)
			p = voiceGetSTTPlugin(ctx, key, "")
		}
		return nil
	}); err != nil {
		voiceLog().Debug(fmt.Sprintf("STT plugin requirements check skipped for '%s': %s", key, err))
		return false
	}
	if p == nil {
		return false
	}
	available := false
	if err := pluginCall(func() error {
		available = p.IsAvailable()
		return nil
	}); err != nil {
		voiceLog().Warn(fmt.Sprintf("STT plugin provider '%s' is_available() raised during requirements check: %s - treating as unavailable", key, err))
		return false
	}
	return available
}

// VoiceRequirements is the check_voice_requirements verdict
type VoiceRequirements struct {
	Available      bool             `json:"available"`
	AudioAvailable bool             `json:"audio_available"`
	STTAvailable   bool             `json:"stt_available"`
	Details        string           `json:"details"`
	Environment    AudioEnvironment `json:"environment"`
}

// CheckVoiceRequirements reports whether capture, an STT provider and the host environment allow voice mode
func CheckVoiceRequirements(ctx context.Context) (VoiceRequirements, error) {
	sttConfig := loadSTTConfig(ctx)
	sttEnabled := isSTTEnabled(ctx, sttConfig)
	sttProvider, err := voiceGetSTTProvider(ctx, sttConfig)
	if err != nil {
		return VoiceRequirements{}, err
	}
	sttLabel := ""
	if label, ok := nativeSTTLabels[sttProvider]; ok {
		sttLabel = "OK (" + label + ")"
	} else if sttEnabled && resolveCommandSTTProviderConfig(sttProvider, sttConfig) != nil {
		sttLabel = "OK (command: " + sttProvider + ")"
	} else if sttEnabled && checkPluginSTTProvider(ctx, sttProvider) {
		sttLabel = "OK (plugin: " + sttProvider + ")"
	}
	sttAvailable := sttEnabled && sttLabel != ""
	termuxCapture := termuxVoiceCaptureAvailable()
	audioOK, err := audioAvailable()
	if err != nil {
		return VoiceRequirements{}, err
	}
	hasAudio := audioOK || termuxCapture
	envCheck, err := detectAudioEnv()
	if err != nil {
		return VoiceRequirements{}, err
	}
	capture := "Audio capture: OK"
	switch {
	case termuxCapture:
		capture = "Audio capture: OK (Termux:API microphone)"
	case !hasAudio:
		reason, err := audioUnavailableReason()
		if err != nil {
			return VoiceRequirements{}, err
		}
		capture = "Audio capture: MISSING (" + reason + ")"
	}
	stt := "STT provider: MISSING (run `icebath tools` and configure Speech-to-Text: Local Whisper or a cloud provider)"
	if !sttEnabled {
		stt = "STT provider: DISABLED in config (stt.enabled: false)"
	} else if sttLabel != "" {
		stt = "STT provider: " + sttLabel
	}
	details := []string{capture, stt}
	for _, w := range envCheck.Warnings {
		details = append(details, "Environment: "+w)
	}
	for _, n := range envCheck.Notices {
		details = append(details, "Environment: "+n)
	}
	return VoiceRequirements{Available: hasAudio && sttAvailable && envCheck.Available, AudioAvailable: hasAudio,
		STTAvailable: sttAvailable, Details: strings.Join(details, "\n"), Environment: envCheck}, nil
}

// CleanupTempRecordings deletes voice recordings older than maxAgeSeconds and returns how many went
func CleanupTempRecordings(maxAgeSeconds int) int {
	deleted := pyfs.UnlinkFilesOlderThan(voiceTempDir(), "recording_*.wav", float64(maxAgeSeconds), nil)
	if deleted > 0 {
		voiceLog().Debug(fmt.Sprintf("Cleaned up %d old voice recordings", deleted))
	}
	return deleted
}
