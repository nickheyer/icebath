//go:build !icebath_core && !icebath_noextras

package media

import (
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pysounddevice"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
)

// Anything the reader can wait on for the halt signal
type wakeWaiter interface {
	Wait(timeout float64) bool
}

// Halt event of one detector, its wait replaceable like the instance attribute a test patches
type wakeStop struct {
	ev   pythread.Event
	wait func(timeout float64) bool
}

func (s *wakeStop) Set()        { s.ev.Set() }
func (s *wakeStop) Clear()      { s.ev.Clear() }
func (s *wakeStop) IsSet() bool { return s.ev.IsSet() }

// Waits for the halt signal through the patched wait when one is installed
func (s *wakeStop) Wait(timeout float64) bool {
	if s.wait != nil {
		return s.wait(timeout)
	}
	return s.ev.Wait(timeout)
}

// One armed audio source, the reader takes blocks from it until it fails or the halt is signalled
type wakeCapture interface {
	Read(stop wakeWaiter) ([]int16, bool, error)
	Close()
	Rate() int
}

// A PortAudio input stream on the backend microphone
type wakeAudioCapture struct {
	stream            pysounddevice.InputStream
	rate              int
	frameLength       int
	skipAvailablePoll bool
}

func (c *wakeAudioCapture) Rate() int { return c.rate }

// One raw block, polling read_available against the halt so a wedged device never blocks the reader
func (c *wakeAudioCapture) Read(stop wakeWaiter) ([]int16, bool, error) {
	if _, err := c.stream.ReadAvailable(); err != nil {
		return nil, false, err
	}
	if stop != nil && !c.skipAvailablePoll {
		stalled := 0.0
		for {
			n, err := c.stream.ReadAvailable()
			if err != nil {
				return nil, false, err
			}
			if n >= c.frameLength {
				break
			}
			if stop.Wait(wakeReadPollSeconds) {
				return nil, false, nil
			}
			stalled += wakeReadPollSeconds
			if stalled >= wakeReadStallFallbackSeconds {
				pkgLog().Warn(fmt.Sprintf("wake word: read_available stuck below frame length for %.1fs — falling back to blocking reads", stalled))
				c.skipAvailablePoll = true
				break
			}
		}
	}
	data, _, err := c.stream.Read(c.frameLength)
	if err != nil {
		return nil, false, err
	}
	return data, true, nil
}

// Aborts first to unblock an in flight read on a dead device, then closes, ignoring failures
func (c *wakeAudioCapture) Close() {
	if c.stream == nil {
		return
	}
	_ = c.stream.Abort(true)
	_ = c.stream.Close(true)
}

// Goroutine standing in for the Python reader thread
type wakeThread struct {
	done chan struct{}
	gid  atomic.Uint64
}

func (t *wakeThread) alive() bool {
	select {
	case <-t.done:
		return false
	default:
		return true
	}
}

func (t *wakeThread) join(timeout float64) {
	timer := time.NewTimer(time.Duration(timeout * float64(time.Second)))
	defer timer.Stop()
	select {
	case <-t.done:
	case <-timer.C:
	}
}

func (t *wakeThread) isCurrent() bool { return t.gid.Load() == pythread.GoroutineID() }

// WakeDetectorOptions are the optional keywords of the detector constructor
type WakeDetectorOptions struct {
	Cooldown    *float64
	OnFailure   func(*WakeWordDetector)
	InputDevice any
}

// WakeWordDetector is the background hotword listener, the engine kept across pause and resume
type WakeWordDetector struct {
	Engine      WakeEngine
	cooldown    float64
	onFailure   func(*WakeWordDetector)
	inputDevice any

	mu                 sync.Mutex
	onWake             func() error
	inputDeviceDetails WakeInputDeviceDetails
	cap                wakeCapture

	lock             sync.Mutex
	thread           *wakeThread
	stop             *wakeStop
	callbackInflight pythread.Event
	lastFire         float64
	audioSilent      atomic.Bool
	silentFrames     int
	openCapture      func(frameLength int) (wakeCapture, error)
}

// NewWakeWordDetector builds an idle detector over an engine and the wake callback
func NewWakeWordDetector(engine WakeEngine, onWake func() error, opts WakeDetectorOptions) *WakeWordDetector {
	d := &WakeWordDetector{Engine: engine, onWake: onWake, cooldown: wakeFireCooldownSeconds, onFailure: opts.OnFailure,
		inputDevice: opts.InputDevice, stop: &wakeStop{}, inputDeviceDetails: WakeInputDeviceDetails{Selector: opts.InputDevice}}
	if opts.Cooldown != nil {
		d.cooldown = *opts.Cooldown
	}
	d.openCapture = d.openAudioCapture
	return d
}

// AudioSilent reports an open stream delivering only near silent frames
func (d *WakeWordDetector) AudioSilent() bool { return d.audioSilent.Load() }

// InputDeviceDetails returns a copy of the diagnostics of the device the detector opened
func (d *WakeWordDetector) InputDeviceDetails() WakeInputDeviceDetails {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.inputDeviceDetails
}

