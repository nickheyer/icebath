package media

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pysounddevice"
	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
)

// Replaces a package variable for the test and restores it afterwards
func swapVoice[T any](t *testing.T, p *T, v T) {
	t.Helper()
	orig := *p
	*p = v
	t.Cleanup(func() { *p = orig })
}

type fakeStream struct {
	mu         sync.Mutex
	active     bool
	activeErr  error
	startErr   error
	starts     int
	stops      int
	stopErr    error
	closes     int
	aborts     int
	samplerate float64
	params     pysounddevice.StreamParams
	callback   pysounddevice.InputCallback
	reads      [][]int16
}

func (s *fakeStream) Active() (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active, s.activeErr
}
func (s *fakeStream) Stopped() (bool, error) { return !s.active, nil }
func (s *fakeStream) Closed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closes > 0
}
func (s *fakeStream) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.starts++
	if s.startErr != nil {
		return s.startErr
	}
	s.active = true
	return nil
}
func (s *fakeStream) Stop(bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stops++
	s.active = false
	return s.stopErr
}
func (s *fakeStream) Abort(bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.aborts++
	s.active = false
	return nil
}
func (s *fakeStream) Close(bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closes++
	s.active = false
	return nil
}
func (s *fakeStream) Samplerate() float64 { return s.samplerate }
func (s *fakeStream) Blocksize() int      { return s.params.Blocksize }
func (s *fakeStream) Channels() int       { return 1 }
func (s *fakeStream) Latency() float64    { return 0 }
func (s *fakeStream) Read(frames int) ([]int16, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.reads) == 0 {
		return make([]int16, frames), false, nil
	}
	block := s.reads[0]
	s.reads = s.reads[1:]
	return block, false, nil
}
func (s *fakeStream) ReadAvailable() (int, error) { return 0, nil }

type fakeSD struct {
	mu           sync.Mutex
	played       [][]int16
	rates        []float64
	blocksizes   []int
	stops        int
	devices      pysounddevice.DeviceList
	devicesErr   error
	inputDevice  pysounddevice.Device
	inputErr     error
	stream       *fakeStream
	inputs       []*fakeStream
	inputFactory func(p pysounddevice.StreamParams, cb pysounddevice.InputCallback) (pysounddevice.InputStream, error)
}

func (f *fakeSD) QueryDevices() (pysounddevice.DeviceList, error) { return f.devices, f.devicesErr }
func (f *fakeSD) QueryDevice(any, string) (pysounddevice.Device, error) {
	return f.inputDevice, f.inputErr
}
func (f *fakeSD) QueryHostAPI(int) (pysounddevice.HostAPI, error) {
	return pysounddevice.HostAPI{}, nil
}
func (f *fakeSD) Play(data []int16, samplerate float64, blocksize int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.played = append(f.played, append([]int16(nil), data...))
	f.rates = append(f.rates, samplerate)
	f.blocksizes = append(f.blocksizes, blocksize)
	return nil
}
func (f *fakeSD) Stop() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stops++
}
func (f *fakeSD) GetStream() (pysounddevice.Stream, error) { return f.stream, nil }
func (f *fakeSD) InputStream(p pysounddevice.StreamParams, cb pysounddevice.InputCallback) (pysounddevice.InputStream, error) {
	if f.inputFactory != nil {
		return f.inputFactory(p, cb)
	}
	s := &fakeStream{params: p, callback: cb, samplerate: p.Samplerate}
	f.mu.Lock()
	f.inputs = append(f.inputs, s)
	f.mu.Unlock()
	return s, nil
}
func (f *fakeSD) OutputStream(pysounddevice.StreamParams) (pysounddevice.OutputStream, error) {
	return nil, errors.New("output streams are not used by voice mode")
}

func (f *fakeSD) playCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.played)
}

// Installs a fake sounddevice module with one device and an idle playback stream
func withFakeSD(t *testing.T) *fakeSD {
	sd := &fakeSD{devices: pysounddevice.DeviceList{{Name: "dev"}}, stream: &fakeStream{},
		inputDevice: pysounddevice.Device{DefaultSamplerate: voiceSampleRate}}
	swapVoice(t, &importAudio, func() (pysounddevice.Module, error) { return sd, nil })
	swapVoice(t, &audioAvailable, func() (bool, error) { return true, nil })
	return sd
}

