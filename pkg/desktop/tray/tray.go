package tray

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
)

// Status is the minimize-to-tray:get answer and the minimize-to-tray:changed payload.
type Status struct {
	Enabled   bool `json:"enabled"`
	Available bool `json:"available"`
}

// Window is a window the tray can hide and bring back.
type Window interface {
	IsDestroyed() bool
	IsMinimized() bool
	IsVisible() bool
	Hide()
	Show()
	ShowInactive()
	Focus()
	Blur()
	Restore()
	SetSkipTaskbar(skip bool)
	Send(event string, payload any)
	On(event string, listener func())
}

// Icon is a created tray icon.
type Icon interface{ Destroy() }

// Host creates the native tray icon and controls the dock.
type Host interface {
	CreateIcon(restore func(), clickRestores bool) (Icon, error)
	DockShow()
	DockHide()
	Windows() []Window
	Defer(f func())
}

// Watch starts watching the Linux tray host and returns a stop function, calling lost when the host goes away.
type Watch func(lost func()) (stop func(), err error)

// Options configures minimize to tray.
type Options struct {
	PreferencesPath      string
	Platform             string
	Host                 Host
	WatchHost            Watch
	RestoreMainWindow    func()
	IsQuittingForHandoff func() bool
	Log                  func(string)
}

// Tray hides windows into a system tray icon instead of minimizing or closing them.
type Tray struct {
	o              Options
	mu             sync.Mutex
	op             sync.Mutex
	enabled        bool
	quitting       bool
	icon           Icon
	stopWatching   func()
	dockHidden     bool
	hostGeneration int
	windows        map[Window]bool
	hidden         map[Window]bool
}

// New creates the tray controller
func New(o Options) *Tray {
	return &Tray{o: o, windows: map[Window]bool{}, hidden: map[Window]bool{}}
}

// Status reports whether minimize to tray is enabled and the icon exists
func (t *Tray) Status() Status {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.statusLocked()
}

func (t *Tray) statusLocked() Status { return Status{Enabled: t.enabled, Available: t.icon != nil} }

func (t *Tray) broadcast() {
	st := t.Status()
	for _, w := range t.o.Host.Windows() {
		if !w.IsDestroyed() {
			w.Send("minimize-to-tray:changed", st)
		}
	}
}

func (t *Tray) showDockLocked() {
	if t.dockHidden {
		t.dockHidden = false
		t.o.Host.DockShow()
	}
}

func (t *Tray) syncDock() {
	if t.o.Platform != "darwin" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	foreground := false
	for w := range t.windows {
		if !w.IsDestroyed() && w.IsVisible() && !w.IsMinimized() {
			foreground = true
		}
	}
	if t.icon != nil && len(t.hidden) > 0 && !foreground && !t.quitting {
		t.dockHidden = true
		t.o.Host.DockHide()
	} else {
		t.showDockLocked()
	}
}

func (t *Tray) releasedLocked(w Window) bool {
	if !t.hidden[w] {
		return false
	}
	delete(t.hidden, w)
	if t.o.Platform == "win32" {
		w.SetSkipTaskbar(false)
	}
	t.showDockLocked()
	return true
}

func (t *Tray) restoreHidden() {
	t.mu.Lock()
	t.showDockLocked()
	var restore []Window
	for w := range t.hidden {
		if w.IsDestroyed() {
			delete(t.hidden, w)
			continue
		}
		t.releasedLocked(w)
		restore = append(restore, w)
	}
	t.mu.Unlock()
	for _, w := range restore {
		if w.IsMinimized() {
			w.Restore()
		}
		if t.o.Platform == "win32" {
			w.Show()
			w.Focus()
		} else {
			w.ShowInactive()
		}
	}
}

// Restore brings every hidden window back and restores the main window
func (t *Tray) Restore() {
	t.restoreHidden()
	t.o.RestoreMainWindow()
}

func (t *Tray) destroyTray(force bool) {
	t.mu.Lock()
	stop := t.stopWatching
	t.stopWatching = nil
	if t.o.Platform == "linux" && !t.quitting && !force {
		t.mu.Unlock()
		if stop != nil {
			stop()
		}
		return
	}
	icon := t.icon
	t.icon = nil
	t.mu.Unlock()
	if stop != nil {
		stop()
	}
	if icon != nil {
		icon.Destroy()
	}
}

func (t *Tray) hostLost() {
	t.mu.Lock()
	t.hostGeneration++
	t.mu.Unlock()
	t.restoreHidden()
	t.destroyTray(true)
	t.broadcast()
}

func (t *Tray) watch() error {
	t.mu.Lock()
	generation := t.hostGeneration
	t.mu.Unlock()
	stop, err := t.o.WatchHost(t.hostLost)
	if err != nil {
		return err
	}
	t.mu.Lock()
	t.stopWatching = stop
	changed := generation != t.hostGeneration
	t.mu.Unlock()
	if changed {
		return errors.New("System tray host disappeared")
	}
	return nil
}

