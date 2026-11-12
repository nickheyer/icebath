package main

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"

	"github.com/nickheyer/icebath/pkg/desktop/backend"
	"github.com/nickheyer/icebath/pkg/desktop/lifecycle"
	"github.com/nickheyer/icebath/pkg/desktop/tray"
	"github.com/nickheyer/icebath/pkg/desktop/windows"
	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
)

const (
	appName  = "Icebath"
	bundleID = "ai.icebath"
)

// Shell owns the Wails app, the main window and the pkg/desktop services the bindings call.
type Shell struct {
	app       *application.App
	platform  windows.Platform
	userData  string
	devServer string
	rendererD string
	packaged  bool
	argv      []string
	log       func(string)

	mu          sync.Mutex
	main        *shellWindow
	all         map[string]*shellWindow
	seq         int
	quitting    bool
	handoffQuit atomic.Bool
	tray        *tray.Tray

	Windows   *windows.Service
	DeepLinks *lifecycle.DeepLinks
}

// userDataDir is Electron userData, the per user config directory of the app name with the data suffix or the override
func userDataDir() string {
	base, err := os.UserConfigDir()
	if err != nil {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".config")
	}
	env := map[string]string{
		"ICEBATH_DESKTOP_USER_DATA_DIR": os.Getenv("ICEBATH_DESKTOP_USER_DATA_DIR"),
		"ICEBATH_DATA_DIR_SUFFIX":       os.Getenv("ICEBATH_DATA_DIR_SUFFIX"),
	}
	return backend.ResolveDesktopUserData(filepath.Join(base, appName), env, electronPlatform())
}

func electronPlatform() string {
	switch runtime.GOOS {
	case "windows":
		return "win32"
	}
	return runtime.GOOS
}

// rendererDir finds the built renderer next to the executable or in the packaged resources directory
func rendererDir() (string, bool) {
	if v := os.Getenv("ICEBATH_DESKTOP_RENDERER_DIR"); v != "" {
		return v, false
	}
	exe, err := os.Executable()
	if err != nil {
		return "", false
	}
	exe, _ = filepath.EvalSymlinks(exe)
	dir := filepath.Dir(exe)
	for _, c := range []string{
		filepath.Join(dir, "renderer"),
		filepath.Join(dir, "..", "Resources", "renderer"),
		filepath.Join(dir, "..", "share", "icebath-desktop", "renderer"),
		filepath.Join(dir, "dist-wails"),
	} {
		if st, err := os.Stat(filepath.Join(c, "index.html")); err == nil && !st.IsDir() {
			return c, true
		}
	}
	return "", false
}

func main() {
	userData := userDataDir()
	logClose := initLogging(userData)
	defer logClose()
	s := &Shell{
		userData:  userData,
		devServer: os.Getenv("ICEBATH_DESKTOP_DEV_SERVER"),
		argv:      os.Args,
		all:       map[string]*shellWindow{},
		log:       func(line string) { slog.Info(line) },
	}
	dir, packaged := rendererDir()
	s.rendererD, s.packaged = dir, packaged
	if s.devServer == "" && s.rendererD == "" {
		fmt.Fprintln(os.Stderr, "[icebath] renderer bundle not found next to the executable; set ICEBATH_DESKTOP_RENDERER_DIR or ICEBATH_DESKTOP_DEV_SERVER")
		os.Exit(1)
	}
	s.platform = hostPlatform()
	s.DeepLinks = lifecycle.NewDeepLinks(s.devServer != "", func() lifecycle.MainWindow {
		if w := s.mainWindow(); w != nil {
			return w
		}
		return nil
	}, s.log)

	app := application.New(application.Options{
		Name:        appName,
		Description: "Icebath Agent desktop app",
		Services: []application.Service{
			application.NewService(&WindowService{shell: s}),
			application.NewService(&LifecycleService{shell: s}),
		},
		Assets: application.AssetOptions{
			Handler:    s.assetHandler(),
			Middleware: s.bootMiddleware,
		},
		SingleInstance: &application.SingleInstanceOptions{
			UniqueID:               bundleID,
			OnSecondInstanceLaunch: s.onSecondInstance,
		},
		Mac: application.MacOptions{ApplicationShouldTerminateAfterLastWindowClosed: false},
		Linux: application.LinuxOptions{
			ProgramName: "icebath",
		},
	})
	s.app = app
	s.Windows = windows.NewService(&wailsHost{shell: s}, s.platform, userData, s.log, windows.RealClock)
	s.startTray()
	app.Event.OnApplicationEvent(events.Common.ApplicationStarted, func(*application.ApplicationEvent) {
		nativeThemeStarted()
		s.applyFrameThemes()
		s.tray.Start()
	})

	app.Event.OnApplicationEvent(events.Common.ApplicationLaunchedWithUrl, func(e *application.ApplicationEvent) {
		s.DeepLinks.Handle(e.Context().URL())
	})
	app.Event.OnApplicationEvent(events.Common.ThemeChanged, func(*application.ApplicationEvent) {
		s.Windows.NativeThemeUpdated()
	})
	app.OnShutdown(func() {
		s.tray.WillQuit()
		s.Windows.FlushPersistence()
	})

	windows.InstallApplicationMenuAfterFirstWindow(windows.MenuStartup[*application.Menu]{
		IsMac:              s.platform.IsMac(),
		BuildMenu:          s.buildApplicationMenu,
		SetApplicationMenu: func(m *application.Menu) { app.Menu.Set(m) },
		CreateWindow:       s.createMainWindow,
	})
	s.registerProtocol()
	if url := s.DeepLinks.ExtractDeepLink(os.Args); url != "" {
		s.DeepLinks.Handle(url)
	}
	if err := app.Run(); err != nil {
		slog.Error("desktop app failed", "error", err)
		os.Exit(1)
	}
}

