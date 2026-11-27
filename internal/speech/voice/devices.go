package voice

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"math"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/speech/portaudio"
	"github.com/nickheyer/icebath/internal/terminal"
)

// Capture format every recording is made in
const (
	sampleRate = 16000
	channels   = 1
	sampleBits = 2
	// Level under which a block of audio counts as silence
	silenceRMS = 200
)

const termuxAppMissing = "The Termux:API app for Android is not installed, and recording needs it. Install or update the Termux:API app."

// Package probes of the Termux:API app
var termuxProbes = [][]string{{"pm", "list", "packages", "com.termux.api"}, {"cmd", "package", "list", "packages", "com.termux.api"}}

// Notice of recording through Termux:API where PortAudio can't load
const termuxRecords = "Recording works through Termux:API, so PortAudio is not needed."

// PortAudio of this process, the reason it can't load kept apart from other failures
func audio() (portaudio.Audio, error) {
	return portaudio.Open()
}

// Whether a failure is this build lacking a loader for PortAudio
func unloadable(err error) bool {
	return errors.Is(err, portaudio.ErrUnsupported)
}

// Whether a failure is PortAudio missing from this machine
func missingLibrary(err error) bool {
	_, ok := errors.AsType[*portaudio.LibraryError](err)
	return ok
}

func termux() bool {
	return os.Getenv("TERMUX_VERSION") != "" || strings.Contains(os.Getenv("PREFIX"), "com.termux/files/usr")
}

var wslKernel = sync.OnceValue(func() bool {
	data, err := os.ReadFile("/proc/version")
	return err == nil && strings.Contains(strings.ToLower(string(data)), "microsoft")
})

func wsl() bool {
	return runtime.GOOS == "linux" && (os.Getenv("WSL_DISTRO_NAME") != "" || wslKernel())
}

func found(name string) (string, bool) {
	path, err := exec.LookPath(name)
	return path, err == nil
}

// Why this build can't load PortAudio, the Termux:API route on Termux
func captureHint(err error) string {
	if termux() {
		return "this build of icebath can't load PortAudio on Termux, so record through Termux:API instead: run pkg install termux-api, then install the Termux:API app for Android"
	}
	return err.Error()
}

// Text with its first letter in capitals
func capital(text string) string {
	if text == "" {
		return ""
	}
	return strings.ToUpper(text[:1]) + text[1:]
}

// How to install PortAudio, a line per kind of machine the loader searches
func portaudioMissing() string {
	hint := "  Linux:    sudo apt-get install libportaudio2\n  macOS:    brew install portaudio\n" +
		"  Windows:  put portaudio.dll in a folder on PATH, or libportaudio64bit.dll next to icebath.exe"
	if termux() {
		hint = "  Termux:   pkg install portaudio"
	}
	return "PortAudio, the sound library icebath records with, is not installed. Install it:\n" + hint + "\nThen run /voice on again."
}

// termux-microphone-record on Termux, empty elsewhere
func termuxMic() string {
	if !termux() {
		return ""
	}
	path, _ := found("termux-microphone-record")
	return path
}

// Output of a short program, nil when it fails
func output(argv []string, timeout time.Duration) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdin = nil
	return cmd.Output()
}

// Whether the Termux:API app answers the package probes, the recorder on PATH trusted when both are inconclusive
func termuxApp() bool {
	if !termux() {
		return false
	}
	inconclusive := false
	for _, argv := range termuxProbes {
		out, err := output(argv, 5*time.Second)
		if err != nil {
			inconclusive = true
			continue
		}
		if strings.Contains(strings.ToLower(string(out)), "package:com.termux.api") {
			return true
		}
	}
	if _, ok := found("termux-microphone-record"); inconclusive && ok {
		slog.Debug("Termux package-manager probes inconclusive; trusting termux-microphone-record binary on PATH.")
		return true
	}
	return false
}

func termuxCapture() bool {
	return termuxMic() != "" && termuxApp()
}

