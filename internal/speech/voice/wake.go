package voice

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/packages"
	"github.com/nickheyer/icebath/internal/speech"
	speechconfig "github.com/nickheyer/icebath/internal/speech/config"
	"github.com/nickheyer/icebath/internal/speech/portaudio"
	"github.com/nickheyer/icebath/pkg/fsx"
	speechv1 "github.com/nickheyer/icebath/pkg/gen/icebath/speech/v1"
	"github.com/nickheyer/icebath/pkg/strs"
)

const (
	// Loudness under which a frame counts as silence
	silencePeak  = 10
	cooldown     = 2 * time.Second
	startTimeout = 5 * time.Second
	readPoll     = 50 * time.Millisecond
	stallLimit   = 2 * time.Second
	haltJoin     = 2 * time.Second
	silenceAlert = 10 * time.Second
)

// Another app or process holds the wake word microphone
var ErrOwned = errors.New("Another icebath window already listens for the wake word.")

// Microphone a wake_word.input_device setting names: a whole number a device number, blank or a boolean the default
func inputSelector(text string) any {
	text = strings.TrimSpace(text)
	if text == "" || strings.EqualFold(text, "true") || strings.EqualFold(text, "false") {
		return nil
	}
	if index, err := strconv.Atoi(text); err == nil {
		return index
	}
	return text
}

// What the input device of a selector turns out to be
type Device struct {
	Selector  any
	Error     string
	Name      string
	Channels  int
	Rate      float64
	HostIndex int
	HostAPI   string
}

// The input device a selector names, from PortAudio of this machine
func describe(selector any, pa *portaudio.Audio) Device {
	d := Device{Selector: selector}
	var a portaudio.Audio
	if pa != nil {
		a = *pa
	} else {
		var err error
		if a, err = audio(); err != nil {
			d.Error = err.Error()
			return d
		}
	}
	info, err := a.Device(selector, portaudio.Input)
	if err != nil {
		d.Error = err.Error()
		return d
	}
	d.Name, d.Channels, d.Rate, d.HostIndex = info.Name, info.MaxInputChannels, info.DefaultSampleRate, info.HostAPI
	if host, err := a.HostAPI(info.HostAPI); err == nil {
		d.HostAPI = host.Name
	}
	return d
}

// How to fix a microphone that delivers only silence
func silentHint(d Device) string {
	if runtime.GOOS == "darwin" {
		return "Let your terminal or the icebath desktop app use the microphone in System Settings › Privacy & Security › Microphone, " +
			"then run /wake off and /wake on."
	}
	label := strings.TrimSpace(d.Name)
	if label == "" {
		if d.Selector == nil {
			label = "the default microphone"
		} else {
			label = "the microphone " + fmt.Sprint(d.Selector)
		}
	}
	if host := strings.TrimSpace(d.HostAPI); host != "" {
		label += " through " + host
	}
	if runtime.GOOS == "windows" {
		return "It listens on " + label + ". Pick another Microphone in /settings under Wake word, then run /wake off and /wake on."
	}
	return "It listens on " + label + ". Check that this microphone is the right one and not muted, then run /wake off and /wake on."
}

// One capture block at the microphone's rate as an exact engine frame, averaged down or interpolated up
func resample(frame []int16, length int) []int16 {
	n := len(frame)
	if n == length {
		return append([]int16(nil), frame...)
	}
	out := make([]int16, length)
	if n == 0 {
		return out
	}
	values := make([]float64, length)
	if n > length {
		raw := linspace(0, float64(n), length+1, true)
		edges := make([]int, len(raw))
		for i, e := range raw {
			edges[i] = int(math.Floor(e))
		}
		for i := range length {
			lo, hi := edges[i], n
			if i+1 < length {
				hi = edges[i+1]
			}
			sum := float64(frame[lo])
			for _, v := range frame[min(lo+1, hi):hi] {
				sum += float64(v)
			}
			values[i] = sum / float64(edges[i+1]-edges[i])
		}
	} else {
		for i, x := range linspace(0, float64(n-1), length, true) {
			j := int(math.Floor(x))
			if j >= n-1 {
				values[i] = float64(frame[n-1])
				continue
			}
			values[i] = float64(frame[j]) + (float64(frame[j+1])-float64(frame[j]))*(x-float64(j))
		}
	}
	for i, v := range values {
		out[i] = int16(min(max(math.RoundToEven(v), math.MinInt16), math.MaxInt16))
	}
	return out
}

