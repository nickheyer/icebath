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
	speechv1 "github.com/nickheyer/icebath/pkg/gen/icebath/speech/v1"
	"github.com/nickheyer/icebath/pkg/strs"
)

// Capture format every recording is made in
const (
	sampleRate = 16000
	channels   = 1
	sampleBits = 2
	// Level under which a block of audio counts as silence
	silenceRMS = 200
)

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

// What keeps this computer from recording, and what to do about it
type Problem struct {
	// A clause in a person's words, such as icebath runs over SSH and reaches no sound server
	Cause string
	// What to do, a sentence; empty when nothing a person does is known
	Fix string
	// The longer how-to, lines of commands to run, empty for none
	Detail string
}

// Why this build can't load PortAudio, the Termux:API route on Termux
func captureProblem(err error) Problem {
	if termux() {
		return Problem{Cause: "this build of icebath can't load PortAudio on Termux", Fix: "Record through Termux:API: run pkg install termux-api, then install " +
			"the Termux:API app for Android."}
	}
	return Problem{Cause: "this build of icebath can't load PortAudio, the sound library it records with", Fix: "Use a build of icebath made for this " +
		"computer. The error was: " + strings.TrimSuffix(err.Error(), ".") + "."}
}

// PortAudio missing from this machine, with the command that installs it here
func portaudioProblem() Problem {
	fix := "Install it with sudo apt-get install libportaudio2."
	switch {
	case termux():
		fix = "Install it with pkg install portaudio."
	case runtime.GOOS == "darwin":
		fix = "Install it with brew install portaudio."
	case runtime.GOOS == "windows":
		fix = "Put portaudio.dll in a folder on PATH, or libportaudio64bit.dll next to icebath.exe."
	}
	return Problem{Cause: "PortAudio, the sound library icebath records with, is not installed", Fix: fix}
}

// The problem as one message: what is wrong, then what to do
func (p Problem) Sentence() string {
	return strings.TrimSpace(strs.Capital(p.Cause) + ". " + p.Fix)
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
	// What keeps it from recording, the first the one to fix first
	Problems []Problem
	Notices  []string
}

// Problem of the Termux:API app missing where the recorder of Termux is
var termuxAppProblem = Problem{Cause: "the Termux:API app for Android, which records here, is not installed", Fix: "Install or update the Termux:API app."}

// Audio library verdict, Termux capture or a forwarded sound server softening a failure into a notice
func probeLibraries(env *Environment, forwarded bool, recorder string, app bool) {
	capture := recorder != "" && app
	outcome := func(termuxNotice string, problem Problem, forwardedNotice string, importFailed bool) {
		switch {
		case forwardedNotice != "" && forwarded:
			env.Notices = append(env.Notices, forwardedNotice)
		case capture:
			env.Notices = append(env.Notices, termuxNotice)
		case importFailed && recorder != "" && !app:
			env.Problems = append(env.Problems, termuxAppProblem)
		default:
			env.Problems = append(env.Problems, problem)
		}
	}
	pa, err := audio()
	if err != nil {
		switch {
		case unloadable(err):
			outcome(termuxRecords, captureProblem(err), "", true)
		case missingLibrary(err):
			outcome(termuxRecords, portaudioProblem(), "", true)
		default:
			outcome("PortAudio failed to start, but recording works through Termux:API.", Problem{Cause: "the sound system did not start",
				Fix: "The error was: " + strings.TrimSuffix(err.Error(), ".") + "."}, "", true)
		}
		return
	}
	devices, err := pa.Devices()
	if err != nil {
		outcome("PortAudio could not list the sound devices, but recording works through Termux:API.",
			Problem{Cause: "the sound system can't list the sound devices", Fix: "The error was: " + strings.TrimSuffix(err.Error(), ".") + "."},
			"PortAudio could not list the sound devices, but sound comes from the host, so voice input works.", false)
		return
	}
	if len(devices) == 0 {
		outcome("PortAudio found no sound devices, but recording works through Termux:API.",
			Problem{Cause: "no microphone or speaker was found", Fix: "Connect a microphone."},
			"PortAudio found no sound devices, but sound comes from the host, so voice input works.", false)
	}
}

