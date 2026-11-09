package app

import (
	"io"
	"os"
	"sync"

	"github.com/nickheyer/icebath/pkg/cliui"
)

var (
	streamsMu  sync.Mutex
	appConsole *cliui.Console
	appStderr  io.Writer
)

// Console every classic CLI print reaches, the process console unless SetStreams installed another
func console() *cliui.Console {
	streamsMu.Lock()
	defer streamsMu.Unlock()
	if appConsole != nil {
		return appConsole
	}
	return cliui.Default()
}

// Writer sys.stderr prints reach, os.Stderr unless SetStreams installed another
func stderrWriter() io.Writer {
	streamsMu.Lock()
	defer streamsMu.Unlock()
	if appStderr != nil {
		return appStderr
	}
	return os.Stderr
}

// Installs the console and stderr writer prints reach and returns the function restoring the previous pair
func SetStreams(c *cliui.Console, stderr io.Writer) func() {
	streamsMu.Lock()
	prevConsole, prevStderr := appConsole, appStderr
	appConsole, appStderr = c, stderr
	streamsMu.Unlock()
	return func() {
		streamsMu.Lock()
		appConsole, appStderr = prevConsole, prevStderr
		streamsMu.Unlock()
	}
}