// Unix sockets a PulseAudio or PipeWire server may listen on
func pulseSockets() []string {
	var out []string
	for _, part := range strings.Split(os.Getenv("PULSE_SERVER"), ";") {
		if p := strings.TrimSpace(part); strings.HasPrefix(p, "unix:") {
			out = append(out, strings.TrimPrefix(p, "unix:"))
		}
	}
	if dir := os.Getenv("PULSE_RUNTIME_PATH"); dir != "" {
		out = append(out, filepath.Join(dir, "native"))
	}
	if xdg := os.Getenv("XDG_RUNTIME_DIR"); xdg != "" {
		out = append(out, filepath.Join(xdg, "pulse", "native"), filepath.Join(xdg, "pipewire-0"))
	}
	return out
}

// Whether a sound server socket takes a connection within half a second
func pulseReachable() bool {
	for _, path := range pulseSockets() {
		if info, err := os.Stat(path); err != nil || info.Mode()&fs.ModeSocket == 0 {
			continue
		}
		if conn, err := net.DialTimeout("unix", path, 500*time.Millisecond); err == nil {
			conn.Close()
			return true
		}
	}
	return false
}

// Whether this machine can record and play audio, what blocks it and what is worth knowing
type Environment struct {
	Available bool
	Warnings  []string
	Notices   []string
}

// Audio library verdict, Termux capture or a forwarded sound server softening a failure into a notice
func probeLibraries(env *Environment, forwarded bool, recorder string, app bool) {
	capture := recorder != "" && app
	outcome := func(termuxNotice, warning, forwardedNotice string, importFailed bool) {
		switch {
		case forwardedNotice != "" && forwarded:
			env.Notices = append(env.Notices, forwardedNotice)
		case capture:
			env.Notices = append(env.Notices, termuxNotice)
		case importFailed && recorder != "" && !app:
			env.Warnings = append(env.Warnings, termuxAppMissing)
		default:
			env.Warnings = append(env.Warnings, warning)
		}
	}
	pa, err := audio()
	if err != nil {
		switch {
		case unloadable(err):
			outcome(termuxRecords, capital(captureHint(err))+".", "", true)
		case missingLibrary(err):
			outcome(termuxRecords, portaudioMissing(), "", true)
		default:
			outcome("PortAudio failed to start, but recording works through Termux:API.", "The sound system failed: "+
				strings.TrimSuffix(err.Error(), ".")+".", "", true)
		}
		return
	}
	devices, err := pa.Devices()
	if err != nil {
		outcome("PortAudio could not list the sound devices, but recording works through Termux:API.",
			"The sound system failed, because PortAudio can't list the sound devices.",
			"PortAudio could not list the sound devices, but sound comes from the host, so voice mode goes on.", false)
		return
	}
	if len(devices) == 0 {
		outcome("PortAudio found no sound devices, but recording works through Termux:API.", "No microphone or speaker was found.",
			"PortAudio found no sound devices, but sound comes from the host, so voice mode goes on.", false)
	}
}