// Whether this machine can record and play audio, with what blocks it and what only informs
func Detect() Environment {
	env := Environment{}
	recorder, app := termuxMic(), termuxApp()
	forwarded := os.Getenv("PULSE_SERVER") != "" || os.Getenv("PIPEWIRE_REMOTE") != "" || pulseReachable()
	report := func(notice string, problem Problem) {
		if forwarded {
			env.Notices = append(env.Notices, notice)
		} else {
			env.Problems = append(env.Problems, problem)
		}
	}
	if os.Getenv("SSH_CLIENT") != "" || os.Getenv("SSH_TTY") != "" || os.Getenv("SSH_CONNECTION") != "" {
		report("icebath runs over SSH, and it can reach a PulseAudio or PipeWire sound server.",
			Problem{Cause: "icebath runs over SSH and reaches no sound server", Fix: "Point PULSE_SERVER at the sound server of this computer.",
				Detail: "export XDG_RUNTIME_DIR=/run/user/$(id -u)\nexport PULSE_SERVER=unix:$XDG_RUNTIME_DIR/pulse/native"})
	}
	if terminal.InContainer() {
		report("icebath runs in a container, such as Docker, Podman or LXC, and sound comes from the host.",
			Problem{Cause: "icebath runs in a container with no sound devices", Fix: "Pass the sound server of the host into the container.",
				Detail: "PulseAudio: -v $XDG_RUNTIME_DIR/pulse/native:$XDG_RUNTIME_DIR/pulse/native -e PULSE_SERVER=unix:$XDG_RUNTIME_DIR/pulse/native\n" +
					"PipeWire: -e PIPEWIRE_REMOTE=$XDG_RUNTIME_DIR/pipewire-0"})
	}
	if wsl() {
		switch {
		case forwarded:
			env.Notices = append(env.Notices, "icebath runs in WSL, and it can reach a PulseAudio or PipeWire sound server.")
		case wslPowershell():
			env.Notices = append(env.Notices, "icebath runs in WSL without a PulseAudio bridge, so it plays speech through PowerShell.")
			env.Problems = append(env.Problems, Problem{Cause: "icebath runs in WSL without a PulseAudio bridge to record through",
				Fix: "Set PULSE_SERVER=unix:/mnt/wslg/PulseServer.", Detail: "Make ~/.asoundrc point ALSA at PulseAudio\n" +
					"Check it with arecord -d 3 test.wav && aplay test.wav"})
		default:
			env.Problems = append(env.Problems, Problem{Cause: "icebath runs in WSL and reaches no sound server of Windows",
				Fix: "Set PULSE_SERVER=unix:/mnt/wslg/PulseServer.", Detail: "PipeWire: export PIPEWIRE_REMOTE=$XDG_RUNTIME_DIR/pipewire-0\n" +
					"Check it with arecord -d 3 test.wav && aplay test.wav"})
		}
	}
	probeLibraries(&env, forwarded, recorder, app)
	env.Available = len(env.Problems) == 0
	return env
}

// Whether PortAudio loads
func audioLoads() bool {
	_, err := audio()
	return err == nil
}

