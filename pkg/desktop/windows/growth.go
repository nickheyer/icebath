package windows

import (
	"os"
	"regexp"
	"sync"
)

// PlacedWindow reports where a window sits and whether it is maximized.
type PlacedWindow interface {
	NormalBounds() Rect
	IsMaximized() bool
}

// SizableWindow is the main window the first run intro resizes.
type SizableWindow interface {
	PlacedWindow
	Bounds() Rect
	SetBounds(Rect)
	IsDestroyed() bool
	IsFullScreen() bool
}

// Screen answers the work area of the display that best matches bounds.
type Screen interface {
	WorkAreaOf(bounds Rect) Rect
}

// Sizing remembers app chosen window bounds and the bounds a window had before the intro.
type Sizing struct {
	mu               sync.Mutex
	screen           Screen
	appSized         map[PlacedWindow]Rect
	beforeOnboarding map[SizableWindow]Rect
}

// NewSizing creates the sizing registry over screen
func NewSizing(screen Screen) *Sizing {
	return &Sizing{screen: screen, appSized: map[PlacedWindow]Rect{}, beforeOnboarding: map[SizableWindow]Rect{}}
}

// MarkAppSized records bounds the app gave win so geometry persistence skips them
func (s *Sizing) MarkAppSized(win PlacedWindow, bounds Rect) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.appSized[win] = bounds
}

// IsAppSized reports whether win still sits within one pixel of the bounds the app gave it
func (s *Sizing) IsAppSized(win PlacedWindow) bool {
	s.mu.Lock()
	given, ok := s.appSized[win]
	s.mu.Unlock()
	if !ok || win.IsMaximized() {
		return false
	}
	b := win.NormalBounds()
	near := func(a, c int) bool { return max(a-c, c-a) <= 1 }
	return near(b.X, given.X) && near(b.Y, given.Y) && near(b.Width, given.Width) && near(b.Height, given.Height)
}

func centeredBounds(workArea Rect, width, height int) Rect {
	return Rect{
		X:      jsRound(float64(workArea.X) + float64(workArea.Width-width)/2),
		Y:      jsRound(float64(workArea.Y) + float64(workArea.Height-height)/2),
		Width:  width,
		Height: height,
	}
}

func sizedBounds(mode SizeMode, bounds, workArea Rect) *Rect {
	target := WindowSize(mode, workArea)
	if mode == SizeOnboarding {
		r := centeredBounds(workArea, target.Width, target.Height)
		return &r
	}
	w, h := max(bounds.Width, target.Width), max(bounds.Height, target.Height)
	if w == bounds.Width && h == bounds.Height {
		return nil
	}
	r := centeredBounds(workArea, w, h)
	return &r
}

// HandleSizeRequest grows the main window for the intro and puts it back after, ignoring other senders
func (s *Sizing) HandleSizeRequest(main, sender SizableWindow, mode string) {
	if main == nil || main.IsDestroyed() || sender != main {
		return
	}
	if mode != string(SizeNormal) && mode != string(SizeOnboarding) {
		return
	}
	s.mu.Lock()
	before, had := s.beforeOnboarding[main]
	delete(s.beforeOnboarding, main)
	s.mu.Unlock()
	if main.IsMaximized() || main.IsFullScreen() {
		return
	}
	bounds := main.Bounds()
	if mode == string(SizeOnboarding) {
		s.mu.Lock()
		s.beforeOnboarding[main] = bounds
		s.mu.Unlock()
	} else if had {
		main.SetBounds(before)
		return
	}
	if next := sizedBounds(SizeMode(mode), bounds, s.screen.WorkAreaOf(bounds)); next != nil {
		s.MarkAppSized(main, *next)
		main.SetBounds(*next)
	}
}

// Forget drops what the registry holds for a closed window
func (s *Sizing) Forget(win SizableWindow) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.appSized, win)
	delete(s.beforeOnboarding, win)
}

// ControlAction is a renderer drawn window control.
type ControlAction string

const (
	ControlClose          ControlAction = "close"
	ControlMinimize       ControlAction = "minimize"
	ControlToggleMaximize ControlAction = "toggle-maximize"
)

var wslKernel = regexp.MustCompile(`(?i)microsoft|wsl`)

// CustomWindowControlsEnabled is true on Linux under WSL, by environment or kernel release
func CustomWindowControlsEnabled(platform string, env func(string) string, kernelRelease string) bool {
	if platform != "linux" {
		return false
	}
	if env("WSL_DISTRO_NAME") != "" || env("WSL_INTEROP") != "" {
		return true
	}
	return kernelRelease != "" && wslKernel.MatchString(kernelRelease)
}

// KernelRelease reads the Linux kernel release string or returns empty
func KernelRelease() string {
	raw, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil {
		return ""
	}
	return string(raw)
}

// ControllableWindow is a window the renderer controls can act on.
type ControllableWindow interface {
	IsDestroyed() bool
	IsMaximized() bool
	Minimize()
	Maximize()
	Unmaximize()
	Close()
	Focus()
}

// PerformWindowControl applies action to win and reports whether it was handled
func PerformWindowControl(win ControllableWindow, action string) bool {
	if win == nil || win.IsDestroyed() {
		return false
	}
	switch ControlAction(action) {
	case ControlMinimize:
		win.Minimize()
		return true
	case ControlToggleMaximize:
		if win.IsMaximized() {
			win.Unmaximize()
		} else {
			win.Maximize()
		}
		win.Focus()
		return true
	case ControlClose:
		win.Close()
		return true
	}
	return false
}

// ControlState is what the renderer reads about its window controls.
type ControlState struct {
	CustomWindowControls bool `json:"customWindowControls"`
	IsMaximized          bool `json:"isMaximized"`
}

// WindowControlState reports custom controls and the live maximized flag of win
func WindowControlState(win ControllableWindow, custom bool) ControlState {
	return ControlState{CustomWindowControls: custom, IsMaximized: win != nil && !win.IsDestroyed() && win.IsMaximized()}
}

// ChromeEventSource is a window emitting chrome lifecycle events.
type ChromeEventSource interface {
	On(event string, listener func())
}

// ChromeFullscreenEnter, ChromeFullscreenLeave and ChromeStateEvents name the events that change window chrome.
var (
	ChromeFullscreenEnter = []string{"will-enter-full-screen", "enter-full-screen"}
	ChromeFullscreenLeave = []string{"will-leave-full-screen", "leave-full-screen"}
	ChromeStateEvents     = []string{"maximize", "unmaximize", "minimize", "restore", "hide", "show"}
)

// BindWindowChromeEvents publishes each chrome event of win with its fullscreen transition or nil
func BindWindowChromeEvents[T ChromeEventSource](win T, publish func(fullscreen *bool, target T)) {
	yes, no := true, false
	for _, e := range ChromeFullscreenEnter {
		win.On(e, func() { publish(&yes, win) })
	}
	for _, e := range ChromeFullscreenLeave {
		win.On(e, func() { publish(&no, win) })
	}
	for _, e := range ChromeStateEvents {
		win.On(e, func() { publish(nil, win) })
	}
}