// Whether this machine can record and play audio, with what blocks it and what only informs
func Detect() Environment {
	env := Environment{}
	recorder, app := termuxMic(), termuxApp()
	forwarded := os.Getenv("PULSE_SERVER") != "" || os.Getenv("PIPEWIRE_REMOTE") != "" || pulseReachable()
	report := func(notice, warning string) {
		if forwarded {
			env.Notices = append(env.Notices, notice)
		} else {
			env.Warnings = append(env.Warnings, warning)
		}
	}
	if os.Getenv("SSH_CLIENT") != "" || os.Getenv("SSH_TTY") != "" || os.Getenv("SSH_CONNECTION") != "" {
		report("icebath runs over SSH, and it can reach a PulseAudio or PipeWire sound server.",
			"icebath runs over SSH, so it has no sound devices.\n  If a PulseAudio or PipeWire sound server runs on this computer, point icebath at it, such as with\n"+
				"    export XDG_RUNTIME_DIR=/run/user/$(id -u)\n  or\n    export PULSE_SERVER=unix:$XDG_RUNTIME_DIR/pulse/native")
	}
	if terminal.InContainer() {
		report("icebath runs in a container, such as Docker, Podman or LXC, and sound comes from the host.",
			"icebath runs in a container, such as Docker, Podman or LXC, so it has no sound devices.\n"+
				"  Pass the sound of the host in with one of these, where $XDG_RUNTIME_DIR is usually /run/user/$UID:\n"+
				"    PulseAudio:  -v $XDG_RUNTIME_DIR/pulse/native:$XDG_RUNTIME_DIR/pulse/native \\\n"+
				"                 -e PULSE_SERVER=unix:$XDG_RUNTIME_DIR/pulse/native\n    PipeWire:    -e PIPEWIRE_REMOTE=$XDG_RUNTIME_DIR/pipewire-0")
	}
	if wsl() {
		switch {
		case forwarded:
			env.Notices = append(env.Notices, "icebath runs in WSL, and it can reach a PulseAudio or PipeWire sound server.")
		case wslPowershell():
			env.Notices = append(env.Notices, "icebath runs in WSL without a PulseAudio bridge, so it plays speech through PowerShell. "+
				"Recording your voice still needs the bridge:\n  1. Set PULSE_SERVER=unix:/mnt/wslg/PulseServer\n"+
				"  2. Make ~/.asoundrc point ALSA at PulseAudio\n  3. Check it with arecord -d 3 test.wav && aplay test.wav")
		default:
			env.Warnings = append(env.Warnings, "icebath runs in WSL, where sound needs a sound server of Windows.\n  PulseAudio: export PULSE_SERVER=unix:/mnt/wslg/PulseServer\n"+
				"  PipeWire:   export PIPEWIRE_REMOTE=$XDG_RUNTIME_DIR/pipewire-0\n  Then check it with arecord -d 3 test.wav && aplay test.wav")
		}
	}
	probeLibraries(&env, forwarded, recorder, app)
	env.Available = len(env.Warnings) == 0
	return env
}

// Whether PortAudio loads
func audioLoads() bool {
	_, err := audio()
	return err == nil
}

// Why capture can't run, empty when PortAudio loads
func captureMissing() string {
	_, err := audio()
	switch {
	case err == nil:
		return ""
	case unloadable(err):
		return captureHint(err)
	case missingLibrary(err):
		return "PortAudio, the sound library icebath records with, is not installed"
	}
	return err.Error()
}

// Whether WSL plays speech through PowerShell and ffmpeg without a PulseAudio bridge
func wslPowershell() bool {
	if !wsl() {
		return false
	}
	_, ps := found("powershell.exe")
	_, ff := found("ffmpeg")
	return ps && ff
}

// Rate of the default microphone, half to even, else 16 kHz
func inputRate(pa portaudio.Audio) int {
	if info, err := pa.Device(nil, portaudio.Input); err == nil && info.DefaultSampleRate > 0 && !math.IsInf(info.DefaultSampleRate, 1) {
		return int(math.RoundToEven(info.DefaultSampleRate))
	}
	return sampleRate
}

// Whether this machine can talk to the agent: a way to record, a recognizer and an environment with audio
type Requirements struct {
	Available bool
	Audio     bool
	STT       bool
	// Why sound can't be recorded, empty when it can or nothing more is known
	NoAudio string
	Env     Environment
}

// Requirements of voice mode with the recognizer the server reports: whether speech is transcribed at all, and the recognizer by its label
func Check(enabled bool, label string) Requirements {
	stt := enabled && label != ""
	termuxed := termuxCapture()
	has := audioLoads() || termuxed
	env := Detect()
	why := ""
	if !has {
		why = captureMissing()
	}
	return Requirements{Available: has && stt && env.Available, Audio: has, STT: stt, NoAudio: why, Env: env}
}
