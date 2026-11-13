package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"os"
	"runtime"

	"github.com/nickheyer/icebath/pkg/desktop/pty"
)

//go:embed package.json
var desktopPackageJSON []byte

// Version of this desktop app, the package.json version the Electron app reported
func desktopAppVersion() string {
	var pkg struct {
		Version string `json:"version"`
	}
	_ = json.Unmarshal(desktopPackageJSON, &pkg)
	return pkg.Version
}

// Binds the pty group preload functions over pkg/desktop/pty
type TerminalService struct {
	shell *Shell
	svc   *pty.Service
}

// Terminal host routed through the connections service, which closes the SSH scoped sessions on teardown
func newTerminalService(s *Shell) *TerminalService {
	svc := pty.NewService(pty.Options{
		IsWindows: runtime.GOOS == "windows",
		Platform:  electronPlatform(),
		HomeDir: func() string {
			home, _ := os.UserHomeDir()
			return home
		},
		UserData:       s.userData,
		IcebathHome:    s.HermesHome,
		AppVersion:     desktopAppVersion,
		Log:            s.DesktopLog.Remember,
		Routes:         s.Connections.svc,
		WindowRoutes:   s.WindowRoutes,
		PrimaryProfile: func() string { return s.PrimaryPin.Resolve(s.Connections.svc.ActiveProfile) },
	})
	s.Connections.svc.AttachTerminalSessions(svc)
	return &TerminalService{shell: s, svc: svc}
}

// Calling window a terminal streams its output and exit to
type terminalSender struct{ w *shellWindow }

func (t terminalSender) ID() string                     { return t.w.w.Name() }
func (t terminalSender) Send(event string, payload any) { t.w.Send(event, payload) }
func (t terminalSender) OnDestroyed(f func())           { t.w.onDestroyed(f) }

func (b *TerminalService) callerID(ctx context.Context) string {
	if w := callerWindow(b.shell, ctx); w != nil {
		return w.w.Name()
	}
	return ""
}

// Handles terminal.start for the calling window
func (b *TerminalService) TerminalStart(ctx context.Context, options any) (pty.StartResult, error) {
	w := callerWindow(b.shell, ctx)
	if w == nil {
		return pty.StartResult{}, errors.New("terminal.start was called without a window")
	}
	return b.svc.TerminalStart(terminalSender{w}, options)
}

// Handles terminal.attach for the calling window
func (b *TerminalService) TerminalAttach(ctx context.Context, id any) bool {
	return b.svc.TerminalAttach(b.callerID(ctx), id)
}

// Handles terminal.write
func (b *TerminalService) TerminalWrite(id any, data any) bool { return b.svc.TerminalWrite(id, data) }

// Handles terminal.resize
func (b *TerminalService) TerminalResize(id any, size any) (bool, error) {
	return b.svc.TerminalResize(id, size)
}

// Handles terminal.cwd
func (b *TerminalService) TerminalCwd(id any) *string { return b.svc.TerminalCwd(id) }

// Handles terminal.dispose
func (b *TerminalService) TerminalDispose(id any) bool { return b.svc.TerminalDispose(id) }

// Kills every shell on quit without reporting the kills to the renderer as exits
func (b *TerminalService) ServiceShutdown() error {
	b.svc.DisposeAllTerminalSessions()
	return nil
}