// Hotword listener of this app: its engine kept across pauses, its microphone opened while it listens
type detector struct {
	engine speech.Hotword
	input  any
	onWake func()
	failed func(*detector)

	mu      sync.Mutex
	device  Device
	stop    chan struct{}
	done    chan struct{}
	stream  *portaudio.Stream
	silent  bool
	quiet   int
	last    time.Time
	firing  bool
	skipped bool
}

func (d *detector) running() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.done == nil {
		return false
	}
	select {
	case <-d.done:
		return false
	default:
		return true
	}
}

// Opens the microphone and listens, nothing while it runs
func (d *detector) start() error {
	if d.running() {
		return nil
	}
	ready := make(chan error, 1)
	d.mu.Lock()
	d.stop, d.done = make(chan struct{}), make(chan struct{})
	stop, done := d.stop, d.done
	d.mu.Unlock()
	go func() {
		defer close(done)
		d.run(stop, ready)
	}()
	select {
	case err := <-ready:
		if err != nil {
			d.halt()
			return fmt.Errorf("The wake word microphone did not open: %s.", strings.TrimSuffix(err.Error(), "."))
		}
		return nil
	case <-time.After(startTimeout):
		d.halt()
		return errors.New("The wake word microphone did not open within " + strs.Span(startTimeout) + ".")
	}
}

// Signals the reader and waits for it, aborting a microphone wedged past the wait
func (d *detector) halt() {
	d.mu.Lock()
	stop, done := d.stop, d.done
	if stop != nil {
		select {
		case <-stop:
		default:
			close(stop)
		}
	}
	d.mu.Unlock()
	if done == nil {
		return
	}
	select {
	case <-done:
	case <-time.After(haltJoin):
		d.mu.Lock()
		stream := d.stream
		d.mu.Unlock()
		if stream != nil {
			stream.Abort()
			stream.Close()
		}
	}
}

