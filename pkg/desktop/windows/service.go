package windows

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// Platform describes the host the shell runs on in Electron process.platform terms.
type Platform struct {
	OS                    string
	IsWSL                 bool
	DarwinMajor           int
	GlassSupported        bool
	TranslucencySupported bool
	CustomControls        bool
}

// IsMac reports a macOS host
func (p Platform) IsMac() bool { return p.OS == "darwin" }

// IsWindows reports a Windows host
func (p Platform) IsWindows() bool { return p.OS == "win32" }

func (p Platform) overlayPlatform() string {
	switch {
	case p.IsMac():
		return "mac"
	case p.IsWSL || p.CustomControls:
		return "wslg"
	case p.IsWindows():
		return "windows"
	}
	return "linux"
}

// TranslucencyChange flags which parts of the window surface a translucency update touches.
type TranslucencyChange struct {
	Backing  bool
	Material bool
	Opacity  bool
}

// Window is one shell window as the windows group drives it.
type Window interface {
	ControllableWindow
	SizableWindow
	Name() string
	IsFocused() bool
	IsMinimized() bool
	IsVisible() bool
	ShowInactive()
	ZoomLevel() float64
	SetZoomLevel(level float64)
	Send(event string, payload any)
	SetTitleBarOverlay(overlay TitleBarOverlay)
	SetBackgroundColor(color string)
	SetBackgroundMaterial(material string)
	SetVibrancy(material string)
	Opacity() float64
	SetOpacity(opacity float64)
}

// Host is what the windows group needs from the shell.
type Host interface {
	Windows() []Window
	MainWindow() Window
	Screen
	ShouldUseDarkColors() bool
	SetThemeSource(mode string)
}

// WindowState is the window-state-changed payload and the window fields merged into connections.
type WindowState struct {
	IsFullscreen         bool            `json:"isFullscreen"`
	IsMinimized          bool            `json:"isMinimized"`
	IsVisible            bool            `json:"isVisible"`
	NativeOverlayWidth   int             `json:"nativeOverlayWidth"`
	WindowButtonPosition *ButtonPosition `json:"windowButtonPosition"`
	DarwinMajor          int             `json:"darwinMajor"`
	CustomWindowControls bool            `json:"customWindowControls"`
	IsMaximized          bool            `json:"isMaximized"`
}

// ButtonPosition is the macOS traffic light inset.
type ButtonPosition struct {
	X int `json:"x"`
	Y int `json:"y"`
}

// Service implements the windows and chrome preload functions over a Host.
type Service struct {
	host         Host
	platform     Platform
	userData     string
	log          func(string)
	Sizing       *Sizing
	Chrome       *Chrome
	WindowFile   StateFile
	ZoomFile     ZoomStateFile
	mu           sync.Mutex
	translucency TranslucencyState
	backed       map[Window]bool
	writer       *Debouncer
	persist      *Debouncer
}

// NewService loads persisted chrome state from userData and binds it to host
func NewService(host Host, platform Platform, userData string, log func(string), clock Clock) *Service {
	s := &Service{
		host:       host,
		platform:   platform,
		userData:   userData,
		log:        log,
		Sizing:     NewSizing(host),
		Chrome:     NewChrome(filepath.Join(userData, "native-theme.json"), log),
		WindowFile: StateFile{Path: filepath.Join(userData, "window-state.json"), Log: log},
		ZoomFile:   ZoomStateFile{Path: filepath.Join(userData, "zoom-state.json"), Log: log},
		backed:     map[Window]bool{},
	}
	s.translucency = s.readTranslucency()
	s.writer = NewDebouncer(s.writeTranslucency, persistDebounce, clock)
	s.persist = NewDebouncer(func() { s.WindowFile.Persist(host.MainWindow(), s.Sizing) }, persistDebounce, clock)
	host.SetThemeSource(s.Chrome.ThemeSource())
	return s
}

func (s *Service) translucencyPath() string { return filepath.Join(s.userData, "translucency.json") }