func (t *Tray) apply(on bool) Status {
	t.mu.Lock()
	t.enabled = on
	available, quitting, watching := t.icon != nil, t.quitting, t.stopWatching != nil
	t.mu.Unlock()
	switch {
	case !on:
		t.restoreHidden()
		t.destroyTray(false)
	case !quitting && available && t.o.Platform == "linux" && !watching:
		if err := t.watch(); err != nil {
			t.restoreHidden()
			t.destroyTray(true)
			t.o.Log("[tray] unavailable; ordinary window behavior retained: " + err.Error())
		}
	case !available && !quitting:
		if err := t.create(); err != nil {
			t.restoreHidden()
			t.destroyTray(false)
			t.o.Log("[tray] unavailable; ordinary window behavior retained: " + err.Error())
		}
	}
	t.broadcast()
	return t.Status()
}

func (t *Tray) create() error {
	if t.o.Platform == "linux" {
		if err := t.watch(); err != nil {
			return err
		}
	}
	t.mu.Lock()
	quitting := t.quitting
	t.mu.Unlock()
	if quitting {
		t.destroyTray(false)
		return nil
	}
	icon, err := t.o.Host.CreateIcon(t.Restore, t.o.Platform != "darwin")
	if err != nil {
		return err
	}
	t.mu.Lock()
	t.icon = icon
	t.mu.Unlock()
	return nil
}

// RegisterWindow hides win into the tray on minimize, and on close when closeToTray is set
func (t *Tray) RegisterWindow(win Window, closeToTray bool) (onClose func() (cancel bool)) {
	t.mu.Lock()
	t.windows[win] = true
	t.mu.Unlock()
	hide := func() bool {
		if win.IsDestroyed() {
			return false
		}
		t.mu.Lock()
		if !t.enabled || t.icon == nil || t.quitting || t.o.IsQuittingForHandoff() {
			t.mu.Unlock()
			return false
		}
		t.hidden[win] = true
		t.mu.Unlock()
		if t.o.Platform == "win32" {
			win.SetSkipTaskbar(true)
			win.Blur()
		}
		win.Hide()
		t.syncDock()
		return true
	}
	win.On("minimize", func() {
		t.o.Host.Defer(func() {
			if !win.IsDestroyed() && !win.IsMinimized() && win.IsVisible() {
				return
			}
			hide()
		})
	})
	release := func() {
		t.mu.Lock()
		released := t.releasedLocked(win)
		t.mu.Unlock()
		if released && t.o.Platform == "win32" {
			t.o.Host.Defer(func() {
				if !win.IsDestroyed() && !win.IsMinimized() && win.IsVisible() {
					win.Show()
					win.Focus()
				}
			})
		}
		t.syncDock()
	}
	win.On("show", release)
	win.On("restore", release)
	if !closeToTray {
		return func() bool { return false }
	}
	return hide
}

// Closed forgets a destroyed window
func (t *Tray) Closed(win Window) {
	t.mu.Lock()
	delete(t.windows, win)
	delete(t.hidden, win)
	t.mu.Unlock()
	t.syncDock()
}

// SessionEnding marks the app quitting when the OS ends the session
func (t *Tray) SessionEnding() {
	t.mu.Lock()
	t.quitting = true
	t.mu.Unlock()
}

// Start applies the persisted preference
func (t *Tray) Start() Status {
	on := false
	if raw, err := os.ReadFile(t.o.PreferencesPath); err == nil {
		var v map[string]any
		if json.Unmarshal(raw, &v) == nil {
			on = v["enabled"] == true
		}
	}
	t.op.Lock()
	defer t.op.Unlock()
	return t.apply(on)
}

// SetEnabled persists the preference and applies it after any operation in flight
func (t *Tray) SetEnabled(on bool) (Status, error) {
	t.op.Lock()
	defer t.op.Unlock()
	if err := os.MkdirAll(filepath.Dir(t.o.PreferencesPath), 0o755); err != nil {
		return Status{}, err
	}
	body, _ := json.Marshal(map[string]bool{"enabled": on})
	if err := os.WriteFile(t.o.PreferencesPath+".tmp", body, 0o644); err != nil {
		return Status{}, err
	}
	if err := os.Rename(t.o.PreferencesPath+".tmp", t.o.PreferencesPath); err != nil {
		return Status{}, err
	}
	return t.apply(on), nil
}

// WillQuit marks quitting and removes the icon
func (t *Tray) WillQuit() {
	t.mu.Lock()
	t.quitting = true
	t.mu.Unlock()
	t.destroyTray(false)
}

// BeginQuit marks quitting so windows close instead of hiding
func (t *Tray) BeginQuit() {
	t.mu.Lock()
	t.quitting = true
	t.mu.Unlock()
}