// Opens the microphone at its own rate, blocks sized for one engine frame
func (d *detector) open(frame int) (*portaudio.Stream, int, int, error) {
	pa, err := audio()
	if err != nil {
		if unloadable(err) || missingLibrary(err) {
			slog.Error(fmt.Sprintf("wake word: audio libraries unavailable: %s", err))
		}
		return nil, 0, 0, err
	}
	device := describe(d.input, &pa)
	d.mu.Lock()
	d.device = device
	d.mu.Unlock()
	rate := speech.WakeRate
	if device.Rate > 0 && !math.IsInf(device.Rate, 0) {
		rate = int(math.RoundToEven(device.Rate))
	}
	length := max(1, int(math.RoundToEven(float64(frame*rate)/speech.WakeRate)))
	selector, defaultRate := "default", "unknown"
	if d.input != nil {
		selector = fmt.Sprintf("%#v", d.input)
	}
	if device.Rate != 0 {
		defaultRate = strconv.FormatFloat(device.Rate, 'f', -1, 64)
	}
	slog.Info(fmt.Sprintf("wake word: opening microphone device=%s selector=%s hostapi=%s default_rate=%s capture_rate=%d engine_rate=%d",
		cmpOr(device.Name, "system default"), selector, cmpOr(device.HostAPI, "unknown"), defaultRate, rate, speech.WakeRate))
	stream, err := pa.OpenInput(portaudio.StreamParams{Device: d.input, SampleRate: float64(rate), BlockSize: length, Channels: 1}, nil)
	if err == nil {
		if err = stream.Start(); err != nil {
			stream.Close()
		}
	}
	if err != nil {
		slog.Error(fmt.Sprintf("wake word: failed to open microphone: %s", err))
		return nil, 0, 0, err
	}
	return stream, rate, length, nil
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// One block, polling what is ready against the stop so a wedged device never blocks the reader
func (d *detector) read(stream *portaudio.Stream, length int, stop chan struct{}) ([]int16, bool, error) {
	if _, err := stream.ReadAvailable(); err != nil {
		return nil, false, err
	}
	if !d.skipped {
		stalled := time.Duration(0)
		for {
			n, err := stream.ReadAvailable()
			if err != nil {
				return nil, false, err
			}
			if n >= length {
				break
			}
			select {
			case <-stop:
				return nil, false, nil
			case <-time.After(readPoll):
			}
			if stalled += readPoll; stalled >= stallLimit {
				slog.Warn(fmt.Sprintf("wake word: read_available stuck below frame length for %.1fs — falling back to blocking reads", stalled.Seconds()))
				d.skipped = true
				break
			}
		}
	}
	data, _, err := stream.Read(length)
	return data, err == nil, err
}

// Counts frames in a row near zero and flags the silent state
func (d *detector) hush(frame []int16, alert int) {
	peak := 0
	for _, v := range frame {
		peak = max(peak, int(math.Abs(float64(v))))
	}
	if peak <= silencePeak {
		if d.quiet++; d.quiet == alert {
			d.mu.Lock()
			d.silent = true
			device := d.device
			d.mu.Unlock()
			if len(frame) > 0 {
				slog.Warn(fmt.Sprintf("wake word: mic delivers only silence (peak<=%d for %ds); %s", silencePeak, int(silenceAlert.Seconds()), silentHint(device)))
			}
		}
		return
	}
	if d.quiet != 0 {
		d.mu.Lock()
		if d.silent {
			slog.Info("wake word: mic audio detected — stream healthy")
		}
		d.silent = false
		d.mu.Unlock()
		d.quiet = 0
	}
}

// Fires the wake callback on its own goroutine once, the cooldown honored
func (d *detector) fire() {
	if time.Since(d.last) < cooldown {
		slog.Debug("wake word: detection within cooldown — ignored")
		return
	}
	d.last = time.Now()
	slog.Info("wake word: phrase detected — firing callback")
	d.mu.Lock()
	if d.firing {
		d.mu.Unlock()
		return
	}
	d.firing = true
	d.mu.Unlock()
	go func() {
		defer func() {
			d.mu.Lock()
			d.firing = false
			d.mu.Unlock()
		}()
		call(d.onWake)
	}()
}

// Reader: scores frames, reopens a failing microphone within its budget and reports a death nobody asked for
func (d *detector) run(stop chan struct{}, ready chan error) {
	frame := d.engine.FrameLength()
	stream, rate, length, err := d.open(frame)
	if err != nil {
		ready <- err
		return
	}
	d.mu.Lock()
	d.stream = stream
	d.mu.Unlock()
	d.engine.Reset()
	slog.Info(fmt.Sprintf("wake word: listening (frame=%d, rate=%d)", frame, speech.WakeRate))
	ready <- nil
	failed := false
	retries := []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second}
	alert := max(1, int(silenceAlert.Seconds()*speech.WakeRate/float64(max(1, frame))))
	defer func() {
		d.mu.Lock()
		if d.stream != nil {
			d.stream.Abort()
			d.stream.Close()
			d.stream = nil
		}
		d.mu.Unlock()
		slog.Info("wake word: stream closed")
		if failed && d.failed != nil {
			d.failed(d)
		}
	}()
	stopped := func() bool {
		select {
		case <-stop:
			return true
		default:
			return false
		}
	}
	for !stopped() {
		data, ok, err := d.read(stream, length, stop)
		if err != nil {
			slog.Warn(fmt.Sprintf("wake word: stream read error: %s", err))
			stream.Abort()
			stream.Close()
			d.mu.Lock()
			d.stream = nil
			d.mu.Unlock()
			stream = nil
			for len(retries) > 0 && stream == nil && !stopped() {
				delay := retries[0]
				retries = retries[1:]
				slog.Warn(fmt.Sprintf("wake word: reopening microphone in %.1fs", delay.Seconds()))
				select {
				case <-stop:
				case <-time.After(delay):
					if stream, rate, length, err = d.open(frame); err != nil {
						slog.Warn(fmt.Sprintf("wake word: microphone reopen failed: %s", err))
						stream = nil
					}
				}
			}
			if stream == nil {
				if !stopped() {
					slog.Warn("wake word: microphone recovery exhausted; toggle wake word to retry")
					failed = true
				}
				return
			}
			d.mu.Lock()
			d.stream, d.silent = stream, false
			d.mu.Unlock()
			d.quiet = 0
			d.engine.Reset()
			continue
		}
		if !ok {
			d.hush(nil, alert)
			continue
		}
		if rate != speech.WakeRate {
			data = resample(data, frame)
		}
		d.hush(data, alert)
		fired, err := d.engine.Process(data)
		if err != nil {
			slog.Debug(fmt.Sprintf("wake word: engine error: %s", err))
			continue
		}
		if fired {
			d.fire()
		}
	}
}