func initLogging(userData string) func() {
	dir := filepath.Join(userData, "logs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return func() {}
	}
	f, err := os.OpenFile(filepath.Join(dir, "desktop-shell.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return func() {}
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(f, nil)))
	return func() { f.Close() }
}

// assetHandler serves the built renderer, or proxies the development server named by ICEBATH_DESKTOP_DEV_SERVER
func (s *Shell) assetHandler() http.Handler {
	if s.devServer != "" {
		target, err := url.Parse(s.devServer)
		if err != nil {
			fmt.Fprintln(os.Stderr, "[icebath] invalid ICEBATH_DESKTOP_DEV_SERVER: "+err.Error())
			os.Exit(1)
		}
		return httputil.NewSingleHostReverseProxy(target)
	}
	return application.AssetFileServerFS(os.DirFS(s.rendererD))
}

func (s *Shell) onSecondInstance(data application.SecondInstanceData) {
	if lifecycle.HasClosePreviewFlag(data.Args) {
		s.DeepLinks.Handle("icebath://close-preview")
	}
	url := s.DeepLinks.ExtractDeepLink(data.Args)
	if url != "" {
		s.DeepLinks.Handle(url)
	}
	deep := "no"
	if url != "" {
		deep = "yes"
	}
	s.log("[second-instance] relaunch (deepLink=" + deep + ")")
	main := s.mainWindow()
	if main == nil {
		s.createMainWindow()
		return
	}
	if url == "" {
		main.Activate()
	}
}

func (s *Shell) mainWindow() *shellWindow {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.main == nil || s.main.IsDestroyed() {
		return nil
	}
	return s.main
}

func (s *Shell) windowsList() []windows.Window {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]windows.Window, 0, len(s.all))
	for _, w := range s.all {
		if !w.IsDestroyed() {
			out = append(out, w)
		}
	}
	return out
}

func (s *Shell) windowByName(name string) *shellWindow {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.all[name]
}

// createMainWindow opens the main chat window with saved geometry, chrome options and lifecycle wiring
func (s *Shell) createMainWindow() {
	saved := s.Windows.WindowFile.Read()
	var state windows.State
	if saved != nil {
		state = *saved
	} else {
		size := windows.WindowSize(windows.SizeNormal, s.primaryWorkArea())
		state = windows.State{Width: size.Width, Height: size.Height}
	}
	opts := windows.ComputeWindowOptions(state, s.displays(), s.platform.OS)
	win := s.newChatWindow("main", opts)
	s.mu.Lock()
	s.main = win
	s.mu.Unlock()
	s.Windows.RegisterBacked(win)
	if saved != nil && saved.IsMaximized {
		win.Maximize()
	}
	hideToTray := s.tray.RegisterWindow(trayWindow{win}, true)
	win.onClose = func() bool { return !hideToTray() }
	win.onDestroyed(func() { s.tray.Closed(trayWindow{win}) })
	windows.BindGeometryPersistence(win, s.Windows.SchedulePersistWindowState)
	win.reveal(func() { s.Windows.SchedulePersistWindowState() })
}

