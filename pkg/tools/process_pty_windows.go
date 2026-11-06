//go:build windows

package tools

import (
	"errors"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Pseudo console child for pty=true background spawns, the pywinpty contract on Windows

var errPtyNotTerminated = errors.New("Could not terminate the child.")

type ptyProcess struct {
	console  windows.Handle
	process  windows.Handle
	pid      int
	in       *os.File
	out      *os.File
	done     chan struct{}
	exitCode *int
	closeMu  sync.Mutex
	closed   bool
}

// Starts argv on a fresh pseudo console of rows by cols
func spawnPty(argv []string, cwd string, env map[string]string, rows, cols int) (*ptyProcess, error) {
	var inRead, inWrite, outRead, outWrite windows.Handle
	if err := windows.CreatePipe(&inRead, &inWrite, nil, 0); err != nil {
		return nil, err
	}
	if err := windows.CreatePipe(&outRead, &outWrite, nil, 0); err != nil {
		windows.CloseHandle(inRead)
		windows.CloseHandle(inWrite)
		return nil, err
	}
	var console windows.Handle
	size := windows.Coord{X: int16(cols), Y: int16(rows)}
	if err := windows.CreatePseudoConsole(size, inRead, outWrite, 0, &console); err != nil {
		for _, h := range []windows.Handle{inRead, inWrite, outRead, outWrite} {
			windows.CloseHandle(h)
		}
		return nil, err
	}
	windows.CloseHandle(inRead)
	windows.CloseHandle(outWrite)
	p := &ptyProcess{console: console, in: os.NewFile(uintptr(inWrite), "conpty-in"), out: os.NewFile(uintptr(outRead), "conpty-out"), done: make(chan struct{})}
	if err := p.spawn(argv, cwd, env); err != nil {
		windows.ClosePseudoConsole(console)
		p.in.Close()
		p.out.Close()
		return nil, err
	}
	go func() {
		windows.WaitForSingleObject(p.process, windows.INFINITE)
		var code uint32
		if err := windows.GetExitCodeProcess(p.process, &code); err == nil {
			c := int(code)
			p.exitCode = &c
		}
		close(p.done)
	}()
	return p, nil
}

// Creates the process attached to the pseudo console
func (p *ptyProcess) spawn(argv []string, cwd string, env map[string]string) error {
	attrs, err := windows.NewProcThreadAttributeList(1)
	if err != nil {
		return err
	}
	defer attrs.Delete()
	if err := attrs.Update(windows.PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE, *(*unsafe.Pointer)(unsafe.Pointer(&p.console)), unsafe.Sizeof(p.console)); err != nil {
		return err
	}
	si := &windows.StartupInfoEx{ProcThreadAttributeList: attrs.List()}
	si.Cb = uint32(unsafe.Sizeof(*si))
	si.Flags |= windows.STARTF_USESTDHANDLES
	cmdline, err := windows.UTF16PtrFromString(windows.ComposeCommandLine(argv))
	if err != nil {
		return err
	}
	var dir *uint16
	if cwd != "" {
		if dir, err = windows.UTF16PtrFromString(cwd); err != nil {
			return err
		}
	}
	block, err := ptyEnvironBlock(env)
	if err != nil {
		return err
	}
	var pi windows.ProcessInformation
	flags := uint32(windows.EXTENDED_STARTUPINFO_PRESENT | windows.CREATE_UNICODE_ENVIRONMENT)
	if err := windows.CreateProcess(nil, cmdline, nil, nil, false, flags, block, dir, &si.StartupInfo, &pi); err != nil {
		return err
	}
	windows.CloseHandle(pi.Thread)
	p.process, p.pid = pi.Process, int(pi.ProcessId)
	return nil
}

// Environment as one block of NUL separated UTF-16 strings sorted by name
func ptyEnvironBlock(env map[string]string) (*uint16, error) {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k + "=" + env[k] + "\x00")
	}
	b.WriteString("\x00")
	u := utf16.Encode([]rune(b.String()))
	return &u[0], nil
}

func (p *ptyProcess) Pid() int { return p.pid }

func (p *ptyProcess) Read(buf []byte) (int, error) {
	n, err := p.out.Read(buf)
	if err != nil {
		return n, io.EOF
	}
	return n, nil
}

func (p *ptyProcess) Write(data []byte) error {
	_, err := p.in.Write(data)
	return err
}

func (p *ptyProcess) IsAlive() bool {
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}

func (p *ptyProcess) Wait() { <-p.done }

// Exit status once the child ended, nil while running
func (p *ptyProcess) ExitStatus() *int {
	select {
	case <-p.done:
		return p.exitCode
	default:
		return nil
	}
}

// Terminates the console process, true once it is gone
func (p *ptyProcess) Terminate(force bool) bool {
	if !p.IsAlive() {
		return true
	}
	windows.TerminateProcess(p.process, 1)
	select {
	case <-p.done:
		return true
	case <-time.After(2 * time.Second):
		return !p.IsAlive()
	}
}

// Resizes the pseudo console to rows by cols
func (p *ptyProcess) Resize(rows, cols int) error {
	return windows.ResizePseudoConsole(p.console, windows.Coord{X: int16(cols), Y: int16(rows)})
}

// Writes the console end of file keystroke
func (p *ptyProcess) SendEOF() error { return p.Write([]byte{26}) }

// Closes the console and its pipes, terminating a child that is still alive
func (p *ptyProcess) Close() error {
	p.closeMu.Lock()
	defer p.closeMu.Unlock()
	if p.closed {
		return nil
	}
	windows.ClosePseudoConsole(p.console)
	p.in.Close()
	p.out.Close()
	if p.IsAlive() && !p.Terminate(true) {
		return errPtyNotTerminated
	}
	windows.CloseHandle(p.process)
	p.closed = true
	return nil
}