func (s *Service) readTranslucency() TranslucencyState {
	raw, err := os.ReadFile(s.translucencyPath())
	if err == nil {
		var v any
		if json.Unmarshal(raw, &v) == nil {
			return NormalizeState(v, s.platform.GlassSupported)
		}
	}
	appearance := "light"
	if s.host.ShouldUseDarkColors() {
		appearance = "dark"
	}
	return DefaultTranslucencyState(appearance, s.platform.GlassSupported, s.platform.IsWindows())
}

func (s *Service) writeTranslucency() {
	s.mu.Lock()
	st := s.translucency
	s.mu.Unlock()
	body, _ := json.MarshalIndent(st, "", "  ")
	err := os.MkdirAll(s.userData, 0o755)
	if err == nil {
		err = os.WriteFile(s.translucencyPath(), body, 0o644)
	}
	if err != nil {
		s.log("[translucency] write failed: " + err.Error())
	}
}

// FlushPersistence writes pending translucency and geometry before quit
func (s *Service) FlushPersistence() {
	s.writer.Flush()
	s.persist.Flush()
}

// SchedulePersistWindowState saves main window geometry after a quiet period
func (s *Service) SchedulePersistWindowState() { s.persist.Call() }

// Translucency returns the state in force
func (s *Service) Translucency() TranslucencyState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.translucency
}

// SurfaceOptions is the creation time backing of a translucency backed window.
type SurfaceOptions struct {
	Vibrancy           string
	BackgroundMaterial string
	Opacity            float64
	SetOpacity         bool
	BackgroundColor    string
	SetBackground      bool
}

// ChatWindowSurfaceOptions mirrors the vibrancy, material, opacity and backing a chat window opens with
func (s *Service) ChatWindowSurfaceOptions() SurfaceOptions {
	st := s.Translucency()
	var o SurfaceOptions
	if s.platform.IsMac() {
		o.Vibrancy = VibrancyFor(st)
	}
	o.BackgroundMaterial, _ = WindowBackgroundMaterialOptions(st, s.platform.IsWindows(), s.platform.GlassSupported)
	o.Opacity, o.SetOpacity = WindowOpacityOptions(st, s.platform.TranslucencySupported)
	o.BackgroundColor, o.SetBackground = WindowBackingOptions(st, s.Chrome.WindowBackgroundColor(s.host.ShouldUseDarkColors()))
	return o
}

// RegisterBacked marks win as following the translucency backing
func (s *Service) RegisterBacked(win Window) {
	s.mu.Lock()
	s.backed[win] = true
	s.mu.Unlock()
}

// Forget drops a closed window from every registry
func (s *Service) Forget(win Window) {
	s.mu.Lock()
	delete(s.backed, win)
	s.mu.Unlock()
	s.Sizing.Forget(win)
}

// SetTranslucency normalizes payload and applies only a changed state to every window
func (s *Service) SetTranslucency(payload any) {
	next := NormalizeState(payload, s.platform.GlassSupported)
	s.mu.Lock()
	prev := s.translucency
	if next == prev {
		s.mu.Unlock()
		return
	}
	s.translucency = next
	s.mu.Unlock()
	changed := TranslucencyChange{
		Backing:  GlassActive(prev) != GlassActive(next),
		Material: VibrancyFor(prev) != VibrancyFor(next),
		Opacity:  WindowOpacityFor(prev) != WindowOpacityFor(next),
	}
	s.writer.Call()
	if changed.Backing || changed.Material || changed.Opacity {
		for _, w := range s.host.Windows() {
			s.ApplyWindowTranslucency(w, changed)
		}
	}
}

