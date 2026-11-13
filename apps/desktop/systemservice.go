package main

import (
	"context"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/desktop/system"
	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
)

// SystemService binds the system preload functions
type SystemService struct{ shell *Shell }

// systemHost is the shell surface of the system service, windows, focus and the clipboard
type systemHost struct{ shell *Shell }

func (h systemHost) SendMain(event string, payload any) bool {
	main := h.shell.mainWindow()
	if main == nil {
		return false
	}
	main.Send(event, payload)
	return true
}

func (h systemHost) Broadcast(event string, payload any) {
	for _, w := range h.shell.windowsList() {
		w.Send(event, payload)
	}
}

func (h systemHost) Window(name string) system.NotifyWindow {
	sw := h.shell.windowByName(name)
	if sw == nil || sw.IsDestroyed() {
		return nil
	}
	return notifyWindow{sw: sw}
}

func (h systemHost) MainWindow() system.NotifyWindow {
	sw := h.shell.mainWindow()
	if sw == nil {
		return nil
	}
	return notifyWindow{sw: sw}
}

func (h systemHost) FocusedWindow() string {
	for _, w := range h.shell.windowsList() {
		if w.IsFocused() {
			return w.(*shellWindow).Name()
		}
	}
	return ""
}

// Chat windows load only the renderer, so a call from one comes from the renderer main frame
func (h systemHost) IsRendererWindow(name string) bool { return h.shell.windowByName(name) != nil }

func (h systemHost) WriteClipboardText(text string) { h.shell.app.Clipboard.SetText(text) }

func (h systemHost) ReadClipboardText() string {
	text, _ := h.shell.app.Clipboard.Text()
	return text
}

func (h systemHost) ReadClipboardPNG() []byte {
	return application.InvokeSyncWithResult(nativeClipboardPNG)
}

// notifyWindow adds the attention flash and the one shot focus hook a notification needs
type notifyWindow struct{ sw *shellWindow }

func (n notifyWindow) IsDestroyed() bool              { return n.sw.IsDestroyed() }
func (n notifyWindow) IsMinimized() bool              { return n.sw.IsMinimized() }
func (n notifyWindow) Restore()                       { n.sw.Restore() }
func (n notifyWindow) IsVisible() bool                { return n.sw.IsVisible() }
func (n notifyWindow) ShowInactive()                  { n.sw.ShowInactive() }
func (n notifyWindow) IsFocused() bool                { return n.sw.IsFocused() }
func (n notifyWindow) Focus()                         { n.sw.Focus() }
func (n notifyWindow) Send(event string, payload any) { n.sw.Send(event, payload) }
func (n notifyWindow) FlashFrame(on bool)             { nativeFlashFrame(n.sw, on) }

func (n notifyWindow) OnceFocus(f func()) {
	var once sync.Once
	var cancel func()
	cancel = n.sw.w.OnWindowEvent(events.Common.WindowFocus, func(*application.WindowEvent) {
		once.Do(func() {
			go cancel()
			f()
		})
	})
}

// Runs tick on the UI thread every interval until stopped
func uiInterval(tick func(), every time.Duration) func() {
	ticker := time.NewTicker(every)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-ticker.C:
				application.InvokeAsync(tick)
			case <-done:
				return
			}
		}
	}()
	return func() {
		ticker.Stop()
		close(done)
	}
}

func newSystemService(s *Shell) *system.Service {
	return system.NewService(system.Options{
		Platform:         electronPlatform(),
		IsWSL:            s.platform.IsWSL,
		UserData:         s.userData,
		Log:              s.DesktopLog.Remember,
		Host:             systemHost{shell: s},
		Throttle:         shellThrottle{shell: s},
		Blocker:          system.NewNativeBlocker(appName),
		Notifier:         nativeNotifier(),
		Power:            system.NewPowerMonitor(),
		Screenshot:       nativeScreenshots(),
		ScreenshotHelper: nativeHelperPath("command-screenshot-monitor"),
		SetInterval:      uiInterval,
	})
}

