package main

import (
	_ "embed"
	"path/filepath"
	"runtime"

	"github.com/nickheyer/icebath/pkg/desktop/tray"
	"github.com/wailsapp/wails/v3/pkg/application"
)

//go:embed assets/icon.png
var trayIconPNG []byte

// trayWindow adds the tray hooks to a shell window.
type trayWindow struct{ *shellWindow }

func (t trayWindow) Hide()                 { t.w.Hide() }
func (t trayWindow) Blur()                 { nativeBlur(t.w) }
func (t trayWindow) SetSkipTaskbar(b bool) { nativeSetSkipTaskbar(t.w, b) }

// trayHost creates the Wails system tray icon and drives the macOS dock.
type trayHost struct{ shell *Shell }

type trayIcon struct{ t *application.SystemTray }

func (i trayIcon) Destroy() { i.t.Destroy() }

func (h *trayHost) CreateIcon(restore func(), clickRestores bool) (tray.Icon, error) {
	t := h.shell.app.SystemTray.New()
	t.SetIcon(trayIconPNG)
	t.SetTooltip(appName)
	menu := application.NewMenu()
	menu.Add("Show " + appName).OnClick(func(*application.Context) { restore() })
	menu.AddSeparator()
	menu.Add("Quit " + appName).OnClick(func(*application.Context) { h.shell.quit() })
	t.SetMenu(menu)
	if clickRestores {
		t.OnClick(restore)
	}
	t.OnDoubleClick(restore)
	return trayIcon{t}, nil
}

func (h *trayHost) DockShow() { nativeDockShow() }
func (h *trayHost) DockHide() { nativeDockHide() }

func (h *trayHost) Windows() []tray.Window {
	var out []tray.Window
	for _, w := range h.shell.windowsList() {
		out = append(out, trayWindow{w.(*shellWindow)})
	}
	return out
}

func (h *trayHost) Defer(f func()) { application.InvokeAsync(f) }

func (s *Shell) startTray() {
	watch := func(lost func()) (func(), error) { return func() {}, nil }
	if runtime.GOOS == "linux" {
		watch = tray.WatchLinuxTrayHost
	}
	s.tray = tray.New(tray.Options{
		PreferencesPath:      filepath.Join(s.userData, "minimize-to-tray.json"),
		Platform:             s.platform.OS,
		Host:                 &trayHost{shell: s},
		WatchHost:            watch,
		RestoreMainWindow:    s.restoreMainWindow,
		IsQuittingForHandoff: s.handoffQuit.Load,
		Log:                  s.log,
	})
}

func (s *Shell) restoreMainWindow() {
	if w := s.mainWindow(); w != nil {
		w.Activate()
		return
	}
	s.createMainWindow()
}

func (s *Shell) quit() {
	s.mu.Lock()
	s.quitting = true
	s.mu.Unlock()
	s.tray.BeginQuit()
	s.app.Quit()
}

// MinimizeToTrayGet answers minimizeToTray.get
func (b *WindowService) MinimizeToTrayGet() tray.Status { return b.shell.tray.Status() }

// MinimizeToTraySet answers minimizeToTray.set
func (b *WindowService) MinimizeToTraySet(on any) (tray.Status, error) {
	return b.shell.tray.SetEnabled(on == true)
}