func (d *WakeWordDetector) setOnWake(fn func() error) {
	d.mu.Lock()
	d.onWake = fn
	d.mu.Unlock()
}

func (d *WakeWordDetector) currentCap() wakeCapture {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.cap
}

func (d *WakeWordDetector) setCap(c wakeCapture) {
	d.mu.Lock()
	d.cap = c
	d.mu.Unlock()
}

func (d *WakeWordDetector) runningLocked() bool { return d.thread != nil && d.thread.alive() }

// Running reports a live reader goroutine
func (d *WakeWordDetector) Running() bool {
	d.lock.Lock()
	defer d.lock.Unlock()
	return d.runningLocked()
}

// Opens the microphone and begins listening, a no op while running
func (d *WakeWordDetector) Start() error {
	d.lock.Lock()
	if d.runningLocked() {
		d.lock.Unlock()
		return nil
	}
	d.stop.Clear()
	ready := &pythread.Event{}
	startupErrors := &[]error{}
	t := &wakeThread{done: make(chan struct{})}
	d.thread = t
	go func() {
		t.gid.Store(pythread.GoroutineID())
		defer close(t.done)
		d.run(ready, startupErrors)
	}()
	d.lock.Unlock()
	if !ready.Wait(wakeStartTimeoutSeconds) {
		d.haltThread()
		return &WakeWordError{Class: "TimeoutError", Msg: "Timed out while opening the wake-word microphone."}
	}
	if len(*startupErrors) > 0 {
		d.haltThread()
		return &WakeWordError{Class: "RuntimeError", Msg: "Failed to open the wake-word microphone.", Cause: (*startupErrors)[0]}
	}
	return nil
}

// Pause releases the microphone and keeps the engine
func (d *WakeWordDetector) Pause() { d.haltThread() }

// Resume reopens the microphone with the kept engine
func (d *WakeWordDetector) Resume() error { return d.Start() }

// Stop halts the reader and tears the engine down
func (d *WakeWordDetector) Stop() {
	d.haltThread()
	d.Engine.Close()
}

// Signals the reader and joins it outside the lock, aborting a capture wedged past the join timeout
func (d *WakeWordDetector) haltThread() {
	d.lock.Lock()
	d.stop.Set()
	t := d.thread
	d.lock.Unlock()
	if t != nil && !t.isCurrent() {
		t.join(wakeHaltJoinSeconds)
		if t.alive() {
			if c := d.currentCap(); c != nil {
				c.Close()
			}
		}
	}
	d.lock.Lock()
	if d.thread == t && (t == nil || !t.alive()) {
		d.thread = nil
	}
	d.lock.Unlock()
}

// Runs the wake callback, logging its failure, then clears the in flight flag
func (d *WakeWordDetector) dispatchWake() {
	defer d.callbackInflight.Clear()
	d.mu.Lock()
	onWake := d.onWake
	d.mu.Unlock()
	if err := onWake(); err != nil {
		pkgLog().Warn(fmt.Sprintf("wake word callback failed: %s", err))
	}
}

// Opens the configured PortAudio input at its native rate
func (d *WakeWordDetector) openAudioCapture(frameLength int) (wakeCapture, error) {
	sd, err := importAudio()
	if err != nil {
		if isAudioImportError(err) || isAudioOSError(err) {
			pkgLog().Error(fmt.Sprintf("wake word: audio libraries unavailable: %s", err))
		}
		return nil, err
	}
	details := describeInputDevice(d.inputDevice, sd)
	d.mu.Lock()
	d.inputDeviceDetails = details
	d.mu.Unlock()
	c := &wakeAudioCapture{rate: WakeSampleRate, frameLength: 1280}
	if details.DefaultSamplerate != nil {
		if rate := *details.DefaultSamplerate; rate > 0 && !math.IsInf(rate, 0) {
			c.rate = int(math.RoundToEven(rate))
		}
	}
	c.frameLength = max(1, int(math.RoundToEven(float64(frameLength*c.rate)/WakeSampleRate)))
	name, hostapi, defaultRate := "system default", "unknown", "unknown"
	if details.Name != nil && *details.Name != "" {
		name = *details.Name
	}
	if details.HostAPI != nil && *details.HostAPI != "" {
		hostapi = *details.HostAPI
	}
	if details.DefaultSamplerate != nil && *details.DefaultSamplerate != 0 {
		defaultRate = pyrepr.Str(*details.DefaultSamplerate)
	}
	pkgLog().Info(fmt.Sprintf("wake word: opening microphone device=%s selector=%s hostapi=%s default_rate=%s capture_rate=%d engine_rate=%d",
		name, errchain.Show(d.inputDevice), hostapi, defaultRate, c.rate, WakeSampleRate))
	stream, err := sd.InputStream(pysounddevice.StreamParams{Device: d.inputDevice, Samplerate: float64(c.rate), Blocksize: c.frameLength, Channels: 1}, nil)
	if err == nil {
		c.stream = stream
		err = stream.Start()
	}
	if err != nil {
		c.Close()
		pkgLog().Error(fmt.Sprintf("wake word: failed to open microphone: %s", err))
		return nil, err
	}
	return c, nil
}

