package main

import (
	"context"

	"github.com/nickheyer/icebath/pkg/desktop/chrome"
)

// Binds the chrome group preload functions over pkg/desktop/chrome
type ChromeService struct {
	shell *Shell
	svc   *chrome.Service
}

// Chrome group with the persisted Disable F12 DevTools setting of userData
func newChromeService(s *Shell) *ChromeService {
	return &ChromeService{shell: s, svc: chrome.NewService(chrome.Options{UserData: s.userData, Platform: electronPlatform(), Log: s.DesktopLog.Remember})}
}

// Calling window whose webview the chrome verbs act on
type chromeWindow struct{ w *shellWindow }

func (c chromeWindow) EditingCommand(command string) { nativeEditingCommand(c.w.w, command) }

// Handles contextMenuEdit on the focused element of the calling window
func (b *ChromeService) ContextMenuEdit(ctx context.Context, command any) {
	if w := callerWindow(b.shell, ctx); w != nil {
		b.svc.ContextMenuEdit(chromeWindow{w}, command)
	}
}

func (c chromeWindow) ID() string                     { return c.w.w.Name() }
func (c chromeWindow) Send(event string, payload any) { c.w.Send(event, payload) }
func (c chromeWindow) OnDestroyed(f func())           { c.w.onDestroyed(f) }
func (c chromeWindow) IsDestroyed() bool              { return c.w.IsDestroyed() }
func (c chromeWindow) DevToolsOpened() bool           { return nativeDevToolsOpened(c.w) }
func (c chromeWindow) OpenDevTools()                  { nativeOpenDevTools(c.w) }
func (c chromeWindow) CloseDevTools()                 { nativeCloseDevTools(c.w) }

// Hooks the keys a chat window sees before its page into the F12, inspect and find shortcuts
func (s *Shell) installChromeWindow(sw *shellWindow) {
	nativeInstallKeyHook(sw, func(input chrome.KeyInput) bool { return s.Chrome.svc.BeforeInputEvent(chromeWindow{sw}, input) })
}

// Handles setDisableF12
func (b *ChromeService) SetDisableF12(blocked any) { b.svc.SetDisableF12(blocked) }
