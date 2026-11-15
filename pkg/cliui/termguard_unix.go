//go:build !windows

package cliui

import (
	"io"
	"os"
	"os/signal"
	"syscall"

	xterm "github.com/charmbracelet/x/term"
	"golang.org/x/sys/unix"
)

// Restores the terminal when a terminating signal arrives during a hold, then lets that signal end the process
func watchTermination(g *termGuard) func() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT, syscall.SIGINT)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case sig := <-ch:
				if sig == syscall.SIGINT && g.rawHeld() {
					continue
				}
				RestoreTerminal()
				signal.Reset(sig)
				syscall.Kill(syscall.Getpid(), sig.(syscall.Signal))
				return
			case <-done:
				return
			}
		}
	}()
	return func() {
		signal.Stop(ch)
		close(done)
	}
}

// Stops the terminal echoing typed keys, leaving every other mode as it is
func muteEcho(f *os.File) {
	fd := int(f.Fd())
	t, err := unix.IoctlGetTermios(fd, ioctlGetTermios)
	if err != nil {
		return
	}
	t.Lflag &^= unix.ECHO
	unix.IoctlSetTermios(fd, ioctlSetTermios, t)
}

// Hands the terminal back to the shell for a stop request and takes it again once the process continues
func (c *Console) suspend() bool {
	g := c.termGuard()
	if g.file == nil {
		return false
	}
	g.mu.Lock()
	for i := len(g.modes) - 1; i >= 0; i-- {
		io.WriteString(g.modes[i].out, g.modes[i].off)
	}
	if g.pristine != nil {
		xterm.Restore(g.file.Fd(), g.pristine)
	}
	g.mu.Unlock()
	syscall.Kill(0, syscall.SIGTSTP)
	g.mu.Lock()
	defer g.mu.Unlock()
	switch {
	case g.holds > 0:
		xterm.MakeRaw(g.file.Fd())
	case g.quiet > 0:
		muteEcho(g.file)
	}
	for _, m := range g.modes {
		io.WriteString(m.out, m.on)
	}
	return true
}