// Wake listener of this app with its machine wide lease on the microphone
type listener struct {
	detector *detector
	release  func()
	phrase   string
	engine   string
	surface  string
}

// Settings of the wake word for the engines of this machine
func (m *Mode) wakeOf(w *speechv1.Wake) *speech.Wake {
	wake := &speech.Wake{Speech: m.speech, Phrase: w.GetPhrase(), Sensitivity: w.GetSensitivity(), Frames: int(w.GetFrames()), Routing: w.GetRouting(),
		Profile: m.Settings().GetProfile(), Settings: w.GetSettings(), AccessKey: strings.TrimSpace(w.GetAccessKey())}
	for _, p := range w.GetProfiles() {
		wake.Profiles = append(wake.Profiles, speech.Phrase{Profile: p.GetProfile(), Phrase: p.GetPhrase()})
	}
	return wake
}

// Engine the settings pick: auto taking the open vocabulary engine unless a model file is set
func (m *Mode) engine(w *speechv1.Wake) (speech.Engine, bool) {
	model := false
	for _, e := range m.speech.Engines() {
		if e.Model != "" && strings.TrimSpace(w.GetSettings()[e.Model]) != "" {
			model = true
		}
	}
	return m.speech.WakeEngine(w.GetProvider(), model)
}

// Whether the wake word can listen on this machine, and how to make it when it can't
type WakeCheck struct {
	Available bool
	Engine    string
	Phrase    string
	Installed bool
	Audio     bool
	Input     bool
	Key       bool
	STT       bool
	TTS       bool
	Hint      string
}

func supported(sp *speech.Speech, e speech.Engine) bool {
	return e.Supported == nil || e.Supported(sp)
}

func installed(sp *speech.Speech, e speech.Engine) bool {
	if e.Installed == nil {
		return supported(sp, e)
	}
	return e.Installed(sp)
}

// Whether a microphone is there
func inputReady() bool {
	pa, err := audio()
	if err != nil {
		return false
	}
	if devices, err := pa.Devices(); err == nil {
		for _, d := range devices {
			if d.MaxInputChannels > 0 {
				return true
			}
		}
	}
	d, err := pa.Device(nil, portaudio.Input)
	return err == nil && d.MaxInputChannels > 0
}

// Checks engine, install, key, microphone, recognizer and voice without installing anything
func (m *Mode) WakeCheck() WakeCheck {
	settings := m.Settings()
	w := settings.GetWake()
	check := WakeCheck{Phrase: w.GetPhrase(), Key: true}
	check.STT = settings.GetRecognition().GetLabel() != ""
	check.TTS = settings.GetSpeaking().GetReady()
	e, known := m.engine(w)
	if !known {
		check.Hint = fmt.Sprintf("No listener is named %s. Pick another Listener in /settings under Wake word.", w.GetProvider())
		return check
	}
	check.Engine = e.Name
	check.Installed = installed(m.speech, e)
	platform := check.Installed || supported(m.speech, e)
	if check.Installed {
		check.Audio = audioLoads()
		check.Input = inputReady()
	}
	modelOK := e.Model == "" || strings.TrimSpace(w.GetSettings()[e.Model]) != ""
	auto := w.GetProvider() == "" || w.GetProvider() == speechconfig.AutoWake
	switch {
	case !platform && auto && e.Open:
		check.Hint = fmt.Sprintf("%s, the listener that hears “%s”, has no build for this computer.", e.Title, w.GetPhrase())
		for _, other := range m.speech.Engines() {
			if other.Model != "" && supported(m.speech, other) {
				check.Hint += " Give " + other.Title + " a model file trained for your phrase in /settings under Wake word › " + other.Title + "."
				break
			}
		}
	case !platform:
		var others []string
		for _, other := range m.speech.Engines() {
			if other.Name != e.Name && other.Model == "" && supported(m.speech, other) {
				others = append(others, other.Title)
			}
		}
		check.Hint = e.Title + " has no build for this computer."
		if len(others) > 0 {
			check.Hint += " Set Listener in /settings under Wake word to " + strings.Join(others, " or ") + "."
		}
	case !modelOK:
		check.Hint = "Give " + e.Title + " a model file trained for your phrase in /settings under Wake word › " + e.Title +
			fmt.Sprintf(", or set Listener there to Automatic so a listener that hears any phrase listens for “%s”.", w.GetPhrase())
	case e.Key && strings.TrimSpace(w.GetAccessKey()) == "":
		check.Key = false
		check.Hint = e.Title + " needs a Picovoice access key. Get one free at https://console.picovoice.ai, then set PORCUPINE_ACCESS_KEY in your keys file."
	case !check.Installed:
		check.Hint = e.Title + " is not installed. Run " + packages.InstallHint(e.Extra) + " to install it."
	case !check.Audio:
		check.Hint = "Listening needs PortAudio, the sound library icebath records with, and a working microphone."
	case !check.STT || !check.TTS:
		var missing, screens []string
		if !check.STT {
			missing, screens = append(missing, "speech to text"), append(screens, "/recognition")
		}
		if !check.TTS {
			missing, screens = append(missing, "text to speech"), append(screens, "/speech")
		}
		them := "it"
		if len(missing) > 1 {
			them = "them"
		}
		check.Hint = fmt.Sprintf("The wake word needs %s. Set %s up in %s.", strings.Join(missing, " and "), them, strings.Join(screens, " and "))
	}
	check.Available = platform && check.Key && modelOK && check.STT && check.TTS && check.Installed && check.Audio
	return check
}