// Counts consecutive near zero frames and flags or clears the silent state, a missing frame never logging
func (d *WakeWordDetector) noteSilence(frame []int16, present bool, silentAlertFrames int) {
	peak := 0
	if present && len(frame) > 0 {
		m := int16(math.MinInt16)
		for _, v := range frame {
			a := v
			if a < 0 {
				a = -a
			}
			m = max(m, a)
		}
		peak = int(m)
	}
	if peak <= wakeSilencePeak {
		d.silentFrames++
		if d.silentFrames == silentAlertFrames {
			d.audioSilent.Store(true)
			if present {
				pkgLog().Warn(fmt.Sprintf("wake word: mic delivers only silence (peak<=%d for %ds); %s",
					wakeSilencePeak, int(wakeSilenceAlertSeconds), SilentAudioHint(d.InputDeviceDetails())))
			}
		}
	} else if d.silentFrames != 0 {
		if d.audioSilent.Load() {
			pkgLog().Info("wake word: mic audio detected — stream healthy")
		}
		d.silentFrames = 0
		d.audioSilent.Store(false)
	}
}

// Honors the cooldown then runs the wake callback on its own goroutine once
func (d *WakeWordDetector) fire() {
	now := pythread.Monotonic()
	if now-d.lastFire < d.cooldown {
		pkgLog().Debug("wake word: detection within cooldown — ignored")
		return
	}
	d.lastFire = now
	pkgLog().Info("wake word: phrase detected — firing callback")
	if !d.callbackInflight.IsSet() {
		d.callbackInflight.Set()
		go d.dispatchWake()
	}
}

// Reopen delays one arm may still spend
type wakeRetries struct {
	delays []float64
}

func (r *wakeRetries) next() (float64, bool) {
	if len(r.delays) == 0 {
		return 0, false
	}
	delay := r.delays[0]
	r.delays = r.delays[1:]
	return delay, true
}

// Reopens an already started capture within the arm's retry budget, keeping owner and engine
func (d *WakeWordDetector) recoverCapture(frameLength int, retries *wakeRetries) wakeCapture {
	for {
		delay, ok := retries.next()
		if !ok {
			break
		}
		pkgLog().Warn(fmt.Sprintf("wake word: reopening microphone in %.1fs", delay))
		if d.stop.Wait(delay) {
			return nil
		}
		c, err := d.openCapture(frameLength)
		if err != nil {
			pkgLog().Warn(fmt.Sprintf("wake word: microphone reopen failed: %s", err))
			continue
		}
		_ = d.Engine.Reset()
		d.audioSilent.Store(false)
		d.silentFrames = 0
		return c
	}
	pkgLog().Warn("wake word: microphone recovery exhausted; toggle wake word to retry")
	return nil
}

// Reader loop that scores frames, recovers a failing stream and reports an unexpected death
func (d *WakeWordDetector) run(ready *pythread.Event, startupErrors *[]error) {
	frameLength := d.Engine.FrameLength()
	c, err := d.openCapture(frameLength)
	if err != nil {
		*startupErrors = append(*startupErrors, err)
		ready.Set()
		return
	}
	d.setCap(c)
	_ = d.Engine.Reset()
	pkgLog().Info(fmt.Sprintf("wake word: listening (frame=%d, rate=%d)", frameLength, WakeSampleRate))
	ready.Set()
	failed := false
	retries := &wakeRetries{delays: []float64{0.5, 1.0, 2.0}}
	silentAlertFrames := max(1, int(wakeSilenceAlertSeconds*WakeSampleRate/float64(max(1, frameLength))))
	defer func() {
		if c != nil {
			c.Close()
		}
		pkgLog().Info("wake word: stream closed")
		if failed && d.onFailure != nil {
			d.onFailure(d)
		}
	}()
	for !d.stop.IsSet() {
		data, ok, err := c.Read(d.stop)
		if err != nil {
			pkgLog().Warn(fmt.Sprintf("wake word: stream read error: %s", err))
			c.Close()
			c = nil
			if !d.stop.IsSet() {
				c = d.recoverCapture(frameLength, retries)
			}
			if c != nil {
				d.setCap(c)
				continue
			}
			failed = !d.stop.IsSet()
			break
		}
		if !ok {
			d.noteSilence(nil, false, silentAlertFrames)
			continue
		}
		frame := data
		if c.Rate() != WakeSampleRate {
			frame = resampleAudioFrame(frame, frameLength)
		}
		d.noteSilence(frame, true, silentAlertFrames)
		fired, err := d.Engine.Process(frame)
		if err != nil {
			pkgLog().Debug(fmt.Sprintf("wake word: engine error: %s", err))
			continue
		}
		if fired {
			d.fire()
		}
	}
}
