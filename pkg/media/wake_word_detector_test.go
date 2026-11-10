package media

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pysounddevice"
)

// Detector engine stand in firing on demand and counting resets
type fakeWakeEngine struct {
	wakeEngineBase
	fire    bool
	closed  atomic.Bool
	resets  atomic.Int32
	process func(frame []int16) (bool, error)
}

func newFakeWakeEngine(fire bool) *fakeWakeEngine {
	return &fakeWakeEngine{wakeEngineBase: wakeEngineBase{frameLength: 4}, fire: fire}
}

func (e *fakeWakeEngine) Process(frame []int16) (bool, error) {
	if e.process != nil {
		return e.process(frame)
	}
	return e.fire, nil
}

func (e *fakeWakeEngine) Reset() error { e.resets.Add(1); return nil }
func (e *fakeWakeEngine) Close()       { e.closed.Store(true) }

// Input stream stand in, available below zero meaning a stream without read_available
type fakeWakeStream struct {
	mu        sync.Mutex
	closed    bool
	available int
	read      func(n int) ([]int16, error)
	readCalls int
	calls     []string
	onAbort   func()
}

func newFakeWakeStream(read func(n int) ([]int16, error)) *fakeWakeStream {
	return &fakeWakeStream{available: -1, read: read}
}

func (s *fakeWakeStream) record(call string) {
	s.mu.Lock()
	s.calls = append(s.calls, call)
	s.mu.Unlock()
}

func (s *fakeWakeStream) snapshot() (closed bool, readCalls int, calls []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed, s.readCalls, append([]string(nil), s.calls...)
}

func (s *fakeWakeStream) Active() (bool, error)  { return true, nil }
func (s *fakeWakeStream) Stopped() (bool, error) { return false, nil }
func (s *fakeWakeStream) Closed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}
func (s *fakeWakeStream) Start() error        { return nil }
func (s *fakeWakeStream) Stop(bool) error     { s.record("stop"); return nil }
func (s *fakeWakeStream) Samplerate() float64 { return WakeSampleRate }
func (s *fakeWakeStream) Blocksize() int      { return 0 }
func (s *fakeWakeStream) Channels() int       { return 1 }
func (s *fakeWakeStream) Latency() float64    { return 0 }
func (s *fakeWakeStream) Abort(bool) error {
	s.record("abort")
	if s.onAbort != nil {
		s.onAbort()
	}
	return nil
}
func (s *fakeWakeStream) Close(bool) error {
	s.mu.Lock()
	s.calls = append(s.calls, "close")
	s.closed = true
	s.mu.Unlock()
	return nil
}
func (s *fakeWakeStream) ReadAvailable() (int, error) {
	if s.available < 0 {
		return 1 << 30, nil
	}
	return s.available, nil
}
func (s *fakeWakeStream) Read(n int) ([]int16, bool, error) {
	s.mu.Lock()
	s.readCalls++
	s.mu.Unlock()
	data, err := s.read(n)
	return data, false, err
}

// sounddevice stand in, a nil query function behaving like the attribute missing from a SimpleNamespace
type fakeWakeSD struct {
	inputStream  func(p pysounddevice.StreamParams) (pysounddevice.InputStream, error)
	queryDevice  func(device any, kind string) (pysounddevice.Device, error)
	queryHostAPI func(index int) (pysounddevice.HostAPI, error)
}

var errNoQueryDevices = errors.New("'types.SimpleNamespace' object has no attribute 'query_devices'")

func (f *fakeWakeSD) QueryDevices() (pysounddevice.DeviceList, error) { return nil, errNoQueryDevices }
func (f *fakeWakeSD) QueryDevice(device any, kind string) (pysounddevice.Device, error) {
	if f.queryDevice == nil {
		return pysounddevice.Device{}, errNoQueryDevices
	}
	return f.queryDevice(device, kind)
}
func (f *fakeWakeSD) QueryHostAPI(index int) (pysounddevice.HostAPI, error) {
	if f.queryHostAPI == nil {
		return pysounddevice.HostAPI{}, errors.New("'types.SimpleNamespace' object has no attribute 'query_hostapis'")
	}
	return f.queryHostAPI(index)
}
func (f *fakeWakeSD) Play([]int16, float64, int) error         { return errors.New("no play") }
func (f *fakeWakeSD) Stop()                                    {}
func (f *fakeWakeSD) GetStream() (pysounddevice.Stream, error) { return nil, errors.New("no stream") }
func (f *fakeWakeSD) OutputStream(pysounddevice.StreamParams) (pysounddevice.OutputStream, error) {
	return nil, errors.New("no output")
}
func (f *fakeWakeSD) InputStream(p pysounddevice.StreamParams, _ pysounddevice.InputCallback) (pysounddevice.InputStream, error) {
	return f.inputStream(p)
}