// Wires a chat window into stream throttling, screenshot focus and the active work cleanup of its renderer
func (s *Shell) installSystemWindow(sw *shellWindow) {
	name := sw.Name()
	s.installThrottle(sw)
	sw.onDestroyed(func() { s.System.WindowDestroyed(name) })
	nativeOnRendererGone(sw, func() { s.System.RendererGone(name) })
	sw.On("focus", func() { s.System.ScreenshotWindowFocused(name) })
}

// Name of the window a binding call came from, empty outside a window
func callerName(ctx context.Context) string {
	if w, ok := ctx.Value(application.WindowKey).(application.Window); ok && w != nil {
		return w.Name()
	}
	return ""
}

// ClaimAmbientCue handles claimAmbientCue
func (b *SystemService) ClaimAmbientCue(key any) bool { return b.shell.System.ClaimAmbientCue(key) }

// ScreenshotGetSettings handles screenshot.getSettings
func (b *SystemService) ScreenshotGetSettings(ctx context.Context) (system.ScreenshotStatus, error) {
	return b.shell.System.ScreenshotGetSettings(callerName(ctx))
}

// ScreenshotSetEnabled handles screenshot.setEnabled
func (b *SystemService) ScreenshotSetEnabled(ctx context.Context, enabled any) (system.ScreenshotStatus, error) {
	return b.shell.System.ScreenshotSetEnabled(callerName(ctx), enabled)
}

// ScreenshotOpenPermissionSettings handles screenshot.openPermissionSettings
func (b *SystemService) ScreenshotOpenPermissionSettings(ctx context.Context, kind any) error {
	return b.shell.System.ScreenshotOpenPermissionSettings(callerName(ctx), kind)
}

// ScreenshotCapture handles screenshot.capture, the PNG crossing as base64 the bridge turns back into bytes
func (b *SystemService) ScreenshotCapture(ctx context.Context, requestID any) (system.ScreenshotResult, error) {
	return b.shell.System.ScreenshotCapture(callerName(ctx), requestID)
}

// ScreenshotSubscribe handles the screenshot request subscription of the calling window
func (b *SystemService) ScreenshotSubscribe(ctx context.Context, on any) {
	b.shell.System.ScreenshotSubscribe(callerName(ctx), on)
}

// Notify handles notify for the calling window
func (b *SystemService) Notify(ctx context.Context, payload any) bool {
	return b.shell.System.Notify(callerName(ctx), payload)
}

// ClaimStartupLatency handles claimStartupLatency
func (b *SystemService) ClaimStartupLatency() *int { return b.shell.System.ClaimStartupLatency() }

// RequestMicrophoneAccess handles requestMicrophoneAccess
func (b *SystemService) RequestMicrophoneAccess() bool {
	return b.shell.System.RequestMicrophoneAccess()
}

// WriteClipboard handles writeClipboard
func (b *SystemService) WriteClipboard(text any) bool { return b.shell.System.WriteClipboard(text) }

// ReadClipboard handles readClipboard
func (b *SystemService) ReadClipboard() string { return b.shell.System.ReadClipboard() }

// SaveClipboardImage handles saveClipboardImage
func (b *SystemService) SaveClipboardImage() (string, error) {
	return b.shell.System.SaveClipboardImage()
}

// SetActiveWork handles setActiveWork for the calling window
func (b *SystemService) SetActiveWork(ctx context.Context, payload any) {
	if name := callerName(ctx); name != "" {
		b.shell.System.SetActiveWork(name, payload)
	}
}

// SetKeepAwake handles setKeepAwake
func (b *SystemService) SetKeepAwake(mode any) { b.shell.System.SetKeepAwake(mode) }

// GetOnBattery handles getOnBattery
func (b *SystemService) GetOnBattery() bool { return b.shell.System.GetOnBattery() }