// Whether a surface may host the listener
func surfaceAllowed(w *speechv1.Wake, enabled bool, surface string) bool {
	want := strings.ToLower(strings.TrimSpace(w.GetSurface()))
	return enabled && (want == "" || want == speechconfig.AutoWake || want == strings.ToLower(strings.TrimSpace(surface)))
}

// Why a wake start did nothing
const (
	WakeUnavailable = "unavailable"
	WakeDisabled    = "disabled"
	WakeElsewhere   = "disabled_for_surface"
	WakeOwned       = "owned"
)

// What arming the wake word came to
type WakeStart struct {
	Started bool
	Reason  string
	Hint    string
	Phrase  string
	Engine  string
}

// Arms the wake word on this surface, the machine wide lease taken; refused while disabled, scoped elsewhere, owned or not ready. Enabled
// tells it the person just turned it on
func (m *Mode) StartWake(ctx context.Context, surface string, enabled bool) WakeStart {
	settings := m.Settings()
	w := settings.GetWake()
	check := m.WakeCheck()
	if !check.Available {
		slog.Warn(fmt.Sprintf("wake.start(%s): not available — %s", surface, check.Hint))
		return WakeStart{Reason: WakeUnavailable, Hint: check.Hint}
	}
	enabled = enabled || w.GetEnabled()
	if !surfaceAllowed(w, enabled, surface) {
		reason := WakeElsewhere
		if !enabled {
			reason = WakeDisabled
		}
		slog.Info(fmt.Sprintf("wake.start(%s): %s (enabled=%t, surface=%q)", surface, reason, enabled, w.GetSurface()))
		return WakeStart{Reason: reason}
	}
	m.mu.Lock()
	if l := m.wake; l != nil {
		m.mu.Unlock()
		if err := l.detector.start(); err != nil {
			return WakeStart{Reason: WakeUnavailable, Hint: err.Error()}
		}
		return WakeStart{Started: true, Phrase: l.phrase, Engine: l.engine}
	}
	m.mu.Unlock()
	release, ok, err := fsx.TryLock(filepath.Join(m.speech.Dir(), "runtime", "wake-word.lock"))
	if err != nil {
		return WakeStart{Reason: WakeUnavailable, Hint: err.Error()}
	}
	if !ok {
		return WakeStart{Reason: WakeOwned, Hint: ErrOwned.Error()}
	}
	e, _ := m.engine(w)
	wake := m.wakeOf(w)
	hotword, err := e.New(context.WithoutCancel(ctx), wake)
	if err != nil {
		release()
		slog.Warn(fmt.Sprintf("wake.start(%s): failed to start listener: %s", surface, err))
		return WakeStart{Reason: WakeUnavailable, Hint: err.Error()}
	}
	fresh := w.GetNewSession()
	d := &detector{engine: hotword, input: inputSelector(w.GetInputDevice()), failed: m.wakeFailed}
	l := &listener{detector: d, release: release, phrase: w.GetPhrase(), engine: e.Name, surface: surface}
	d.onWake = func() {
		if !m.pauseWake() {
			return
		}
		phrase, profile := l.phrase, ""
		if matcher, ok := hotword.(speech.Matcher); ok {
			if p, owner, matched := matcher.LastMatch(); matched {
				phrase, profile = cmpOr(p, l.phrase), owner
			}
		}
		slog.Info(fmt.Sprintf("wake.detected: profile=%q", profile))
		if m.events.Woke != nil {
			call(func() { m.events.Woke(phrase, profile, fresh) })
		}
	}
	m.mu.Lock()
	m.wake = l
	m.mu.Unlock()
	if err := d.start(); err != nil {
		m.StopWake()
		return WakeStart{Reason: WakeUnavailable, Hint: err.Error()}
	}
	slog.Info(fmt.Sprintf("wake.start(%s): listening for %q (%s)", surface, l.phrase, l.engine))
	return WakeStart{Started: true, Phrase: l.phrase, Engine: l.engine}
}

