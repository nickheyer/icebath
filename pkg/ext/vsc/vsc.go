// Package vsc opens the file changes of the TUI as diffs in the VS Code window it runs in
package vsc

import (
	"os"
	"sync"

	"github.com/charmbracelet/x/term"

	"github.com/nickheyer/icebath/pkg/ext"
	"github.com/nickheyer/icebath/pkg/harness"
	"github.com/nickheyer/icebath/pkg/tool"
)

// Version the agent announces to the editor, replaced by the binary with its build version
var Version = func() string { return "dev" }

type extension struct{}

func (extension) Manifest() ext.Manifest {
	return ext.Manifest{Name: Name, Version: "1", Description: "VS Code window onto the TUI, file changes as diffs with revert, mode and session state"}
}

// Declares the app capture, the message listener, the install command and the settings panel of the bridge
func (extension) Attach(h *ext.Host) error {
	if err := LoadError(); err != nil {
		return err
	}
	h.Tools(func(a *harness.App) ([]tool.Tool, error) {
		attach(a)
		return nil, nil
	})
	h.Observe(observe)
	h.Command(installCommand())
	h.Panel(ext.Panel{Key: SettingsKey, Title: "VS Code", Detail: "bridge to the VS Code extension, on or off and its traffic log", New: newSettingsCard, Settings: true})
	return nil
}

// App of the process with the bridge dialing for it and the agent hearing its messages
var hub struct {
	mu     sync.Mutex
	app    *harness.App
	bridge *bridge
	agent  *Agent
}

// Remembers the app and starts the bridge when the settings, the variable and the terminal call for one
func attach(a *harness.App) {
	hub.mu.Lock()
	hub.app = a
	hub.mu.Unlock()
	applyToBridge(Current())
}

// Hands every harness message to the agent in place
func observe(msg any) {
	hub.mu.Lock()
	a := hub.agent
	hub.mu.Unlock()
	if a != nil {
		a.Observe(msg)
	}
}

// Target of the bridge, the socket the variable names, else the advertised window of a VS Code terminal running the TUI
func bridgeTarget(c Config) (string, bool) {
	if path, ok := SocketRequested(); ok {
		return path, true
	}
	if c.Enabled && InsideVSCode() && interactive() {
		return discoverTarget, true
	}
	return "", false
}

// Reports whether a person sits at this process, both standard streams being terminals
func interactive() bool {
	return term.IsTerminal(os.Stdin.Fd()) && term.IsTerminal(os.Stdout.Fd())
}

// Starts, retargets or stops the bridge of the app so it matches the settings, the variable and the terminal
func applyToBridge(c Config) error {
	hub.mu.Lock()
	app, b := hub.app, hub.bridge
	hub.mu.Unlock()
	if app == nil {
		return nil
	}
	path, ok := bridgeTarget(c)
	if b == nil {
		if !ok {
			return nil
		}
		b = newBridge(app)
		hub.mu.Lock()
		hub.bridge, hub.agent = b, b.agent
		hub.mu.Unlock()
	}
	b.log.setPath(c.Log)
	if ok {
		b.start(path)
	} else {
		b.stop()
	}
	return nil
}

func init() { ext.Register(extension{}) }
