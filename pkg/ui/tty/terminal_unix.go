//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly || solaris || illumos || aix

package tty

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/term"
	"golang.org/x/sys/unix"
)

// Puts the input end into raw mode, keeping what it was
func makeRaw(in, out *os.File) (*ttyState, error) {
	t := &ttyState{in: in, out: out}
	if err := t.saveOrig(); err != nil {
		return nil, err
	}
	st, err := term.MakeRaw(in.Fd())
	if err != nil {
		return nil, err
	}
	t.inState = st
	return t, nil
}

// Raw mode again after the terminal was given back
func (t *ttyState) raw() error {
	_, err := term.MakeRaw(t.in.Fd())
	return err
}

// Input end as it is, the unix input reader polling any descriptor
func adoptInput(*os.File) (func() error, error) {
	return func() error { return nil }, nil
}

// Whether a shell's job control can stop and continue this process group, false for a group the kernel holds orphaned
func JobControl() bool {
	pgrp := unix.Getpgrp()
	sid, err := unix.Getsid(0)
	if err != nil {
		return false
	}
	pid := os.Getpid()
	for range 64 {
		ppid, err := parentOf(pid)
		if err != nil || ppid <= 1 {
			return false
		}
		ppg, err := unix.Getpgid(ppid)
		if err != nil {
			return false
		}
		if ppg != pgrp {
			psid, err := unix.Getsid(ppid)
			return err == nil && psid == sid
		}
		pid = ppid
	}
	return false
}

// Stops the whole process group with SIGTSTP at its default action, returning once continued
func stopGroup() error {
	cont := make(chan os.Signal, 1)
	signal.Notify(cont, syscall.SIGCONT)
	defer signal.Stop(cont)
	restore, err := stopDefault()
	if err != nil {
		return err
	}
	<-cont
	return restore()
}

// Failure of the size request of a terminal that hung up, nil for one still there
func hungUp(out *os.File) error {
	_, _, err := term.GetSize(out.Fd())
	if errors.Is(err, unix.EIO) || errors.Is(err, unix.ENXIO) {
		return err
	}
	return nil
}

// Window size, job control stops and continues, interrupt, terminate and hangup, handled until the returned stop is called
func (p *Program) signals() func() {
	ch := make(chan os.Signal, 4)
	signal.Notify(ch, syscall.SIGWINCH, syscall.SIGTSTP, syscall.SIGCONT, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	quit := make(chan struct{})
	sizes := make(chan [2]int, 1)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-quit:
				return
			case sz := <-sizes:
				p.tea.Send(tea.WindowSizeMsg{Width: sz[0], Height: sz[1]})
			}
		}
	}()
	go func() {
		defer wg.Done()
		w, h := p.size()
		for {
			select {
			case <-quit:
				return
			case s := <-ch:
				switch s {
				case syscall.SIGWINCH:
					if nw, nh := p.size(); nw != w || nh != h {
						w, h = nw, nh
						select {
						case <-sizes:
						default:
						}
						sizes <- [2]int{w, h}
					}
				case syscall.SIGTSTP:
					p.stopSignal()
				case syscall.SIGCONT:
					p.contSignal()
				case syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP:
					p.terminate(s.(syscall.Signal))
				}
			}
		}
	}()
	return func() {
		signal.Stop(ch)
		close(quit)
		wg.Wait()
	}
}

// Stops the group until the shell continues it, the terminal given back with the line, false without job control
func (p *Program) suspend(line string) bool {
	if !JobControl() {
		return false
	}
	p.release()
	if line != "" {
		if _, err := p.out.WriteString(line); err != nil {
			p.mu.Lock()
			p.failLocked(fmt.Errorf("tty: writing the suspend line: %w", err))
			p.mu.Unlock()
			return true
		}
	}
	p.mu.Lock()
	p.conts++
	p.mu.Unlock()
	if err := stopGroup(); err != nil {
		p.mu.Lock()
		p.conts--
		p.failLocked(fmt.Errorf("tty: stopping the process group: %w", err))
		p.mu.Unlock()
		return true
	}
	p.reclaim()
	return true
}

// Takes a stop signal, stopping the group in place while a command owns the terminal, else suspending on the event loop
func (p *Program) stopSignal() {
	if !JobControl() {
		return
	}
	p.mu.Lock()
	owned := p.execing
	if owned {
		p.conts++
	}
	p.mu.Unlock()
	if !owned {
		go p.tea.Send(tea.SuspendMsg{})
		return
	}
	if err := stopGroup(); err != nil {
		p.mu.Lock()
		p.conts--
		p.failLocked(fmt.Errorf("tty: stopping the process group: %w", err))
		p.mu.Unlock()
	}
}

// Takes a continue nobody in tty asked for, the terminal taken anew on the event loop unless a command owns it
func (p *Program) contSignal() {
	p.mu.Lock()
	own := p.conts > 0
	if own {
		p.conts--
	}
	owned := p.execing
	p.mu.Unlock()
	if !own && !owned {
		go p.tea.Send(tea.ResumeMsg{})
	}
}