func useWakeAudio(t *testing.T, sd pysounddevice.Module) {
	t.Helper()
	swap(t, &importAudio, func() (pysounddevice.Module, error) { return sd, nil })
}

// Frames of one value after a short sleep, the Python fake streams
func steadyRead(value int16, pause time.Duration) func(n int) ([]int16, error) {
	return func(n int) ([]int16, error) {
		time.Sleep(pause)
		out := make([]int16, n)
		for i := range out {
			out[i] = value
		}
		return out, nil
	}
}

// Streams handed out by a fake sounddevice, in creation order
type wakeStreamLog struct {
	mu      sync.Mutex
	streams []*fakeWakeStream
	params  []pysounddevice.StreamParams
}

func (l *wakeStreamLog) add(p pysounddevice.StreamParams, s *fakeWakeStream) pysounddevice.InputStream {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.streams = append(l.streams, s)
	l.params = append(l.params, p)
	return s
}

func (l *wakeStreamLog) first() *fakeWakeStream {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.streams) == 0 {
		return nil
	}
	return l.streams[0]
}

func waitFor(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for !cond() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	return cond()
}

func lockIn(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "wake.lock")
	swap(t, &wakeLockPath, func() string { return path })
	return path
}

func buildWith(t *testing.T, engine WakeEngine) {
	t.Helper()
	swap(t, &wakeBuildEngine, func(context.Context, map[string]any) (WakeEngine, error) { return engine, nil })
}

func noWake() error { return nil }

func TestDetectorOpensConfiguredInputDeviceAndReportsBackend(t *testing.T) {
	var mu sync.Mutex
	var reads []int
	var processed [][]int16
	log := &wakeStreamLog{}
	useWakeAudio(t, &fakeWakeSD{
		inputStream: func(p pysounddevice.StreamParams) (pysounddevice.InputStream, error) {
			loud := steadyRead(500, 5*time.Millisecond)
			return log.add(p, newFakeWakeStream(func(n int) ([]int16, error) {
				mu.Lock()
				reads = append(reads, n)
				mu.Unlock()
				return loud(n)
			})), nil
		},
		queryDevice: func(any, string) (pysounddevice.Device, error) {
			return pysounddevice.Device{Name: "Microphone Array", HostAPI: 2, MaxInputChannels: 2, DefaultSamplerate: 48000.0}, nil
		},
		queryHostAPI: func(int) (pysounddevice.HostAPI, error) { return pysounddevice.HostAPI{Name: "Windows WASAPI"}, nil },
	})
	engine := newFakeWakeEngine(false)
	engine.process = func(frame []int16) (bool, error) {
		mu.Lock()
		processed = append(processed, append([]int16(nil), frame...))
		mu.Unlock()
		return false, nil
	}
	det := NewWakeWordDetector(engine, noWake, WakeDetectorOptions{InputDevice: "Microphone Array"})
	if err := det.Start(); err != nil {
		t.Fatal(err)
	}
	defer det.Stop()
	p := log.params[0]
	if p.Device != "Microphone Array" || p.Samplerate != 48000 || p.Blocksize != 12 {
		t.Fatalf("opened %+v", p)
	}
	waitFor(2*time.Second, func() bool { mu.Lock(); defer mu.Unlock(); return len(processed) > 0 })
	mu.Lock()
	defer mu.Unlock()
	if reads[0] != 12 || len(processed[0]) != 4 || !slices.Equal(processed[0], []int16{500, 500, 500, 500}) {
		t.Fatalf("reads %v processed %v", reads, processed[0])
	}
	got := det.InputDeviceDetails()
	want := WakeInputDeviceDetails{Selector: "Microphone Array", Name: ptr("Microphone Array"), HostAPIIndex: ptr(2),
		HostAPI: ptr("Windows WASAPI"), MaxInputChannels: ptr(2), DefaultSamplerate: ptr(48000.0)}
	if fmt.Sprint(detailsMap(got)) != fmt.Sprint(detailsMap(want)) {
		t.Fatalf("details %v, want %v", detailsMap(got), detailsMap(want))
	}
}