// ApplyWindowTranslucency updates backdrop, background, vibrancy and opacity of win for changed parts
func (s *Service) ApplyWindowTranslucency(win Window, changed TranslucencyChange) {
	if win == nil || win.IsDestroyed() {
		return
	}
	st := s.Translucency()
	s.mu.Lock()
	backed := s.backed[win]
	s.mu.Unlock()
	themed := s.Chrome.WindowBackgroundColor(s.host.ShouldUseDarkColors())
	if backed {
		if s.platform.IsWindows() && s.platform.GlassSupported {
			ApplyWindowsBackdrop(win, st, changed.Backing, changed.Material, themed)
		} else {
			if changed.Backing {
				if GlassActive(st) {
					win.SetBackgroundColor("#00000000")
				} else {
					win.SetBackgroundColor(themed)
				}
			}
			if changed.Material && s.platform.IsMac() {
				win.SetVibrancy(VibrancyFor(st))
			}
		}
	}
	if changed.Opacity {
		o := NativeOpacityFor(st, s.platform.TranslucencySupported)
		if OpacityNeedsSetting(o, win.Opacity()) {
			win.SetOpacity(o)
		}
	}
}

// TranslucencySupport is the synchronous glass and translucency verdict the preload publishes.
type TranslucencySupport struct {
	Glass        bool `json:"glass"`
	Translucency bool `json:"translucency"`
}

// Support answers the translucency support query
func (s *Service) Support() TranslucencySupport {
	return TranslucencySupport{Glass: s.platform.GlassSupported, Translucency: s.platform.TranslucencySupported}
}

// WindowControl performs a renderer drawn control on the sender window
func (s *Service) WindowControl(win Window, action any) {
	a, _ := action.(string)
	PerformWindowControl(win, a)
}

// WindowSize handles the intro resize request of sender
func (s *Service) WindowSize(sender Window, mode any) {
	m, _ := mode.(string)
	main := s.host.MainWindow()
	if main == nil {
		return
	}
	s.Sizing.HandleSizeRequest(main, sender, m)
}

// ZoomGet returns the level and percent of the sender window
func (s *Service) ZoomGet(win Window) ZoomChanged {
	level := DefaultZoomLevel
	if win != nil && !win.IsDestroyed() {
		level = win.ZoomLevel()
	}
	return ZoomChanged{Level: level, Percent: ZoomLevelToPercent(level)}
}

// ZoomSetPercent applies and persists the percentage on the sender window
func (s *Service) ZoomSetPercent(win Window, percent any) {
	if win == nil || win.IsDestroyed() {
		return
	}
	s.SetAndPersistZoomLevel(win, PercentToZoomLevel(percent))
}

type zoomTarget struct{ win Window }

func (z zoomTarget) SetZoomLevel(level float64)     { z.win.SetZoomLevel(level) }
func (z zoomTarget) Send(event string, payload any) { z.win.Send(event, payload) }

// SetAndPersistZoomLevel applies level, refreshes the titlebar overlay and stores the level
func (s *Service) SetAndPersistZoomLevel(win Window, level float64) {
	if win == nil || win.IsDestroyed() {
		return
	}
	next := ApplyZoomLevel(zoomTarget{win}, level)
	s.ApplyTitleBarOverlay(win)
	s.ZoomFile.Write(next)
}

// RestoreZoomLevel applies the saved zoom to a new window or the default when none is saved
func (s *Service) RestoreZoomLevel(win Window) {
	if win == nil || win.IsDestroyed() {
		return
	}
	if saved := s.ZoomFile.Read(); saved != nil {
		if cur := win.ZoomLevel(); cur-*saved < 1e-9 && *saved-cur < 1e-9 {
			return
		}
		ApplyZoomLevel(zoomTarget{win}, *saved)
		s.ApplyTitleBarOverlay(win)
		return
	}
	ApplyZoomLevel(zoomTarget{win}, DefaultZoomLevel)
	s.ApplyTitleBarOverlay(win)
}

// MenuZoom applies a zoom menu or accelerator action to win
func (s *Service) MenuZoom(win Window, action MenuAction) {
	if win == nil || win.IsDestroyed() {
		return
	}
	if next, ok := AcceleratorZoom(action, win.ZoomLevel()); ok {
		s.SetAndPersistZoomLevel(win, next)
	}
}

