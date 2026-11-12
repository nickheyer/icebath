//go:build linux

package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type protocolRegistrar struct{}

// SetAsDefaultProtocolClient makes the icebath desktop entry the scheme handler as Electron does through xdg-settings
func (protocolRegistrar) SetAsDefaultProtocolClient(protocol, execPath string, args []string) error {
	desktop := "icebath.desktop"
	if execPath != "" {
		desktop = "icebath-dev.desktop"
		if err := writeDevDesktopEntry(desktop, protocol, execPath, args); err != nil {
			return err
		}
	}
	return runQuiet("xdg-settings", "set", "default-url-scheme-handler", strings.ToLower(protocol), desktop)
}

// writeDevDesktopEntry writes a desktop entry that launches the development shell with its entry argument
func writeDevDesktopEntry(name, protocol, execPath string, args []string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	dir := filepath.Join(home, ".local", "share", "applications")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	quoted := []string{strconv.Quote(execPath)}
	for _, a := range args {
		quoted = append(quoted, strconv.Quote(a))
	}
	body := "[Desktop Entry]\nType=Application\nName=Icebath (dev)\nExec=" + strings.Join(quoted, " ") + " %u\nTerminal=false\nNoDisplay=true\nMimeType=x-scheme-handler/" + protocol + ";\n"
	return os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644)
}