// The details as the dict Python compares
func detailsMap(d WakeInputDeviceDetails) map[string]any {
	out := map[string]any{"selector": d.Selector}
	if d.Error != nil {
		out["error"] = *d.Error
	}
	if d.Name != nil {
		out["name"] = *d.Name
	}
	if d.MaxInputChannels != nil {
		out["max_input_channels"] = *d.MaxInputChannels
	}
	if d.DefaultSamplerate != nil {
		out["default_samplerate"] = *d.DefaultSamplerate
	}
	if d.HostAPIIndex != nil {
		out["hostapi_index"] = *d.HostAPIIndex
	}
	if d.HostAPI != nil {
		out["hostapi"] = *d.HostAPI
	}
	return out
}

func TestWindowsSilentHintNamesSelectedDevice(t *testing.T) {
	swap(t, &wakeSysPlatform, func() string { return "win32" })
	hint := SilentAudioHint(WakeInputDeviceDetails{Selector: 3, Name: ptr("Microphone Array"), HostAPI: ptr("Windows WASAPI")})
	if !strings.Contains(hint, "Microphone Array (Windows WASAPI)") || !strings.Contains(hint, "wake_word.input_device") || strings.Contains(hint, "macOS") {
		t.Fatal(hint)
	}
}

func TestMacosSilentHintPointsAtPrivacySettings(t *testing.T) {
	swap(t, &wakeSysPlatform, func() string { return "darwin" })
	hint := SilentAudioHint(WakeInputDeviceDetails{Selector: 1, Name: ptr("MacBook Pro Microphone"), HostAPI: ptr("Core Audio")})
	if !strings.Contains(hint, "Privacy & Security") || !strings.Contains(hint, "Microphone") {
		t.Fatal(hint)
	}
}

func TestLinuxSilentHintNamesSelectedDevice(t *testing.T) {
	swap(t, &wakeSysPlatform, func() string { return "linux" })
	hint := SilentAudioHint(WakeInputDeviceDetails{Selector: 2, Name: ptr("HD Audio Capture"), HostAPI: ptr("ALSA")})
	if !strings.Contains(hint, "HD Audio Capture (ALSA)") || strings.Contains(hint, "Privacy & Security") {
		t.Fatal(hint)
	}
}

func setWakeSingleton(det *WakeWordDetector) {
	wakeDetectorLock.Lock()
	wakeDetector = det
	wakeDetectorLock.Unlock()
}

func TestDetectorFlagsSilentStreamAndRecovers(t *testing.T) {
	swap(t, &wakeSilenceAlertSeconds, 0.001)
	var loud atomic.Bool
	useWakeAudio(t, &fakeWakeSD{inputStream: func(pysounddevice.StreamParams) (pysounddevice.InputStream, error) {
		if loud.Load() {
			return newFakeWakeStream(steadyRead(500, 5*time.Millisecond)), nil
		}
		return newFakeWakeStream(steadyRead(0, 5*time.Millisecond)), nil
	}})
	det := NewWakeWordDetector(newFakeWakeEngine(false), noWake, WakeDetectorOptions{})
	if err := det.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(det.Stop)
	defer setWakeSingleton(nil)
	if !waitFor(2*time.Second, det.AudioSilent) {
		t.Fatal("silent stream never flagged")
	}
	if AudioIsSilent() {
		t.Fatal("the module accessor needs the singleton")
	}
	setWakeSingleton(det)
	if !AudioIsSilent() {
		t.Fatal("singleton accessor missed the silent flag")
	}
	det.Pause()
	loud.Store(true)
	if err := det.Resume(); err != nil {
		t.Fatal(err)
	}
	if !waitFor(2*time.Second, func() bool { return !det.AudioSilent() }) {
		t.Fatal("audible input never cleared the flag")
	}
}

