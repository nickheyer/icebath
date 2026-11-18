//go:build windows

package tty

import (
	"errors"
	"os"

	"github.com/charmbracelet/x/term"
	"golang.org/x/sys/windows"
)

// Puts the console input into raw VT input mode and the output into VT processing, keeping both modes
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
	if t.outState, err = term.GetState(out.Fd()); err != nil {
		return nil, errors.Join(err, t.restore())
	}
	if err := t.vt(); err != nil {
		return nil, errors.Join(err, t.restore())
	}
	return t, nil
}

// Turns on VT input on the console input and VT processing on the console output
func (t *ttyState) vt() error {
	var mode uint32
	if err := windows.GetConsoleMode(windows.Handle(t.in.Fd()), &mode); err != nil {
		return err
	}
	if err := windows.SetConsoleMode(windows.Handle(t.in.Fd()), mode|windows.ENABLE_VIRTUAL_TERMINAL_INPUT); err != nil {
		return err
	}
	if err := windows.GetConsoleMode(windows.Handle(t.out.Fd()), &mode); err != nil {
		return err
	}
	return windows.SetConsoleMode(windows.Handle(t.out.Fd()), mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING|windows.DISABLE_NEWLINE_AUTO_RETURN)
}

// Raw VT mode again after the console was given back
func (t *ttyState) raw() error {
	if _, err := term.MakeRaw(t.in.Fd()); err != nil {
		return err
	}
	return t.vt()
}

// Makes an opened console input the standard input for the session, the reader then seeing console resize events
func adoptInput(in *os.File) (func() error, error) {
	if in == os.Stdin {
		return func() error { return nil }, nil
	}
	old, err := windows.GetStdHandle(windows.STD_INPUT_HANDLE)
	if err != nil {
		return nil, err
	}
	if err := windows.SetStdHandle(windows.STD_INPUT_HANDLE, windows.Handle(in.Fd())); err != nil {
		return nil, err
	}
	prev := os.Stdin
	os.Stdin = in
	return func() error {
		os.Stdin = prev
		return windows.SetStdHandle(windows.STD_INPUT_HANDLE, old)
	}, nil
}

// Windows has no job control, so no shell can stop and continue the program
func JobControl() bool { return false }

// Windows has no job control, so a suspend request leaves the program running
func (p *Program) suspend(string) {}

// Resize arrives through the console input reader on Windows, so there is nothing to watch
func (p *Program) signals() func() { return func() {} }