// The microphone died with nobody stopping it: the lease given up
func (m *Mode) wakeFailed(d *detector) {
	m.mu.Lock()
	l := m.wake
	if l == nil || l.detector != d {
		m.mu.Unlock()
		return
	}
	m.wake = nil
	m.mu.Unlock()
	d.engine.Close()
	l.release()
}

// Stops the wake word and gives up the lease: what it listened for, false when it was not running
func (m *Mode) StopWake() (string, string, bool) {
	m.mu.Lock()
	l := m.wake
	m.wake = nil
	m.mu.Unlock()
	if l == nil {
		return "", "", false
	}
	l.detector.halt()
	l.detector.engine.Close()
	l.release()
	return l.phrase, l.engine, true
}

// Frees the microphone for a recording, the engine kept: whether a listener was paused
func (m *Mode) pauseWake() bool {
	m.mu.Lock()
	l := m.wake
	m.mu.Unlock()
	if l == nil {
		return false
	}
	l.detector.halt()
	return true
}

// Takes the microphone back after a recording, retried in the background while it is still busy
func (m *Mode) resumeWake() {
	m.mu.Lock()
	l := m.wake
	m.mu.Unlock()
	if l == nil || l.detector.running() {
		return
	}
	if err := l.detector.start(); err == nil {
		return
	} else {
		slog.Debug(fmt.Sprintf("wake resume failed (will retry): %s", err))
	}
	go func() {
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			time.Sleep(time.Second)
			m.mu.Lock()
			current := m.wake
			m.mu.Unlock()
			if current != l || l.detector.running() {
				return
			}
			if l.detector.start() == nil {
				slog.Info("wake: detector resumed after retry")
				return
			}
		}
		slog.Warn("wake: could not resume detector after voice turn (microphone still busy?) — toggle the wake word to re-arm")
	}()
}

// Resumes the wake word after its phrase was heard and nothing recorded
func (m *Mode) ResumeWake() { m.resumeWake() }

// How the wake word stands on this app
type WakeState struct {
	Listening bool
	Silent    bool
	Phrase    string
	Engine    string
	Enabled   bool
	Check     WakeCheck
	Device    Device
	Hint      string
}

func (m *Mode) WakeStatus() WakeState {
	w := m.Settings().GetWake()
	check := m.WakeCheck()
	state := WakeState{Phrase: check.Phrase, Engine: check.Engine, Enabled: w.GetEnabled(), Check: check, Hint: check.Hint}
	m.mu.Lock()
	l := m.wake
	m.mu.Unlock()
	if l != nil {
		state.Listening = l.detector.running()
		l.detector.mu.Lock()
		state.Silent, state.Device = state.Listening && l.detector.silent, l.detector.device
		l.detector.mu.Unlock()
		state.Phrase, state.Engine = l.phrase, l.engine
	} else {
		state.Device = describe(inputSelector(w.GetInputDevice()), nil)
	}
	if state.Device.Error != "" && state.Hint == "" {
		state.Hint = "The wake word microphone could not be found: " + strings.TrimSuffix(state.Device.Error, ".") + "."
	}
	if state.Silent && state.Hint == "" {
		state.Hint = silentHint(state.Device)
	}
	return state
}