// ApplyTitleBarOverlay sets the native overlay of win for its current zoom
func (s *Service) ApplyTitleBarOverlay(win Window) {
	o := s.Chrome.Overlay(s.platform.overlayPlatform(), s.platform.DarwinMajor, s.host.ShouldUseDarkColors(), ZoomFactor(win.ZoomLevel()))
	if o.Disabled {
		return
	}
	win.SetTitleBarOverlay(o)
}

// SetTitleBarTheme stores a valid renderer palette and refreshes every overlay
func (s *Service) SetTitleBarTheme(payload any) {
	if !s.Chrome.SetTitleBarTheme(payload) {
		return
	}
	for _, w := range s.host.Windows() {
		s.ApplyTitleBarOverlay(w)
	}
}

// SetNativeTheme switches the native theme source and persists it
func (s *Service) SetNativeTheme(mode any) {
	if s.Chrome.SetNativeTheme(mode) {
		s.host.SetThemeSource(s.Chrome.ThemeSource())
	}
}

// NativeThemeUpdated refreshes overlays after the OS theme changed outside macOS
func (s *Service) NativeThemeUpdated() {
	if s.platform.IsMac() {
		return
	}
	for _, w := range s.host.Windows() {
		s.ApplyTitleBarOverlay(w)
	}
}

// WindowStateOf returns the chrome state of win
func (s *Service) WindowStateOf(win Window) WindowState {
	st := WindowState{
		NativeOverlayWidth: NativeOverlayWidth(s.platform.IsMac()),
		DarwinMajor:        0,
	}
	if s.platform.IsMac() {
		st.DarwinMajor = s.platform.DarwinMajor
		if win == nil || !win.IsFullScreen() {
			st.WindowButtonPosition = &ButtonPosition{X: WindowButtonPosition.X, Y: WindowButtonPosition.Y}
		}
	}
	if win != nil && !win.IsDestroyed() {
		st.IsFullscreen, st.IsMinimized, st.IsVisible = win.IsFullScreen(), win.IsMinimized(), win.IsVisible()
	}
	cs := WindowControlState(win, s.platform.CustomControls)
	st.CustomWindowControls, st.IsMaximized = cs.CustomWindowControls, cs.IsMaximized
	return st
}

// PublishWindowState sends window-state-changed to target with an optional fullscreen transition
func (s *Service) PublishWindowState(fullscreen *bool, target Window) {
	if target == nil || target.IsDestroyed() {
		return
	}
	st := s.WindowStateOf(target)
	if fullscreen != nil {
		st.IsFullscreen = *fullscreen
	}
	target.Send("window-state-changed", st)
}

// MenuHandlers are the actions a menu item reaches outside the windows group.
type MenuHandlers struct {
	About          func()
	OpenUpdates    func()
	NewWindow      func()
	ToggleDevTools func(win Window)
}

// RunMenuAction performs a clicked application menu action against focused, falling back to the main window
func (s *Service) RunMenuAction(action MenuAction, focused Window, h MenuHandlers) {
	main := s.host.MainWindow()
	sendMain := func(event string, payload any) {
		if main == nil || main.IsDestroyed() {
			return
		}
		main.Send(event, payload)
	}
	switch action {
	case ActionAbout:
		h.About()
	case ActionCheckUpdates:
		h.OpenUpdates()
	case ActionNewWindow:
		h.NewWindow()
	case ActionOpenFolder:
		sendMain("open-folder-requested", nil)
	case ActionClosePreview:
		sendMain("close-preview-requested", nil)
	case ActionPreviewReload:
		sendMain("preview-nav", "reload")
	case ActionToggleDevTools:
		if focused == nil {
			focused = main
		}
		h.ToggleDevTools(focused)
	case ActionZoomReset, ActionZoomIn, ActionZoomOut:
		s.MenuZoom(main, action)
	case ActionCloseWindow:
		if focused != nil {
			focused.Close()
		}
	}
}
