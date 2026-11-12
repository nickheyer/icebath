package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"runtime"

	"github.com/nickheyer/icebath/pkg/desktop/lifecycle"
	"github.com/nickheyer/icebath/pkg/desktop/windows"
	"github.com/wailsapp/wails/v3/pkg/application"
)

func callerWindow(s *Shell, ctx context.Context) *shellWindow {
	w, ok := ctx.Value(application.WindowKey).(application.Window)
	if !ok || w == nil {
		return nil
	}
	return s.windowByName(w.Name())
}

// WindowService binds the windows and chrome preload functions.
type WindowService struct{ shell *Shell }

// WindowControl runs windowControls.minimize, toggleMaximize or close on the calling window
func (b *WindowService) WindowControl(ctx context.Context, action any) {
	if w := callerWindow(b.shell, ctx); w != nil {
		b.shell.Windows.WindowControl(w, action)
	}
}

// WindowSize handles chatOnboarding.size for the calling window
func (b *WindowService) WindowSize(ctx context.Context, mode any) {
	if w := callerWindow(b.shell, ctx); w != nil {
		b.shell.Windows.WindowSize(w, mode)
	}
}

// ZoomGet answers zoom.get for the calling window
func (b *WindowService) ZoomGet(ctx context.Context) windows.ZoomChanged {
	w := callerWindow(b.shell, ctx)
	if w == nil {
		return b.shell.Windows.ZoomGet(nil)
	}
	return b.shell.Windows.ZoomGet(w)
}

// ZoomSetPercent handles zoom.setPercent for the calling window
func (b *WindowService) ZoomSetPercent(ctx context.Context, percent any) {
	if w := callerWindow(b.shell, ctx); w != nil {
		b.shell.Windows.ZoomSetPercent(w, percent)
	}
}

// SetTitleBarTheme handles setTitleBarTheme
func (b *WindowService) SetTitleBarTheme(payload any) { b.shell.Windows.SetTitleBarTheme(payload) }

// SetNativeTheme handles setNativeTheme and repaints every window frame
func (b *WindowService) SetNativeTheme(mode any) {
	b.shell.Windows.SetNativeTheme(mode)
	b.shell.applyFrameThemes()
}

// SetTranslucency handles setTranslucency
func (b *WindowService) SetTranslucency(payload any) { b.shell.Windows.SetTranslucency(payload) }

// LifecycleService binds the app lifecycle preload functions.
type LifecycleService struct{ shell *Shell }

// SignalDeepLinkReady handles signalDeepLinkReady
func (b *LifecycleService) SignalDeepLinkReady() lifecycle.ReadyResult {
	return b.shell.DeepLinks.Ready()
}

func (s *Shell) applyFrameThemes() {
	dark := (&wailsHost{shell: s}).ShouldUseDarkColors()
	for _, w := range s.windowsList() {
		nativeApplyWindowTheme(w.(*shellWindow).w, dark)
	}
}

// BootPayload is the synchronous pre paint state boot.js publishes before the bridge runs.
type BootPayload struct {
	Platform             string                      `json:"platform"`
	Translucency         windows.TranslucencySupport `json:"translucency"`
	FeatureFlags         windows.FeatureFlags        `json:"featureFlags"`
	WindowControlsCustom bool                        `json:"windowControlsCustom"`
}

func (s *Shell) bootPayload() BootPayload {
	return BootPayload{
		Platform:             s.platform.OS,
		Translucency:         s.Windows.Support(),
		FeatureFlags:         windows.ResolveFeatureFlags(runtime.GOOS, runtime.GOARCH, s.argv, s.packaged),
		WindowControlsCustom: s.platform.CustomControls,
	}
}

// bootMiddleware serves /__icebath/boot.js and passes every other request to the renderer assets
func (s *Shell) bootMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/__icebath/boot.js" {
			next.ServeHTTP(w, r)
			return
		}
		body, err := json.Marshal(s.bootPayload())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write([]byte("window.__ICEBATH_DESKTOP_BOOT__ = " + string(body) + ";\n"))
	})
}

func hostPlatform() windows.Platform {
	p := windows.Platform{OS: electronPlatform()}
	p.IsWSL = windows.CustomWindowControlsEnabled(p.OS, os.Getenv, windows.KernelRelease())
	p.DarwinMajor = darwinMajor()
	p.GlassSupported = windows.GlassSupportedOn(p.OS, osRelease())
	p.TranslucencySupported = windows.TranslucencySupportedOn(p.OS)
	p.CustomControls = !p.IsMac()
	return p
}
