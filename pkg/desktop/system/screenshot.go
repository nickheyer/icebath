package system

import (
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"
)

const screenshotRequestTTL = 5 * time.Second

// ScreenshotStatus is what the screenshot settings and status event carry
type ScreenshotStatus struct {
	Enabled bool   `json:"enabled"`
	State   string `json:"state"`
}

// ScreenshotWindow is the frontmost window a double command press captures
type ScreenshotWindow struct {
	WindowID uint32  `json:"windowId"`
	Width    float64 `json:"width"`
	Height   float64 `json:"height"`
}

// ScreenshotResult is the capture reply, PNG bytes or the reason there are none
type ScreenshotResult struct {
	OK     bool   `json:"ok"`
	PNG    []byte `json:"png,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// ScreenshotNative is the macOS screen capture surface
type ScreenshotNative interface {
	// Screen Recording granted, the getMediaAccessStatus screen check
	HasScreenPermission() bool
	// Raises the Screen Recording prompt
	RequestScreenPermission()
	// PNG of one window scaled to the thumbnail size, nil when it cannot be captured
	CaptureWindow(windowID uint32, width, height int) ([]byte, error)
	OpenExternal(url string) error
}

var errUntrustedScreenshotFrame = errors.New("Screenshot request from an untrusted frame")

type screenshotPending struct {
	id      string
	owner   string
	window  ScreenshotWindow
	expires time.Time
}

type screenshots struct {
	mu            sync.Mutex
	native        ScreenshotNative
	monitor       *GestureMonitor[ScreenshotWindow]
	configPath    string
	recipients    map[string]bool
	lastRecipient string
	enabled       bool
	monitorState  string
	disposed      bool
	generation    int
	pending       *screenshotPending
	captureGen    int
	busy          bool
}

// Capture gesture of a helper line, nil for any malformed window
func parseScreenshotCapture(message map[string]any) *ScreenshotWindow {
	if message["type"] != "capture" {
		return nil
	}
	id, ok1 := message["windowId"].(float64)
	width, ok2 := message["width"].(float64)
	height, ok3 := message["height"].(float64)
	if !ok1 || id != math.Trunc(id) || id <= 0 || id > 0xffffffff || !ok2 || math.IsInf(width, 0) || width <= 0 || !ok3 || math.IsInf(height, 0) || height <= 0 {
		return nil
	}
	return &ScreenshotWindow{WindowID: uint32(id), Width: width, Height: height}
}

// Screenshot state, the persisted switch read only on macOS where the feature is installed
func newScreenshots(platform, userData, helper string, native ScreenshotNative) *screenshots {
	sc := &screenshots{native: native, configPath: filepath.Join(userData, "screenshot.json"), recipients: map[string]bool{}, monitorState: "disabled"}
	sc.monitor = NewGestureMonitor(GestureMonitorOptions[ScreenshotWindow]{Path: helper, ParseGesture: parseScreenshotCapture})
	if platform != "darwin" {
		return sc
	}
	if body, err := os.ReadFile(sc.configPath); err == nil {
		var saved map[string]any
		if json.Unmarshal(body, &saved) == nil {
			sc.enabled = saved["enabled"] == true
		}
	}
	return sc
}

func (sc *screenshots) statusLocked() ScreenshotStatus {
	state := sc.monitorState
	switch {
	case !sc.enabled:
		state = "disabled"
	case sc.monitorState == "ready" && !sc.native.HasScreenPermission():
		state = "screen-permission"
	}
	return ScreenshotStatus{Enabled: sc.enabled, State: state}
}

func (s *Service) publishScreenshotStatus() {
	sc := s.shots
	sc.mu.Lock()
	status := sc.statusLocked()
	sc.mu.Unlock()
	s.o.Host.Broadcast("screenshot:status", status)
}

// Requests a capture id for a gesture, nil while one is pending or capturing
func (sc *screenshots) requestLocked(owner string, window ScreenshotWindow) string {
	if sc.busy || (sc.pending != nil && sc.pending.expires.After(time.Now())) {
		return ""
	}
	id := uuid.NewString()
	sc.pending = &screenshotPending{id: id, owner: owner, window: window, expires: time.Now().Add(screenshotRequestTTL)}
	return id
}

func (sc *screenshots) clearLocked() {
	sc.captureGen++
	sc.pending = nil
}

// Starts the gesture helper for the current generation, routing captures to the focused or last subscribed window
func (s *Service) startScreenshotMonitor(requestPermission bool) {
	sc := s.shots
	sc.mu.Lock()
	sc.generation++
	current := sc.generation
	sc.mu.Unlock()
	sc.monitor.Start(func(window ScreenshotWindow) {
		sc.mu.Lock()
		if sc.disposed || !sc.enabled || current != sc.generation {
			sc.mu.Unlock()
			return
		}
		recipient := sc.lastRecipient
		if focused := s.o.Host.FocusedWindow(); focused != "" && sc.recipients[focused] {
			recipient = focused
		}
		if recipient == "" || !sc.recipients[recipient] || s.o.Host.Window(recipient) == nil {
			sc.mu.Unlock()
			return
		}
		id := sc.requestLocked(recipient, window)
		sc.mu.Unlock()
		if id != "" {
			if w := s.o.Host.Window(recipient); w != nil {
				w.Send("screenshot:request", id)
			}
		}
	}, func(status GestureStatus) {
		sc.mu.Lock()
		if sc.disposed || current != sc.generation {
			sc.mu.Unlock()
			return
		}
		switch {
		case status.Type == "error" && status.Code == "permission-required":
			sc.monitorState = "input-permission"
		case status.Type == "error":
			sc.monitorState = "unavailable"
		case status.Type == "stopped":
			sc.monitorState = "disabled"
		default:
			sc.monitorState = status.Type
		}
		sc.mu.Unlock()
		s.publishScreenshotStatus()
	}, requestPermission)
}

// Screenshot settings of a renderer window, the screenshot.getSettings invoke
func (s *Service) ScreenshotGetSettings(caller string) (ScreenshotStatus, error) {
	if err := s.screenshotHandler(caller, "settings:get"); err != nil {
		return ScreenshotStatus{}, err
	}
	s.shots.mu.Lock()
	defer s.shots.mu.Unlock()
	return s.shots.statusLocked(), nil
}

// Refuses an invoke from a frame that is no renderer window, and every invoke off macOS where nothing is registered
func (s *Service) screenshotHandler(caller, name string) error {
	if s.o.Platform != "darwin" {
		return errors.New("No handler registered for 'hermes:screenshot:" + name + "'")
	}
	if caller == "" || !s.o.Host.IsRendererWindow(caller) {
		return errUntrustedScreenshotFrame
	}
	return nil
}

// Persists the screenshot switch and restarts the helper, prompting for Screen Recording when it is missing
func (s *Service) ScreenshotSetEnabled(caller string, value any) (ScreenshotStatus, error) {
	if err := s.screenshotHandler(caller, "settings:set"); err != nil {
		return ScreenshotStatus{}, err
	}
	on, ok := value.(bool)
	if !ok {
		return ScreenshotStatus{}, errors.New("Screenshot setting must be a boolean")
	}
	sc := s.shots
	if err := os.MkdirAll(filepath.Dir(sc.configPath), 0o777); err != nil {
		return ScreenshotStatus{}, err
	}
	body, _ := json.Marshal(map[string]bool{"enabled": on})
	if err := os.WriteFile(sc.configPath+".tmp", body, 0o600); err != nil {
		return ScreenshotStatus{}, err
	}
	if err := os.Rename(sc.configPath+".tmp", sc.configPath); err != nil {
		return ScreenshotStatus{}, err
	}
	sc.mu.Lock()
	sc.enabled = on
	sc.generation++
	sc.clearLocked()
	sc.mu.Unlock()
	sc.monitor.Stop()
	if on {
		s.startScreenshotMonitor(true)
		if !sc.native.HasScreenPermission() {
			sc.native.RequestScreenPermission()
		}
	} else {
		sc.mu.Lock()
		sc.monitorState = "disabled"
		sc.mu.Unlock()
	}
	s.publishScreenshotStatus()
	sc.mu.Lock()
	defer sc.mu.Unlock()
	return sc.statusLocked(), nil
}

// Opens the Input Monitoring or Screen Recording privacy pane
func (s *Service) ScreenshotOpenPermissionSettings(caller string, kind any) error {
	if err := s.screenshotHandler(caller, "permission"); err != nil {
		return err
	}
	pane := ""
	switch kind {
	case "input":
		pane = "Privacy_ListenEvent"
	case "screen":
		pane = "Privacy_ScreenCapture"
	default:
		return errors.New("Unknown screenshot permission")
	}
	return s.shots.native.OpenExternal("x-apple.systempreferences:com.apple.preference.security?" + pane)
}

// Captures the window a pending request names for the window that owns it
func (s *Service) ScreenshotCapture(caller string, requestID any) (ScreenshotResult, error) {
	if err := s.screenshotHandler(caller, "capture"); err != nil {
		return ScreenshotResult{}, err
	}
	result := s.takeScreenshot(caller, requestID)
	if !result.OK && result.Reason == "screen-permission" {
		s.publishScreenshotStatus()
	}
	return result, nil
}

func (s *Service) takeScreenshot(owner string, id any) ScreenshotResult {
	sc := s.shots
	sc.mu.Lock()
	if sc.pending == nil || sc.pending.id != id || sc.pending.owner != owner {
		sc.mu.Unlock()
		return ScreenshotResult{Reason: "expired"}
	}
	if !sc.pending.expires.After(time.Now()) {
		sc.pending = nil
		sc.mu.Unlock()
		return ScreenshotResult{Reason: "expired"}
	}
	window := sc.pending.window
	sc.pending = nil
	sc.mu.Unlock()
	if !sc.native.HasScreenPermission() {
		return ScreenshotResult{Reason: "screen-permission"}
	}
	sc.mu.Lock()
	captured := sc.captureGen
	sc.busy = true
	sc.mu.Unlock()
	defer func() {
		sc.mu.Lock()
		sc.busy = false
		sc.mu.Unlock()
	}()
	scale := math.Min(2, 4096/math.Max(window.Width, window.Height))
	png, err := sc.native.CaptureWindow(window.WindowID, int(math.Ceil(window.Width*scale)), int(math.Ceil(window.Height*scale)))
	if err != nil {
		if sc.native.HasScreenPermission() {
			return ScreenshotResult{Reason: "unavailable"}
		}
		return ScreenshotResult{Reason: "screen-permission"}
	}
	sc.mu.Lock()
	stale := captured != sc.captureGen
	sc.mu.Unlock()
	if stale {
		return ScreenshotResult{Reason: "expired"}
	}
	if len(png) == 0 {
		return ScreenshotResult{Reason: "unavailable"}
	}
	return ScreenshotResult{OK: true, PNG: png}
}

// Adds or removes a renderer window from the capture recipients, the screenshot subscribe send
func (s *Service) ScreenshotSubscribe(caller string, subscribed any) {
	if s.o.Platform != "darwin" || caller == "" || !s.o.Host.IsRendererWindow(caller) {
		return
	}
	sc := s.shots
	sc.mu.Lock()
	defer sc.mu.Unlock()
	switch subscribed {
	case true:
		sc.recipients[caller] = true
		if sc.lastRecipient == "" || s.o.Host.FocusedWindow() == caller {
			sc.lastRecipient = caller
		}
	case false:
		delete(sc.recipients, caller)
		if sc.lastRecipient == caller {
			sc.lastRecipient = ""
		}
	}
}

// Makes a focused subscribed window the capture recipient
func (s *Service) ScreenshotWindowFocused(name string) {
	sc := s.shots
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if sc.recipients[name] {
		sc.lastRecipient = name
	}
}

// Forgets a destroyed window as a recipient
func (s *Service) screenshotWindowGone(name string) {
	sc := s.shots
	sc.mu.Lock()
	defer sc.mu.Unlock()
	delete(sc.recipients, name)
	if sc.lastRecipient == name {
		sc.lastRecipient = ""
	}
}

// Stops the helper and drops every recipient at quit
func (s *Service) disposeScreenshots() {
	sc := s.shots
	sc.mu.Lock()
	sc.disposed = true
	sc.clearLocked()
	sc.recipients = map[string]bool{}
	sc.lastRecipient = ""
	sc.mu.Unlock()
	sc.monitor.Stop()
}

// Starts the helper at launch when the switch was left on
func (s *Service) startScreenshots() {
	if s.o.Platform != "darwin" {
		return
	}
	s.shots.mu.Lock()
	on := s.shots.enabled
	s.shots.mu.Unlock()
	if on {
		s.startScreenshotMonitor(false)
	}
}