func TestDetectionCallbackCanPauseAndCloseStream(t *testing.T) {
	log := &wakeStreamLog{}
	useWakeAudio(t, &fakeWakeSD{inputStream: func(p pysounddevice.StreamParams) (pysounddevice.InputStream, error) {
		return log.add(p, newFakeWakeStream(steadyRead(0, 10*time.Millisecond))), nil
	}})
	buildWith(t, newFakeWakeEngine(true))
	lockIn(t)
	owner := new(int)
	paused := make(chan struct{})
	var once sync.Once
	onWake := func() error {
		if PauseListening(owner) {
			once.Do(func() { close(paused) })
		}
		return nil
	}
	if _, err := StartListening(context.Background(), onWake, owner, map[string]any{}, false); err != nil {
		t.Fatal(err)
	}
	select {
	case <-paused:
	case <-time.After(2 * time.Second):
		t.Fatal("callback never paused the listener")
	}
	if IsListening() {
		t.Fatal("paused listener still listening")
	}
	if closed, _, _ := log.first().snapshot(); !closed {
		t.Fatal("pause left the stream open")
	}
	if !StopListening(owner) {
		t.Fatal("owner could not stop its listener")
	}
}

func TestWedgedStreamHaltsWithoutBlockingReadAndAbortsBeforeClose(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	log := &wakeStreamLog{}
	useWakeAudio(t, &fakeWakeSD{inputStream: func(p pysounddevice.StreamParams) (pysounddevice.InputStream, error) {
		s := newFakeWakeStream(func(n int) ([]int16, error) {
			select {
			case <-time.After(30 * time.Second):
			case <-release:
			}
			return make([]int16, n), nil
		})
		s.available = 0
		return log.add(p, s), nil
	}})
	det := NewWakeWordDetector(newFakeWakeEngine(false), noWake, WakeDetectorOptions{})
	if err := det.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(det.Stop)
	if !det.Running() {
		t.Fatal("detector not running")
	}
	time.Sleep(200 * time.Millisecond)
	t0 := time.Now()
	det.Pause()
	if time.Since(t0) >= 1500*time.Millisecond {
		t.Fatal("pause() must not wait out the join timeout")
	}
	if det.Running() {
		t.Fatal("paused detector still running")
	}
	_, readCalls, calls := log.first().snapshot()
	if readCalls != 0 {
		t.Fatal("read() must not be entered while read_available < frame_length")
	}
	if len(calls) < 2 || !slices.Equal(calls[:2], []string{"abort", "close"}) {
		t.Fatalf("calls %v", calls)
	}
}

// slog records of one logger name at or above a level
type wakeLogCapture struct {
	mu      sync.Mutex
	level   slog.Level
	name    string
	records []string
	attrs   []slog.Attr
}

func (c *wakeLogCapture) Enabled(context.Context, slog.Level) bool { return true }
func (c *wakeLogCapture) Handle(_ context.Context, r slog.Record) error {
	name := ""
	for _, a := range c.attrs {
		if a.Key == "logger" {
			name = a.Value.String()
		}
	}
	if r.Level >= c.level && name == c.name {
		c.mu.Lock()
		c.records = append(c.records, r.Message)
		c.mu.Unlock()
	}
	return nil
}
func (c *wakeLogCapture) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &wakeLogView{c, append(append([]slog.Attr(nil), c.attrs...), attrs...)}
}
func (c *wakeLogCapture) WithGroup(string) slog.Handler { return c }

// Attribute bound view of a capture sharing its record list
type wakeLogView struct {
	c     *wakeLogCapture
	attrs []slog.Attr
}

func (v *wakeLogView) Enabled(context.Context, slog.Level) bool { return true }
func (v *wakeLogView) Handle(ctx context.Context, r slog.Record) error {
	name := ""
	for _, a := range v.attrs {
		if a.Key == "logger" {
			name = a.Value.String()
		}
	}
	if r.Level >= v.c.level && name == v.c.name {
		v.c.mu.Lock()
		v.c.records = append(v.c.records, r.Message)
		v.c.mu.Unlock()
	}
	return nil
}
func (v *wakeLogView) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &wakeLogView{v.c, append(append([]slog.Attr(nil), v.attrs...), attrs...)}
}
func (v *wakeLogView) WithGroup(string) slog.Handler { return v }

func captureWakeLogs(t *testing.T, level slog.Level) *wakeLogCapture {
	t.Helper()
	c := &wakeLogCapture{level: level, name: "tools.wake_word"}
	saved := slog.Default()
	slog.SetDefault(slog.New(c))
	t.Cleanup(func() { slog.SetDefault(saved) })
	return c
}