// Why capture can't run, nil when PortAudio loads
func captureMissing() *Problem {
	_, err := audio()
	var p Problem
	switch {
	case err == nil:
		return nil
	case unloadable(err):
		p = captureProblem(err)
	case missingLibrary(err):
		p = portaudioProblem()
	default:
		p = Problem{Cause: "the sound system did not start", Fix: "The error was: " + strings.TrimSuffix(err.Error(), ".") + "."}
	}
	return &p
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

// The one answer to whether voice can hear you here, and when it can't, what keeps it: this computer recording first, then what the
// server says keeps your speech from being turned into text
type Hearing struct {
	// What keeps this computer from recording, nil while nothing does
	Recording *Problem
	// What keeps speech from being turned into text, nil while nothing does
	Recognition *speechv1.Hindrance
	// What is worth knowing of how this computer records
	Notices []string
}

// Whether voice can hear you
func (h Hearing) Ready() bool {
	return h.Recording == nil && h.Recognition == nil
}

// The one thing to do as the second line of the page of Voice says it, naming the row of that page that does it; empty while voice can hear
// you
func (h Hearing) Line() string {
	switch {
	case h.Recording != nil:
		return strings.TrimSpace("Voice can't hear you yet: " + h.Recording.Cause + ". " + h.Recording.Fix)
	case h.Recognition != nil:
		return strings.TrimSpace("Voice can't hear you yet: " + h.Recognition.GetCause() + ". " + Fix(h.Recognition, "Speech to text"))
	}
	return ""
}

// What keeps replies from being read aloud and the one thing to do, as the page of Voice says it, from what the server says of its voice;
// empty while replies can be read aloud. Whether voice can hear you has no part in it
func SpeakingLine(reply *speechv1.VoiceReply) string {
	h := reply.GetSpeaking().GetHindrance()
	if h == nil {
		return ""
	}
	return strings.TrimSpace("Replies can't be read aloud yet: " + h.GetCause() + ". " + Fix(h, "Text to speech"))
}

// What fixes a hindrance, said as the row of the page that does it: the page of the service, the row of the part of Voice it is about
func Fix(h *speechv1.Hindrance, row string) string {
	switch h.GetLack() {
	case speechv1.Lack_LACK_INSTALL:
		return row + " installs it."
	case speechv1.Lack_LACK_KEY:
		return row + " takes its API key."
	case speechv1.Lack_LACK_SIGN_IN:
		return row + " signs you in."
	case speechv1.Lack_LACK_PROGRAM:
		return "Install it with " + h.GetInstall() + "."
	case speechv1.Lack_LACK_BUILD:
		return row + " picks another service."
	case speechv1.Lack_LACK_SETUP:
		return row + " sets it up."
	case speechv1.Lack_LACK_SWITCH:
		return row + " turns it on."
	case speechv1.Lack_LACK_SERVICE:
		return row + " sets one up."
	}
	return ""
}

// Whether voice can hear you here with the recognition the server reports and the record key as the app shows it, empty while it has none
func Hears(reply *speechv1.VoiceReply, key string) Hearing {
	h := Hearing{Recognition: reply.GetRecognition().GetHindrance()}
	env := Detect()
	h.Notices = env.Notices
	switch {
	case len(env.Problems) > 0:
		h.Recording = &env.Problems[0]
	case !audioLoads() && !termuxCapture():
		h.Recording = captureMissing()
	case key == "":
		h.Recording = &Problem{Cause: "the record key has no key", Fix: "Record key gives it one."}
	}
	return h
}

// An input device of this machine, and what wake_word.input_device holds to name it
type Microphone struct {
	Name, Host string
	// The device the system records with by default
	Default bool
	// Its name, with its sound system where another device has the same name
	Value string
}

// Microphones of this machine, in the order the sound system lists them
func Microphones() ([]Microphone, error) {
	pa, err := audio()
	if err != nil {
		if p := captureMissing(); p != nil {
			return nil, errors.New(p.Sentence())
		}
		return nil, err
	}
	devices, err := pa.Devices()
	if err != nil {
		return nil, err
	}
	fallback := -1
	if d, err := pa.Device(nil, portaudio.Input); err == nil {
		fallback = d.Index
	}
	names := map[string]int{}
	var out []Microphone
	for _, d := range devices {
		if d.MaxInputChannels < 1 {
			continue
		}
		host := ""
		if api, err := pa.HostAPI(d.HostAPI); err == nil {
			host = api.Name
		}
		names[d.Name]++
		out = append(out, Microphone{Name: d.Name, Host: host, Default: d.Index == fallback, Value: d.Name})
	}
	for i, m := range out {
		if names[m.Name] > 1 && m.Host != "" {
			out[i].Value = m.Name + ", " + m.Host
		}
	}
	return out, nil
}
