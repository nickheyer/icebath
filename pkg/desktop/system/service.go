package system

import (
	"path/filepath"
	"time"

	"github.com/nickheyer/icebath/pkg/desktop/dedupe"
)

// Host is the shell surface the system handlers push events through and read windows and the clipboard from
type Host interface {
	// Sends one event to the main chat window, false when it is gone
	SendMain(event string, payload any) bool
	// Sends one event to every live window
	Broadcast(event string, payload any)
	// Live window of a name, nil when it is gone
	Window(name string) NotifyWindow
	// Main chat window, nil when it is gone
	MainWindow() NotifyWindow
	// Name of the focused window, empty when none of the app has focus
	FocusedWindow() string
	// Whether the named window hosts the renderer itself rather than a guest page
	IsRendererWindow(name string) bool
	WriteClipboardText(text string)
	ReadClipboardText() string
	// PNG bytes of the clipboard image, nil without one
	ReadClipboardPNG() []byte
}

// ThrottleUpdater is the stream throttle that runs chat windows unthrottled while a turn is in flight
type ThrottleUpdater interface {
	Update(busy bool)
}

// Options name the platform, the user data directory, the log, the shell and the native facilities the service drives
type Options struct {
	Platform string
	IsWSL    bool
	UserData string
	Log      func(line string)
	Host     Host
	Throttle ThrottleUpdater
	Blocker  PowerSaveBlocker
	Notifier Notifier
	Power    PowerMonitor
	// Screen capture natives and the gesture helper binary, set on macOS only
	Screenshot       ScreenshotNative
	ScreenshotHelper string
	// Runs tick every interval on the main thread until stop, the lag watchdog clock
	SetInterval func(tick func(), every time.Duration) (stop func())
}

// Service holds active work, keep awake, notifications, power state, screenshots and the startup claims behind the system preload functions
type Service struct {
	o         Options
	work      *ActiveWorkTracker
	keepAwake *KeepAwakeController
	notes     *notifications
	power     powerState
	latency   startupLatencyClaim
	ambient   dedupe.Arbiter
	shots     *screenshots
	lag       *LagWatchdog
}

// System service over the shell, keep awake persisted in userData keep-awake.json and screenshots in screenshot.json
func NewService(o Options) *Service {
	s := &Service{o: o, work: NewActiveWorkTracker(), notes: newNotifications(), latency: startupLatencyClaim{uptime: processUptime}, ambient: newAmbientArbiter()}
	s.keepAwake = NewKeepAwakeController(KeepAwakeControllerOptions{
		Blocker:    o.Blocker,
		ConfigPath: filepath.Join(o.UserData, "keep-awake.json"),
		IsWorking:  s.work.AnyTurnInFlight,
		Log:        o.Log,
		Offer:      func() bool { return o.Host.SendMain("keep-awake-offer", nil) },
	})
	s.shots = newScreenshots(o.Platform, o.UserData, o.ScreenshotHelper, o.Screenshot)
	s.lag = NewLagWatchdog(LagWatchdogOptions{Cadence: time.Second, Threshold: 2 * time.Second, Now: time.Now, Log: o.Log, SetInterval: o.SetInterval})
	return s
}

// Registers the power listeners, restores keep awake, starts the lag watchdog and the screenshot helper once the app is ready
func (s *Service) Start() {
	s.registerPowerListeners()
	s.keepAwake.Restore()
	s.lag.Start()
	s.startScreenshots()
}

// Stops the lag watchdog and the screenshot helper and closes the notification connection at quit
func (s *Service) Close() {
	s.lag.Stop()
	s.disposeScreenshots()
	if d, ok := s.o.Notifier.(interface{ Dispose() }); ok {
		d.Dispose()
	}
}

// Drives stream throttling and the while working keep awake mode from the merged active work
func (s *Service) updateFromActiveWork() {
	working := s.work.AnyTurnInFlight()
	s.o.Throttle.Update(working)
	s.keepAwake.Apply(working)
}

// Records the turns a renderer window has in flight, the setActiveWork send
func (s *Service) SetActiveWork(windowID string, payload any) {
	s.work.Set(windowID, NormalizeActiveWork(payload))
	s.updateFromActiveWork()
}

// Drops the active work of a window whose renderer process went away
func (s *Service) RendererGone(windowID string) {
	if s.work.Forget(windowID) {
		s.updateFromActiveWork()
	}
}

// Drops the active work and screenshot subscription of a destroyed window
func (s *Service) WindowDestroyed(windowID string) {
	s.RendererGone(windowID)
	s.screenshotWindowGone(windowID)
}

// Live window reports merged, what the last chat window close guard reads
func (s *Service) ActiveWork() ActiveWork {
	return s.work.Merged()
}

// Live reports plus the last summary any renderer sent, what the quit guard reads
func (s *Service) ActiveWorkForQuit() ActiveWork {
	return s.work.MergedWithLastSeen()
}

// Switches the keep awake mode, the setKeepAwake send
func (s *Service) SetKeepAwake(value any) {
	s.keepAwake.SetMode(value)
}