func (c *wakeLogCapture) matching(fragment string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, m := range c.records {
		if strings.Contains(m, fragment) {
			n++
		}
	}
	return n
}

func TestStarvedReadAvailableFallsBackToBlockingRead(t *testing.T) {
	log := &wakeStreamLog{}
	useWakeAudio(t, &fakeWakeSD{inputStream: func(p pysounddevice.StreamParams) (pysounddevice.InputStream, error) {
		s := newFakeWakeStream(steadyRead(0, 10*time.Millisecond))
		s.available = 0
		return log.add(p, s), nil
	}})
	swap(t, &wakeReadStallFallbackSeconds, 0.1)
	logs := captureWakeLogs(t, slog.LevelWarn)
	det := NewWakeWordDetector(newFakeWakeEngine(false), noWake, WakeDetectorOptions{})
	if err := det.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(det.Stop)
	if !det.Running() {
		t.Fatal("detector not running")
	}
	reads := func() int {
		if s := log.first(); s != nil {
			_, n, _ := s.snapshot()
			return n
		}
		return 0
	}
	waitFor(3*time.Second, func() bool { return reads() >= 5 })
	if reads() < 5 {
		t.Fatal("must block-read frames once read_available is starved")
	}
	if n := logs.matching("falling back to blocking reads"); n != 1 {
		t.Fatalf("the stall fallback warns exactly once per capture, got %d", n)
	}
	det.Pause()
	if det.Running() {
		t.Fatal("paused detector still running")
	}
}

func TestStarvedAndWedgedReadIsReleasedByHaltAbort(t *testing.T) {
	log := &wakeStreamLog{}
	useWakeAudio(t, &fakeWakeSD{inputStream: func(p pysounddevice.StreamParams) (pysounddevice.InputStream, error) {
		unblock := make(chan struct{})
		var once sync.Once
		s := newFakeWakeStream(func(n int) ([]int16, error) {
			select {
			case <-unblock:
			case <-time.After(30 * time.Second):
			}
			return make([]int16, n), nil
		})
		s.available = 0
		s.onAbort = func() { once.Do(func() { close(unblock) }) }
		return log.add(p, s), nil
	}})
	swap(t, &wakeReadStallFallbackSeconds, 0.1)
	swap(t, &wakeHaltJoinSeconds, 0.2)
	det := NewWakeWordDetector(newFakeWakeEngine(false), noWake, WakeDetectorOptions{})
	if err := det.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(det.Stop)
	if !det.Running() {
		t.Fatal("detector not running")
	}
	readCalls := func() int {
		if s := log.first(); s != nil {
			_, n, _ := s.snapshot()
			return n
		}
		return 0
	}
	waitFor(3*time.Second, func() bool { return readCalls() >= 1 })
	if readCalls() != 1 {
		t.Fatalf("starved guard must have fallen back into read(), calls %d", readCalls())
	}
	t0 := time.Now()
	det.Pause()
	if time.Since(t0) >= 2500*time.Millisecond {
		t.Fatal("pause() must abort the wedged read, not block on it")
	}
	if _, _, calls := log.first().snapshot(); !slices.Contains(calls, "abort") {
		t.Fatalf("halt must abort the capture to release the reader, calls %v", calls)
	}
	det.lock.Lock()
	th := det.thread
	det.lock.Unlock()
	if th != nil {
		th.join(2)
	}
	if det.Running() {
		t.Fatal("detector still running")
	}
}

func TestStartupFailureReleasesOwnerAndMachineLock(t *testing.T) {
	useWakeAudio(t, &fakeWakeSD{inputStream: func(pysounddevice.StreamParams) (pysounddevice.InputStream, error) {
		return nil, &os.PathError{Op: "open", Path: "mic", Err: errors.New("no microphone")}
	}})
	buildWith(t, newFakeWakeEngine(false))
	path := lockIn(t)
	owner := new(int)
	_, err := StartListening(context.Background(), noWake, owner, map[string]any{}, false)
	var we *WakeWordError
	if !errors.As(err, &we) || we.Class != "RuntimeError" || !strings.Contains(we.Msg, "Failed to open") {
		t.Fatalf("start error %v", err)
	}
	if OwnsListener(owner) {
		t.Fatal("failed start kept the lease")
	}
	handle, err := acquireMachineLock(path)
	if err != nil {
		t.Fatal(err)
	}
	releaseMachineLock(handle)
}

