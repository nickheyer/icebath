package main

import (
	"errors"
	"sync/atomic"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"

	"github.com/nickheyer/icebath/pkg/desktop/connections"
)

// authHostShell is the system browser, the open failure modal and the native sign-in windows of the connections group
type authHostShell struct {
	shell *Shell
	ready atomic.Bool
}

// Marks the host ready once the app event loop runs
func (h *authHostShell) start(app *application.App) {
	app.Event.OnApplicationEvent(events.Common.ApplicationStarted, func(*application.ApplicationEvent) { h.ready.Store(true) })
}

// OpenURL opens a web URL in the default browser
func (h *authHostShell) OpenURL(url string) error {
	return application.Get().Browser.OpenURL(url)
}

// NotifyOpenFailed logs the failure and shows every window the open failure modal with the URL
func (h *authHostShell) NotifyOpenFailed(url, message string) {
	h.shell.DesktopLog.Remember("[open-failed] " + url + ": " + message)
	for _, w := range h.shell.windowsList() {
		w.Send("external-open-failed", map[string]any{"url": url, "message": message})
	}
}

// CanOpenWindows reports whether the app event loop runs
func (h *authHostShell) CanOpenWindows() bool { return h.ready.Load() }

// OpenLoginWindow creates a native sign-in window over the partition's own webview storage
func (h *authHostShell) OpenLoginWindow(spec connections.LoginWindowSpec) (connections.LoginWindow, error) {
	if !h.ready.Load() {
		return nil, errors.New("Desktop is not ready to start an OAuth login.")
	}
	return h.openPlatformLoginWindow(spec)
}