func withAudioImportFailure(t *testing.T, err error) {
	swapVoice(t, &importAudio, func() (pysounddevice.Module, error) { return nil, err })
}

// Clears forwarding, SSH and Termux variables and pins the host predicates to a plain Linux desktop
func cleanAudioEnv(t *testing.T) {
	for _, k := range []string{"SSH_CLIENT", "SSH_TTY", "SSH_CONNECTION", "PULSE_SERVER", "PIPEWIRE_REMOTE",
		"PULSE_RUNTIME_PATH", "TERMUX_VERSION", "PREFIX"} {
		t.Setenv(k, "")
	}
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	swapVoice(t, &voiceIsContainer, func() bool { return false })
	swapVoice(t, &voiceIsWSL, func() bool { return false })
	swapVoice(t, &voicePlatformSystem, func() string { return "Linux" })
	swapVoice(t, &voiceWhich, func(string) (string, bool) { return "", false })
}

func withVoiceHome(t *testing.T, configYAML string) {
	dir := t.TempDir()
	t.Setenv("ICEBATH_HOME", dir)
	if configYAML != "" {
		if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(configYAML), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func withVoiceTempDir(t *testing.T) string {
	dir := t.TempDir()
	swapVoice(t, &voiceTempDir, func() string { return dir })
	return dir
}

func writeSampleWAV(t *testing.T) string {
	path := filepath.Join(t.TempDir(), "test.wav")
	if err := os.WriteFile(path, wrapPCMAsWAV(make([]byte, 32000), 16000, 1, 2), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

type fakePlayer struct {
	mu         sync.Mutex
	rc         int
	running    bool
	terminated int
}

func (p *fakePlayer) Poll() (int, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.rc, !p.running
}
func (p *fakePlayer) Terminate() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.terminated++
	return nil
}
func (p *fakePlayer) Kill() error                     { return nil }
func (p *fakePlayer) Wait(time.Duration) (int, error) { return p.rc, nil }
func (p *fakePlayer) terminateCount() int             { p.mu.Lock(); defer p.mu.Unlock(); return p.terminated }
func newFakePlayer(rc int, running bool) *fakePlayer  { return &fakePlayer{rc: rc, running: running} }
func playerReturning(rc func([]string) int) func([]string, map[string]string) (playbackProcess, error) {
	return func(cmd []string, _ map[string]string) (playbackProcess, error) {
		return newFakePlayer(rc(cmd), false), nil
	}
}

func TestPulseSocketReachableStaleSocketFileNotReachable(t *testing.T) {
	t.Chdir(t.TempDir())
	runtimeDir := "pulse-stale"
	sockPath := filepath.Join(runtimeDir, "pulse", "native")
	if err := os.MkdirAll(filepath.Dir(sockPath), 0o755); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	l.(*net.UnixListener).SetUnlinkOnClose(false)
	l.Close()
	t.Setenv("PULSE_SERVER", "")
	t.Setenv("PULSE_RUNTIME_PATH", "")
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	if pulseSocketReachable() {
		t.Fatal("a stale socket file must not count as reachable")
	}
}

func TestPulseSocketReachableListeningSocketReachableViaXdgRuntime(t *testing.T) {
	t.Chdir(t.TempDir())
	runtimeDir := "pulse-live"
	sockPath := filepath.Join(runtimeDir, "pulse", "native")
	if err := os.MkdirAll(filepath.Dir(sockPath), 0o755); err != nil {
		t.Fatal(err)
	}
	server, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	t.Setenv("PULSE_SERVER", "")
	t.Setenv("PULSE_RUNTIME_PATH", "")
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	if !pulseSocketReachable() {
		t.Fatal("a listening socket must be reachable")
	}
}

func anyContains(items []string, sub string) bool {
	return slices.ContainsFunc(items, func(s string) bool { return strings.Contains(s, sub) })
}

func TestDetectAudioEnvironmentCleanEnvironmentIsAvailable(t *testing.T) {
	cleanAudioEnv(t)
	withFakeSD(t)
	result, err := DetectAudioEnvironment()
	if err != nil || !result.Available || len(result.Warnings) != 0 {
		t.Fatalf("%+v %v", result, err)
	}
}

func TestDetectAudioEnvironmentSshBlocksVoice(t *testing.T) {
	cleanAudioEnv(t)
	t.Setenv("SSH_CLIENT", "1.2.3.4 54321 22")
	withFakeSD(t)
	result, _ := DetectAudioEnvironment()
	if result.Available || !anyContains(result.Warnings, "SSH") {
		t.Fatalf("%+v", result)
	}
}

func TestDetectAudioEnvironmentSshWithPulseServerAllowsVoice(t *testing.T) {
	cleanAudioEnv(t)
	t.Setenv("SSH_CLIENT", "1.2.3.4 54321 22")
	t.Setenv("PULSE_SERVER", "unix:/run/user/1002/pulse/native")
	withFakeSD(t)
	result, _ := DetectAudioEnvironment()
	if !result.Available || len(result.Warnings) != 0 || !anyContains(result.Notices, "SSH") {
		t.Fatalf("%+v", result)
	}
}

func TestDetectAudioEnvironmentWslWithoutPulseBlocksVoice(t *testing.T) {
	cleanAudioEnv(t)
	swapVoice(t, &voiceIsWSL, func() bool { return true })
	withFakeSD(t)
	result, _ := DetectAudioEnvironment()
	if result.Available || !anyContains(result.Warnings, "WSL") || !anyContains(result.Warnings, "PulseAudio") {
		t.Fatalf("%+v", result)
	}
}

func TestDetectAudioEnvironmentDockerWithPipewireRemoteAndNoDevicesAllowsVoice(t *testing.T) {
	cleanAudioEnv(t)
	t.Setenv("PIPEWIRE_REMOTE", "/run/user/1000/pipewire-0")
	swapVoice(t, &voiceIsContainer, func() bool { return true })
	sd := withFakeSD(t)
	sd.devices = pysounddevice.DeviceList{}
	result, _ := DetectAudioEnvironment()
	lowered := make([]string, len(result.Notices))
	for i, n := range result.Notices {
		lowered[i] = strings.ToLower(n)
	}
	if !result.Available || len(result.Warnings) != 0 || !anyContains(lowered, "host audio forwarding") {
		t.Fatalf("%+v", result)
	}
}

func TestDetectAudioEnvironmentDockerWithoutAudioForwardingBlocksVoice(t *testing.T) {
	cleanAudioEnv(t)
	swapVoice(t, &voiceIsContainer, func() bool { return true })
	withFakeSD(t)
	result, _ := DetectAudioEnvironment()
	lowered := make([]string, len(result.Warnings))
	for i, w := range result.Warnings {
		lowered[i] = strings.ToLower(w)
	}
	if result.Available || !anyContains(lowered, "container") ||
		!(anyContains(result.Warnings, "PULSE_SERVER") || anyContains(result.Warnings, "PIPEWIRE_REMOTE")) {
		t.Fatalf("%+v", result)
	}
}

func TestNativeSttLabelSyncEveryNativeSttBuiltinHasVoiceModeLabel(t *testing.T) {
	var missing []string
	for name := range builtinSTTProviders {
		if _, ok := nativeSTTLabels[name]; !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		slices.Sort(missing)
		t.Fatalf("nativeSTTLabels is missing labels for native STT built-ins: %v", missing)
	}
}

func TestCheckVoiceRequirementsAllRequirementsMet(t *testing.T) {
	withVoiceHome(t, "")
	cleanAudioEnv(t)
	swapVoice(t, &audioAvailable, func() (bool, error) { return true, nil })
	swapVoice(t, &detectAudioEnv, func() (AudioEnvironment, error) {
		return AudioEnvironment{Available: true, Warnings: []string{}}, nil
	})
	swapVoice(t, &voiceGetSTTProvider, func(context.Context, map[string]any) (string, error) { return "openai", nil })
	result, err := CheckVoiceRequirements(context.Background())
	if err != nil || !result.Available || !result.AudioAvailable || !result.STTAvailable {
		t.Fatalf("%+v %v", result, err)
	}
}

type fakePluginSTT struct {
	TranscriptionProviderDefaults
	available bool
}

func (f *fakePluginSTT) Name() string      { return "my-plugin-stt" }
func (f *fakePluginSTT) IsAvailable() bool { return f.available }
func (f *fakePluginSTT) Transcribe(context.Context, string, TranscribeOptions) (any, error) {
	return nil, nil
}

func TestCheckVoiceRequirementsPluginSttProvider(t *testing.T) {
	withVoiceHome(t, "")
	cleanAudioEnv(t)
	swapVoice(t, &audioAvailable, func() (bool, error) { return true, nil })
	swapVoice(t, &detectAudioEnv, func() (AudioEnvironment, error) {
		return AudioEnvironment{Available: true, Warnings: []string{}}, nil
	})
	swapVoice(t, &loadSTTConfig, func(context.Context) map[string]any {
		return map[string]any{"enabled": true, "provider": "my-plugin-stt"}
	})
	plugin := &fakePluginSTT{available: true}
	plugin.TranscriptionProviderDefaults = NewTranscriptionProviderDefaults(plugin)
	swapVoice(t, &voiceGetSTTPlugin, func(_ context.Context, name, _ string) TranscriptionProvider {
		if name == "my-plugin-stt" {
			return plugin
		}
		return nil
	})
	swapVoice(t, &sttEnsurePluginsDiscovered, func(context.Context, bool) {})
	result, err := CheckVoiceRequirements(context.Background())
	if err != nil || !result.Available || !result.STTAvailable {
		t.Fatalf("%+v %v", result, err)
	}
}

func TestPlayAudioFilePlayWavViaSounddevice(t *testing.T) {
	cleanAudioEnv(t)
	sd := withFakeSD(t)
	if !PlayAudioFile(writeSampleWAV(t)) {
		t.Fatal("play_audio_file returned false")
	}
	if sd.playCount() != 1 || sd.stops != 1 {
		t.Fatalf("play %d stop %d", sd.playCount(), sd.stops)
	}
}

func TestMacOSAudioOutputPolicyPlayAudioFileSkipsSounddeviceOnMacos(t *testing.T) {
	cleanAudioEnv(t)
	swapVoice(t, &voicePlatformSystem, func() string { return "Darwin" })
	swapVoice(t, &importAudio, func() (pysounddevice.Module, error) {
		t.Error("sounddevice must not be imported for output on macOS")
		return nil, errors.New("forbidden")
	})
	swapVoice(t, &voiceWhich, func(cmd string) (string, bool) { return "/usr/bin/" + cmd, true })
	var cmds [][]string
	swapVoice(t, &startPlayerProcess, playerReturning(func(cmd []string) int { cmds = append(cmds, cmd); return 0 }))
	if !PlayAudioFile(writeSampleWAV(t)) {
		t.Fatal("play_audio_file returned false")
	}
	if len(cmds) == 0 || cmds[0][0] != "afplay" {
		t.Fatalf("players %v", cmds)
	}
}

func TestMacOSAudioOutputPolicyPlayBeepRoutesThroughAfplayOnMacos(t *testing.T) {
	withVoiceHome(t, "")
	cleanAudioEnv(t)
	swapVoice(t, &voicePlatformSystem, func() string { return "Darwin" })
	swapVoice(t, &importAudio, func() (pysounddevice.Module, error) {
		t.Error("sounddevice must not be imported for beeps on macOS")
		return nil, errors.New("forbidden")
	})
	type call struct{ n, rate int }
	var calls []call
	swapVoice(t, &playInt16ViaTempfile, func(audio []int16, rate int) { calls = append(calls, call{len(audio), rate}) })
	PlayBeep(context.Background(), 880, 0.12, 1)
	if len(calls) != 1 || calls[0].n <= 0 || calls[0].rate != voiceSampleRate {
		t.Fatalf("%+v", calls)
	}
}

func TestCleanupTempRecordingsOldFilesDeleted(t *testing.T) {
	dir := withVoiceTempDir(t)
	old := filepath.Join(dir, "recording_20240101_000000.wav")
	if err := os.WriteFile(old, make([]byte, 100), 0o600); err != nil {
		t.Fatal(err)
	}
	stamp := time.Now().Add(-7200 * time.Second)
	if err := os.Chtimes(old, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if deleted := CleanupTempRecordings(3600); deleted != 1 {
		t.Fatalf("deleted %d", deleted)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("old recording survived")
	}
}

func TestCleanupTempRecordingsRecentFilesPreserved(t *testing.T) {
	dir := withVoiceTempDir(t)
	recent := filepath.Join(dir, "recording_20260303_120000.wav")
	if err := os.WriteFile(recent, make([]byte, 100), 0o600); err != nil {
		t.Fatal(err)
	}
	if deleted := CleanupTempRecordings(3600); deleted != 0 {
		t.Fatalf("deleted %d", deleted)
	}
	if _, err := os.Stat(recent); err != nil {
		t.Fatal("recent recording removed")
	}
}

func TestPlayBeepBeepCallsSounddevicePlay(t *testing.T) {
	withVoiceHome(t, "")
	cleanAudioEnv(t)
	sd := withFakeSD(t)
	PlayBeep(context.Background(), 880, 0.1, 1)
	if sd.playCount() != 1 || sd.stops < 1 || len(sd.played[0]) == 0 {
		t.Fatalf("play %d stop %d", sd.playCount(), sd.stops)
	}
}

func TestPlaybackInterruptStopPlaybackTerminatesProcess(t *testing.T) {
	withFakeSD(t)
	proc := newFakePlayer(0, true)
	setActivePlayback(proc)
	StopPlayback()
	if proc.terminateCount() != 1 {
		t.Fatalf("terminate called %d times", proc.terminateCount())
	}
	playbackMu.Lock()
	defer playbackMu.Unlock()
	if activePlayback != nil {
		t.Fatal("active playback not cleared")
	}
}

func TestGetBeepVolumeConfigValueResolution(t *testing.T) {
	cases := []struct {
		config string
		want   float64
	}{
		{"voice: {}\n", 0.3},
		{"voice:\n  beep_volume: 0.0\n", 0.0},
		{"voice:\n  beep_volume: 1.5\n", 0.3},
		{"voice:\n  beep_volume: \"0.7\"\n", 0.7},
		{"voice:\n  beep_volume: true\n", 0.3},
	}
	for _, c := range cases {
		withVoiceHome(t, c.config)
		if got := getBeepVolume(context.Background()); got != c.want {
			t.Fatalf("%q: %v want %v", c.config, got, c.want)
		}
	}
}

func TestGetBeepVolumeLoadConfigExceptionFallsBack(t *testing.T) {
	withVoiceHome(t, "voice: [unclosed\n")
	if got := getBeepVolume(context.Background()); got != 0.3 {
		t.Fatalf("got %v", got)
	}
}

func TestDefaultInputSamplerateUsesDeviceDefaultRate(t *testing.T) {
	sd := &fakeSD{inputDevice: pysounddevice.Device{DefaultSamplerate: 44100.0}}
	if got := defaultInputSamplerate(sd); got != 44100 {
		t.Fatalf("got %d", got)
	}
}

func TestWSL2PowerShellFallbackPowershellPipelinePreservesRealExitStatus(t *testing.T) {
	cleanAudioEnv(t)
	swapVoice(t, &voiceIsWSL, func() bool { return true })
	withAudioImportFailure(t, &audioImportError{})
	swapVoice(t, &voiceWhich, func(x string) (string, bool) {
		switch {
		case x == "powershell.exe" || x == "ffmpeg" || x == "ffplay" || x == "sh":
			return "/bin/" + x, true
		case strings.HasPrefix(x, "/"):
			return x, true
		}
		return "", false
	})
	responses := [][]byte{[]byte("C:/Temp\r\n"), []byte("/mnt/c/Temp\n"), []byte("C:/Temp/hermes.wav\n")}
	swapVoice(t, &voiceCheckOutput, func([]string, float64) ([]byte, error) {
		out := responses[0]
		responses = responses[1:]
		return out, nil
	})
	var cmds [][]string
	swapVoice(t, &startPlayerProcess, playerReturning(func(cmd []string) int {
		cmds = append(cmds, cmd)
		if cmd[0] == "/bin/sh" || cmd[0] == "sh" {
			return 1
		}
		return 0
	}))
	if !PlayAudioFile(writeSampleWAV(t)) {
		t.Fatal("must fall through to ffplay and succeed")
	}
	if len(cmds) != 2 || (cmds[0][0] != "/bin/sh" && cmds[0][0] != "sh") || cmds[1][0] != "ffplay" {
		t.Fatalf("players %v", cmds)
	}
	if script := cmds[0][2]; !strings.Contains(script, "rc=$?") || !strings.Contains(script, "exit $rc") {
		t.Fatalf("shell pipeline must preserve the real exit status past cleanup: %s", script)
	}
}

func TestWSL2PowerShellFallbackWsl2UniqueTempFilename(t *testing.T) {
	cleanAudioEnv(t)
	swapVoice(t, &voiceIsWSL, func() bool { return true })
	withFakeSD(t)
	swapVoice(t, &voiceWhich, func(x string) (string, bool) {
		if x == "powershell.exe" || x == "ffmpeg" || x == "ffplay" {
			return "/bin/" + x, true
		}
		return "", false
	})
	var filenames []string
	swapVoice(t, &voiceCheckOutput, func(cmd []string, _ float64) ([]byte, error) {
		joined := strings.Join(cmd, " ")
		switch {
		case strings.Contains(joined, "TEMP"):
			return []byte("C:\\Temp\r\n"), nil
		case strings.Contains(joined, "wslpath") && slices.Contains(cmd, "-u"):
			return []byte("/mnt/c/Temp\n"), nil
		case strings.Contains(joined, "wslpath") && slices.Contains(cmd, "-w"):
			parts := strings.Split(cmd[len(cmd)-1], "/")
			filenames = append(filenames, parts[len(parts)-1])
			return []byte("C:\\Temp\\" + parts[len(parts)-1] + "\n"), nil
		}
		return nil, nil
	})
	swapVoice(t, &startPlayerProcess, playerReturning(func([]string) int { return 0 }))
	sample := writeSampleWAV(t)
	swapVoice(t, &soundDeviceOutputAllowed, func() bool { return false })
	PlayAudioFile(sample)
	PlayAudioFile(sample)
	if len(filenames) != 2 || filenames[0] == filenames[1] {
		t.Fatalf("concurrent TTS calls must use unique temp WAV filenames: %v", filenames)
	}
}

func TestWSL2PowerShellFallbackNonWslSkipsPowershellFallback(t *testing.T) {
	cleanAudioEnv(t)
	withAudioImportFailure(t, &audioImportError{})
	swapVoice(t, &voiceWhich, func(x string) (string, bool) {
		if x == "ffplay" || x == "aplay" {
			return "/bin/" + x, true
		}
		return "", false
	})
	var cmds [][]string
	swapVoice(t, &startPlayerProcess, playerReturning(func(cmd []string) int { cmds = append(cmds, cmd); return 0 }))
	PlayAudioFile(writeSampleWAV(t))
	if len(cmds) == 0 {
		t.Fatal("no players were tried")
	}
	for _, cmd := range cmds {
		if cmd[0] == "sh" && strings.Contains(strings.Join(cmd, " "), "powershell") {
			t.Fatalf("PowerShell player must not appear on non-WSL Linux: %v", cmd)
		}
	}
}

func TestWSLAudioEnvironmentGateWslNoPulseButPowershellAvailableNotHardBlocked(t *testing.T) {
	cleanAudioEnv(t)
	withFakeSD(t)
	swapVoice(t, &voiceIsWSL, func() bool { return true })
	swapVoice(t, &voiceWhich, func(x string) (string, bool) { return "/bin/" + x, x == "powershell.exe" || x == "ffmpeg" })
	result, _ := DetectAudioEnvironment()
	if !result.Available {
		t.Fatalf("PowerShell TTS fallback must keep voice mode enabled: %v", result.Warnings)
	}
	if !anyContains(result.Notices, "PowerShell") && !anyContains(result.Notices, "Media.SoundPlayer") {
		t.Fatalf("notices %v", result.Notices)
	}
	if !slices.ContainsFunc(result.Notices, func(n string) bool {
		return strings.Contains(strings.ToLower(n), "recording") || strings.Contains(n, "PulseAudio")
	}) {
		t.Fatalf("notices %v", result.Notices)
	}
}

func TestWSLAudioEnvironmentGateWslNoPulseNoPowershellStillBlocked(t *testing.T) {
	cleanAudioEnv(t)
	withFakeSD(t)
	swapVoice(t, &voiceIsWSL, func() bool { return true })
	if result, _ := DetectAudioEnvironment(); result.Available {
		t.Fatalf("%+v", result)
	}
}

func TestWSLAudioEnvironmentGateWslWithPulseServerUnaffected(t *testing.T) {
	cleanAudioEnv(t)
	t.Setenv("PULSE_SERVER", "unix:/mnt/wslg/PulseServer")
	withFakeSD(t)
	swapVoice(t, &voiceIsWSL, func() bool { return true })
	if result, _ := DetectAudioEnvironment(); !result.Available {
		t.Fatalf("%+v", result)
	}
}

func TestPlayAudioFileScrubbedEnv(t *testing.T) {
	cleanAudioEnv(t)
	audio := filepath.Join(t.TempDir(), "t.mp3")
	if err := os.WriteFile(audio, []byte("ID3fake"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TELEGRAM_BOT_TOKEN", "secret-token")
	t.Setenv("OPENAI_API_KEY", "sk-test")
	swapVoice(t, &voiceWhich, func(string) (string, bool) { return "/usr/bin/ffplay", true })
	var captured map[string]string
	swapVoice(t, &startPlayerProcess, func(_ []string, env map[string]string) (playbackProcess, error) {
		captured = env
		return newFakePlayer(0, false), nil
	})
	if !PlayAudioFile(audio) {
		t.Fatal("play_audio_file returned false")
	}
	if captured == nil {
		t.Fatal("player started without an explicit environment")
	}
	if _, ok := captured["TELEGRAM_BOT_TOKEN"]; ok {
		t.Fatal("TELEGRAM_BOT_TOKEN leaked into the player environment")
	}
	if _, ok := captured["OPENAI_API_KEY"]; ok {
		t.Fatal("OPENAI_API_KEY leaked into the player environment")
	}
}

func wslPipewireBase(t *testing.T) {
	cleanAudioEnv(t)
	sd := withFakeSD(t)
	sd.devices = pysounddevice.DeviceList{{Name: "dev"}}
	swapVoice(t, &voiceIsWSL, func() bool { return true })
}

func TestWslWithPipewireRemoteAllowsVoice(t *testing.T) {
	wslPipewireBase(t)
	t.Setenv("PIPEWIRE_REMOTE", "/run/user/1000/pipewire-0")
	if result, _ := DetectAudioEnvironment(); !result.Available {
		t.Fatal(result.Warnings)
	}
}

func TestWslWithPulseServerStillAllowsVoice(t *testing.T) {
	wslPipewireBase(t)
	t.Setenv("PULSE_SERVER", "unix:/mnt/wslg/PulseServer")
	if result, _ := DetectAudioEnvironment(); !result.Available {
		t.Fatal(result.Warnings)
	}
}

func TestWslWithoutForwardingStillBlocks(t *testing.T) {
	wslPipewireBase(t)
	result, _ := DetectAudioEnvironment()
	if result.Available || !anyContains(result.Warnings, "WSL") {
		t.Fatalf("%+v", result)
	}
}

type probeAction struct {
	result *pysubprocess.Completed
	err    error
}

// Dispatches the Termux package probes by command name, failing on any other command
func withTermuxProbes(t *testing.T, behaviors map[string]probeAction) {
	swapVoice(t, &runQuiet, func(cmd []string, _ float64, _ bool) (*pysubprocess.Completed, error) {
		action, ok := behaviors[cmd[0]]
		if !ok {
			t.Errorf("unexpected probe command %v", cmd)
			return nil, errors.New("unexpected probe")
		}
		return action.result, action.err
	})
}

func completed(rc int, stdout, stderr string) probeAction {
	return probeAction{result: &pysubprocess.Completed{Returncode: rc, Stdout: []byte(stdout), Stderr: []byte(stderr)}}
}

func forceTermux(t *testing.T) {
	swapVoice(t, &isTermuxEnvironment, func() bool { return true })
	swapVoice(t, &voiceWhich, func(string) (string, bool) { return "", false })
}

func TestTermuxApiAppInstalledProbeLadderReturnsFalseOutsideTermux(t *testing.T) {
	swapVoice(t, &isTermuxEnvironment, func() bool { return false })
	if termuxAPIAppInstalled() {
		t.Fatal("must be false outside Termux")
	}
}

func TestTermuxApiAppInstalledProbeLadderPmConfirmsPackageReturnsTrue(t *testing.T) {
	forceTermux(t)
	withTermuxProbes(t, map[string]probeAction{"pm": completed(0, "package:com.termux.api\n", "")})
	if !termuxAPIAppInstalled() {
		t.Fatal("pm listing must confirm the app")
	}
}

func TestTermuxApiAppInstalledProbeLadderPmTimeoutThenCmdConfirms(t *testing.T) {
	forceTermux(t)
	withTermuxProbes(t, map[string]probeAction{
		"pm":  {err: &pysubprocess.TimeoutExpired{Cmd: []string{"pm"}, Timeout: 5}},
		"cmd": completed(0, "package:com.termux.api\n", ""),
	})
	if !termuxAPIAppInstalled() {
		t.Fatal("cmd package listing must confirm the app")
	}
}

func TestTermuxApiAppInstalledProbeLadderPmNonzeroExitThenCmdConfirms(t *testing.T) {
	forceTermux(t)
	withTermuxProbes(t, map[string]probeAction{
		"pm":  completed(1, "", "permission denied"),
		"cmd": completed(0, "package:com.termux.api\n", ""),
	})
	if !termuxAPIAppInstalled() {
		t.Fatal("cmd package listing must confirm the app")
	}
}

func termuxRecorderOnPath(t *testing.T) {
	swapVoice(t, &voiceWhich, func(name string) (string, bool) {
		if name == "termux-microphone-record" {
			return "/data/data/com.termux/files/usr/bin/termux-microphone-record", true
		}
		return "", false
	})
}

func TestTermuxApiAppInstalledProbeLadderBothProbesInconclusiveButBinaryPresentReturnsTrue(t *testing.T) {
	forceTermux(t)
	withTermuxProbes(t, map[string]probeAction{
		"pm":  {err: &os.PathError{Op: "exec", Path: "pm", Err: os.ErrNotExist}},
		"cmd": {err: &os.PathError{Op: "exec", Path: "cmd", Err: os.ErrNotExist}},
	})
	termuxRecorderOnPath(t)
	if !termuxAPIAppInstalled() {
		t.Fatal("the recorder binary must be trusted when both probes are inconclusive")
	}
}

func TestTermuxApiAppInstalledProbeLadderMatchIsCaseInsensitive(t *testing.T) {
	forceTermux(t)
	withTermuxProbes(t, map[string]probeAction{"pm": completed(0, "Package:com.termux.api\n", "")})
	if !termuxAPIAppInstalled() {
		t.Fatal("the package match must ignore case")
	}
}

func termuxDetectBase(t *testing.T) {
	cleanAudioEnv(t)
	t.Setenv("TERMUX_VERSION", "0.118.3")
	t.Setenv("PREFIX", "/data/data/com.termux/files/usr")
	withAudioImportFailure(t, &audioImportError{msg: "no audio libs"})
	termuxRecorderOnPath(t)
}

func TestDetectAudioEnvironmentTermuxFallbackInconclusiveProbesWithBinaryDoesNotEmitAppWarning(t *testing.T) {
	termuxDetectBase(t)
	withTermuxProbes(t, map[string]probeAction{
		"pm":  {err: &os.PathError{Op: "exec", Path: "pm", Err: os.ErrNotExist}},
		"cmd": {err: &os.PathError{Op: "exec", Path: "cmd", Err: os.ErrNotExist}},
	})
	result, err := DetectAudioEnvironment()
	if err != nil || !result.Available {
		t.Fatalf("voice mode should be available when the binary is on PATH: %+v %v", result, err)
	}
	if anyContains(result.Warnings, "Termux:API Android app is not installed") {
		t.Fatalf("warnings %v", result.Warnings)
	}
}

func TestDetectAudioEnvironmentTermuxFallbackCleanProbesNoMatchStillBlocks(t *testing.T) {
	termuxDetectBase(t)
	withTermuxProbes(t, map[string]probeAction{"pm": completed(0, "", ""), "cmd": completed(0, "", "")})
	result, _ := DetectAudioEnvironment()
	if result.Available || !anyContains(result.Warnings, "Termux:API Android app is not installed") {
		t.Fatalf("%+v", result)
	}
}