func TestStreamFailureReleasesOwnerAndMachineLock(t *testing.T) {
	useWakeAudio(t, &fakeWakeSD{inputStream: func(pysounddevice.StreamParams) (pysounddevice.InputStream, error) {
		return newFakeWakeStream(func(int) ([]int16, error) { return nil, errors.New("device disconnected") }), nil
	}})
	engine := newFakeWakeEngine(false)
	buildWith(t, engine)
	path := lockIn(t)
	owner := new(int)
	det, err := StartListening(context.Background(), noWake, owner, map[string]any{}, false)
	if err != nil {
		t.Fatal(err)
	}
	defer StopListening(owner)
	det.lock.Lock()
	th := det.thread
	det.lock.Unlock()
	th.join(10)
	if det.Running() || OwnsListener(owner) || !engine.closed.Load() {
		t.Fatalf("running %v owns %v closed %v", det.Running(), OwnsListener(owner), engine.closed.Load())
	}
	handle, err := acquireMachineLock(path)
	if err != nil {
		t.Fatal(err)
	}
	releaseMachineLock(handle)
}

const wakeLockHolderEnv = "ICEBATH_TEST_WAKE_LOCK_HOLDER"

// Child mode holding the machine lock until stdin delivers a line or closes
func dispatchWakeLockHolder() {
	path := os.Getenv(wakeLockHolderEnv)
	if path == "" {
		return
	}
	handle, err := acquireMachineLock(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	fmt.Println("ready")
	_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
	if handle == nil {
		os.Exit(3)
	}
	os.Exit(0)
}

func TestMachineLockIsReleasedWhenOwnerProcessExits(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wake.lock")
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), wakeLockHolderEnv+"="+path)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		stdin.Close()
		if cmd.ProcessState == nil {
			cmd.Process.Kill()
			cmd.Wait()
		}
	}()
	ready := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(stdout).ReadString('\n')
		ready <- line
	}()
	select {
	case line := <-ready:
		if strings.TrimSpace(line) != "ready" {
			t.Fatalf("child said %q", line)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("lock holder never became ready")
	}
	_, err = acquireMachineLock(path)
	var inUse *WakeWordInUse
	if !errors.As(err, &inUse) {
		t.Fatalf("second acquire = %v, want WakeWordInUse", err)
	}
	if _, err := stdin.Write([]byte("release\n")); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil || cmd.ProcessState.ExitCode() != 0 {
		t.Fatalf("holder exit %v", err)
	}
	handle, err := acquireMachineLock(path)
	if err != nil {
		t.Fatal(err)
	}
	releaseMachineLock(handle)
}

func TestClientCaptureFeedFires(t *testing.T) {
	buildWith(t, newFakeWakeEngine(true))
	lockIn(t)
	swap(t, &importAudio, func() (pysounddevice.Module, error) {
		return nil, &os.PathError{Op: "open", Path: "mic", Err: errors.New("no local mic")}
	})
	owner := new(int)
	fired := make(chan struct{})
	var once sync.Once
	onWake := func() error { once.Do(func() { close(fired) }); return nil }
	if _, err := StartListening(context.Background(), onWake, owner, map[string]any{}, true); err != nil {
		t.Fatal(err)
	}
	if !IsListening() {
		t.Fatal("client listener not listening")
	}
	fl := DetectorFrameInfo().FrameLength
	pcm := make([]byte, 2*fl)
	for i := 0; i < fl; i++ {
		binary.NativeEndian.PutUint16(pcm[2*i:], uint16(5000))
	}
	if ok, err := FeedAudio(owner, pcm); err != nil || !ok {
		t.Fatalf("feed = %v, %v", ok, err)
	}
	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("fed frame never fired")
	}
	if !StopListening(owner) {
		t.Fatal("owner could not stop its listener")
	}
}

func TestFeedAudioRejectsWrongOwner(t *testing.T) {
	buildWith(t, newFakeWakeEngine(false))
	lockIn(t)
	owner := new(int)
	if _, err := StartListening(context.Background(), noWake, owner, map[string]any{}, true); err != nil {
		t.Fatal(err)
	}
	if ok, err := FeedAudio(new(int), []byte{0, 0}); err != nil || ok {
		t.Fatalf("foreign feed = %v, %v", ok, err)
	}
	if !StopListening(owner) {
		t.Fatal("owner could not stop its listener")
	}
}