func (s *Shell) newChatWindow(name string, opts windows.Options) *shellWindow {
	surface := s.Windows.ChatWindowSurfaceOptions()
	wo := application.WebviewWindowOptions{
		Name:                       name,
		Title:                      appName,
		Width:                      opts.Width,
		Height:                     opts.Height,
		MinWidth:                   windows.MinWidth,
		MinHeight:                  windows.MinHeight,
		Hidden:                     true,
		URL:                        "/",
		Frameless:                  !s.platform.IsMac(),
		DevToolsEnabled:            true,
		DefaultContextMenuDisabled: false,
		Mac: application.MacWindow{
			TitleBar:                application.MacTitleBarHiddenInset,
			InvisibleTitleBarHeight: int(windows.MacTitleBarOverlayHeight(s.platform.DarwinMajor, windows.TitlebarHeight)),
			Backdrop:                macBackdrop(surface),
		},
		Windows: application.WindowsWindow{BackdropType: windowsBackdrop(surface)},
	}
	if opts.X != nil && opts.Y != nil {
		wo.InitialPosition = application.WindowXY
		wo.X, wo.Y = *opts.X, *opts.Y
	} else {
		wo.InitialPosition = application.WindowCentered
	}
	if surface.SetBackground {
		wo.BackgroundColour = hexRGBA(surface.BackgroundColor)
	} else {
		wo.BackgroundType = application.BackgroundTypeTransparent
	}
	ww := s.app.Window.NewWithOptions(wo)
	sw := newShellWindow(s, ww)
	s.mu.Lock()
	s.all[name] = sw
	s.mu.Unlock()
	if surface.SetOpacity {
		sw.SetOpacity(surface.Opacity)
	}
	windows.BindWindowChromeEvents(sw, func(fs *bool, target *shellWindow) { s.Windows.PublishWindowState(fs, target) })
	windows.InstallZoomReassertOnWindowEvents(sw, func() { sw.reassertZoom() }, s.platform.OS, windows.RealClock)
	windows.InstallZoomReassertOnNavigation(sw, func() { sw.reassertZoom() })
	sw.onRuntimeReady(func() {
		s.Windows.RestoreZoomLevel(sw)
		s.Windows.PublishWindowState(nil, sw)
	})
	sw.onDestroyed(func() {
		s.Windows.Forget(sw)
		s.mu.Lock()
		delete(s.all, name)
		if s.main == sw {
			s.main = nil
		}
		s.mu.Unlock()
	})
	return sw
}

func (s *Shell) newInstanceWindow() {
	s.mu.Lock()
	s.seq++
	name := fmt.Sprintf("instance-%d", s.seq)
	s.mu.Unlock()
	size := windows.WindowSize(windows.SizeNormal, s.primaryWorkArea())
	w := s.newChatWindow(name, windows.Options{Width: size.Width, Height: size.Height})
	s.Windows.RegisterBacked(w)
	s.tray.RegisterWindow(trayWindow{w}, false)
	w.onDestroyed(func() { s.tray.Closed(trayWindow{w}) })
	w.reveal(func() {})
}

func (s *Shell) primaryWorkArea() windows.Rect {
	if p := s.app.Screen.GetPrimary(); p != nil {
		return windows.Rect{X: p.WorkArea.X, Y: p.WorkArea.Y, Width: p.WorkArea.Width, Height: p.WorkArea.Height}
	}
	return windows.Rect{Width: 1280, Height: 820}
}

func (s *Shell) displays() []windows.Display {
	var out []windows.Display
	for _, sc := range s.app.Screen.GetAll() {
		wa := windows.Rect{X: sc.WorkArea.X, Y: sc.WorkArea.Y, Width: sc.WorkArea.Width, Height: sc.WorkArea.Height}
		b := windows.Rect{X: sc.Bounds.X, Y: sc.Bounds.Y, Width: sc.Bounds.Width, Height: sc.Bounds.Height}
		out = append(out, windows.Display{WorkArea: &wa, Bounds: &b})
	}
	return out
}

func (s *Shell) registerProtocol() {
	exe, _ := os.Executable()
	defaultApp := s.devServer != ""
	reg := protocolRegistrar{}
	for _, proto := range []string{lifecycle.Protocol(s.devServer != ""), legacyProtocol(s.devServer != "")} {
		lifecycle.RegisterDeepLinkProtocol(reg, proto, os.Getenv, defaultApp, os.Args, exe, func(p string) string {
			abs, err := filepath.Abs(p)
			if err != nil {
				return p
			}
			return abs
		}, s.log)
	}
}

func legacyProtocol(dev bool) string {
	if dev {
		return "hermes-dev"
	}
	return "hermes"
}
