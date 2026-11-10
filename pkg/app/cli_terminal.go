package app

import (
	"errors"
	"io"
	"os"
	"syscall"
)

// Runs fn on the application loop when one exists, else inline
func runOnAppLoop(app tuiApplication, fn func()) {
	if app != nil && app.CallSoonThreadsafe(fn) {
		return
	}
	fn()
}

// Writes a raw escape sequence through the application output, else the console
func writeTerminalSequence(app tuiApplication, seq string) error {
	if app != nil {
		return app.WriteRaw(seq)
	}
	_, err := io.WriteString(console().Out, seq)
	return err
}

// Reports an operating system level failure, the Go shape of a Python OSError
func isOSError(err error) bool {
	var errno syscall.Errno
	var pathErr *os.PathError
	var sysErr *os.SyscallError
	return errors.As(err, &errno) || errors.As(err, &pathErr) || errors.As(err, &sysErr)
}

// Reports the EIO errno of a dead pty behind the error
func isEIO(err error) bool { return errors.Is(err, syscall.EIO) }

// Freezes UI paints once the pty or stdout became unusable
func (c *CLI) markTerminalIOBroken(reason string) {
	c.uiMu.Lock()
	if c.terminalIOBroken {
		c.uiMu.Unlock()
		return
	}
	c.terminalIOBroken = true
	c.uiMu.Unlock()
	c.petStopAnim()
	suffix := ""
	if reason != "" {
		suffix = " (" + reason + ")"
	}
	cliLogger.Warn("Terminal I/O broken" + suffix + " — freezing UI paints to avoid redraw storm (#81521)")
}

// Invalidates the application, EIO freezing paints, OS errors returned, the rest returned unless swallowed
func (c *CLI) appInvalidate(app tuiApplication, where string, swallow bool) error {
	err := app.Invalidate()
	switch {
	case err == nil:
		return nil
	case isEIO(err):
		c.markTerminalIOBroken(where)
		return nil
	case isOSError(err) || !swallow:
		return err
	}
	return nil
}

// Throttled repaint for high frequency background updates, dropped while a resize recovery is pending
func (c *CLI) invalidate() error { return c.invalidateWithin(0.25) }

// Repaints when at least minInterval seconds passed since the last throttled repaint
func (c *CLI) invalidateWithin(minInterval float64) error {
	c.uiMu.Lock()
	if c.terminalIOBroken || c.resizeRecoveryPending {
		c.uiMu.Unlock()
		return nil
	}
	now := monotonic()
	last := c.lastInvalidate
	app := c.app
	if app == nil || (last != nil && now-*last < minInterval) {
		c.uiMu.Unlock()
		return nil
	}
	c.lastInvalidate = &now
	c.uiMu.Unlock()
	return c.appInvalidate(app, "invalidate", false)
}

// Unthrottled repaint for user blocking modal prompts
func (c *CLI) paintNow() {
	c.uiMu.Lock()
	broken := c.terminalIOBroken
	monitor := c.subagentMonitor
	app := c.app
	c.uiMu.Unlock()
	if broken {
		return
	}
	if monitor != nil {
		monitor.invalidate()
	}
	if app != nil {
		if err := c.appInvalidate(app, "paint_now", true); err != nil {
			cliLogger.Error("paint_now failed", "error", err)
		}
	}
}
