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
			"then turn Listen hands-free off and on."
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
		return "It listens on " + label + ". Pick another Microphone, then turn Listen hands-free off and on."
	}
	return "It listens on " + label + ". Check that this microphone is the right one and not muted, then turn Listen hands-free off and on."
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
	return Listener(m.speech, w)
}

// Engine the wake settings pick on this machine: auto taking the open vocabulary engine unless a model file is set; false for a name no
// engine has
func Listener(sp *speech.Speech, w *speechv1.Wake) (speech.Engine, bool) {
	model := false
	for _, e := range sp.Engines() {
		if e.Model != "" && strings.TrimSpace(w.GetSettings()[e.Model]) != "" {
			model = true
		}
	}
	return sp.WakeEngine(w.GetProvider(), model)
}

// Whether the wake word can listen on this machine, and when it can't, what keeps it and what to do as the page of the wake word says it
type WakeCheck struct {
	Available bool
	// Engine the settings pick, by name and by title
	Engine, Title string
	Phrase        string
	Installed     bool
	Audio         bool
	Input         bool
	Key           bool
	STT           bool
	// What keeps it from listening, a clause; empty when nothing does
	Cause string
	// What to do, a sentence naming the row of the page of the wake word that does it
	Fix string
	// That row, as AtListener, AtMicrophone or AtRecognition name it, empty for none on the page; and what to do there first, said when
	// turning the wake word on lands there
	Row, First string
}

// Rows of the page of the wake word that do a step the wake word misses
const (
	AtListener    = "listener"
	AtMicrophone  = "microphone"
	AtRecognition = "recognition"
)

// What keeps the wake word from listening as one message, empty when nothing does
func (c WakeCheck) Line() string {
	if c.Cause == "" {
		return ""
	}
	return strings.TrimSpace("The wake word can't listen yet: " + c.Cause + ". " + c.Fix)
}

func supported(sp *speech.Speech, e speech.Engine) bool {
	return e.Supported == nil || e.Supported(sp)
}

func installed(sp *speech.Speech, e speech.Engine, w *speechv1.Wake) bool {
	if e.Installed == nil {
		return supported(sp, e)
	}
	return e.Installed(sp, w.GetSettings())
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

// State words of a listener, the same on its row and in every sentence
const (
	NeedsAccessKey = "Needs an access key"
	NeedsModelFile = "Needs a model file"
)

// How a listener stands on this machine with the wake settings: its state words, what keeps it from listening with what to do about it as
// the page of the wake word says it, whose Listener row does it, and what to do first as a note says it
type Standing struct {
	State, Cause, Fix, First string
}

// The one answer to how a listener stands here, picked by name or by Automatic alike: its build, its model file, its key and its install,
// never installing anything
func EngineStanding(sp *speech.Speech, w *speechv1.Wake, e speech.Engine) Standing {
	switch {
	case !installed(sp, e, w) && !supported(sp, e):
		var others []string
		for _, other := range sp.Engines() {
			if other.Name != e.Name && supported(sp, other) {
				others = append(others, other.Title)
			}
		}
		s := Standing{State: speech.CantRun, Cause: e.Title + " has no build for this computer", First: "Pick another listener"}
		if len(others) > 0 {
			s.Fix = "Listener picks " + strs.Or(others) + "."
		}
		return s
	case e.Model != "" && strings.TrimSpace(w.GetSettings()[e.Model]) == "":
		return Standing{State: NeedsModelFile, Cause: fmt.Sprintf("%s has no model file for “%s”", e.Title, w.GetPhrase()),
			Fix: "Listener takes one, or picks Automatic.", First: "Give " + e.Title + " a model file"}
	case e.Key && strings.TrimSpace(w.GetAccessKey()) == "":
		return Standing{State: NeedsAccessKey, Cause: e.Title + " needs a Picovoice access key",
			Fix: "Listener takes the key, which console.picovoice.ai gives free.", First: "Give " + e.Title + " its access key"}
	case !installed(sp, e, w):
		return Standing{State: speech.NotInstalled, Cause: e.Title + " is not installed", Fix: "Listener installs it.", First: "Install " + e.Title}
	}
	return Standing{State: speech.Ready}
}

// The one answer to whether the wake word can listen here with the voice settings of a profile, the listener picked by name or by Automatic
// alike: how its listener stands, the sound system, a microphone, then the speech to text that turns what you say after the phrase into
// your message, never installing anything. Replies read aloud are no part of it, that is Speak replies
func CheckWake(sp *speech.Speech, settings *speechv1.VoiceReply) WakeCheck {
	w := settings.GetWake()
	check := WakeCheck{Phrase: w.GetPhrase(), Key: true}
	stt := settings.GetRecognition().GetHindrance()
	check.STT = stt == nil
	e, known := Listener(sp, w)
	if !known {
		check.Cause, check.Fix, check.Row, check.First = "no listener is called "+w.GetProvider(), "Listener picks another.", AtListener, "Pick another listener"
		return check
	}
	check.Engine, check.Title = e.Name, e.Title
	check.Installed = installed(sp, e, w)
	if check.Installed {
		check.Audio = audioLoads()
		check.Input = check.Audio && inputReady()
	}
	standing := EngineStanding(sp, w, e)
	check.Key = standing.State != NeedsAccessKey
	switch {
	case standing.Cause != "":
		check.Cause, check.Fix, check.Row, check.First = standing.Cause, standing.Fix, AtListener, standing.First
	case !check.Audio:
		p := captureMissing()
		if p == nil {
			p = &Problem{Cause: "the sound system did not start"}
		}
		check.Cause, check.Fix, check.First = p.Cause, p.Fix, "Make the sound system start"
	case !check.Input:
		check.Cause, check.Fix, check.Row, check.First = "no microphone was found", "Microphone picks one once it is connected.", AtMicrophone,
			"Connect a microphone"
	case !check.STT:
		check.Cause, check.Fix, check.Row, check.First = stt.GetCause(), Fix(stt, "Speech to text"), AtRecognition, "Set up speech to text"
	}
	check.Available = check.Cause == ""
	return check
}

// Whether the wake word can listen here with the settings last read
func (m *Mode) WakeCheck() WakeCheck {
	return CheckWake(m.speech, m.Settings())
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

// What arming the wake word came to: listening for the phrase with the engine of the title, else why not, with what to do
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
		slog.Warn(fmt.Sprintf("wake.start(%s): not available — %s", surface, check.Line()))
		return WakeStart{Reason: WakeUnavailable, Hint: check.Line()}
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
	routed := w.GetRouting()
	d := &detector{engine: hotword, input: inputSelector(w.GetInputDevice()), failed: m.wakeFailed}
	l := &listener{detector: d, release: release, phrase: w.GetPhrase(), engine: e.Title, surface: surface}
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
		// Start a new session is read as the phrase is heard, so a change to it counts at once
		fresh := m.Settings().GetWake().GetNewSession()
		if m.events.Woke != nil {
			call(func() { m.events.Woke(phrase, profile, routed, fresh) })
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
	state := WakeState{Phrase: check.Phrase, Engine: check.Title, Enabled: w.GetEnabled(), Check: check, Hint: check.Line()}
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
		state.Hint = "The wake word microphone was not found. Pick another Microphone. The error was: " + strings.TrimSuffix(state.Device.Error, ".") + "."
	}
	if state.Silent && state.Hint == "" {
		state.Hint = silentHint(state.Device)
	}
	return state
}
