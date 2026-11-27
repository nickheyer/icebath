//go:build windows

package terminal

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

// Enter as a program on a Windows pseudo console reads it
const ptyEnter = "\r\n"

var errNotTerminated = errors.New("Could not terminate the child.")

// Program on a pseudo console, for background jobs driven through their prompts
type ptyProc struct {
	console windows.Handle
	process windows.Handle
	pid     int
	in      *os.File
	out     *os.File
	done    chan struct{}
	code    *int
	mu      sync.Mutex
	closed  bool
}

// Starts argv on a fresh pseudo console of rows by cols
func startPty(argv []string, dir string, env []string, rows, cols int) (*ptyProc, error) {
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
	if err := windows.CreatePseudoConsole(windows.Coord{X: int16(cols), Y: int16(rows)}, inRead, outWrite, 0, &console); err != nil {
		for _, h := range []windows.Handle{inRead, inWrite, outRead, outWrite} {
			windows.CloseHandle(h)
		}
		return nil, err
	}
	windows.CloseHandle(inRead)
	windows.CloseHandle(outWrite)
	p := &ptyProc{console: console, in: os.NewFile(uintptr(inWrite), "conpty-in"), out: os.NewFile(uintptr(outRead), "conpty-out"), done: make(chan struct{})}
	if err := p.spawn(argv, dir, env); err != nil {
		windows.ClosePseudoConsole(console)
		p.in.Close()
		p.out.Close()
		return nil, err
	}
	go func() {
		windows.WaitForSingleObject(p.process, windows.INFINITE)
		var code uint32
		if windows.GetExitCodeProcess(p.process, &code) == nil {
			c := int(code)
			p.code = &c
		}
		close(p.done)
	}()
	return p, nil
}

// Creates the process attached to the pseudo console
func (p *ptyProc) spawn(argv []string, dir string, env []string) error {
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
	var cwd *uint16
	if dir != "" {
		if cwd, err = windows.UTF16PtrFromString(dir); err != nil {
			return err
		}
	}
	sorted := append([]string(nil), env...)
	sort.Strings(sorted)
	block := utf16.Encode([]rune(strings.Join(sorted, "\x00") + "\x00\x00"))
	var pi windows.ProcessInformation
	flags := uint32(windows.EXTENDED_STARTUPINFO_PRESENT | windows.CREATE_UNICODE_ENVIRONMENT)
	if err := windows.CreateProcess(nil, cmdline, nil, nil, false, flags, &block[0], cwd, &si.StartupInfo, &pi); err != nil {
		return err
	}
	windows.CloseHandle(pi.Thread)
	p.process, p.pid = pi.Process, int(pi.ProcessId)
	return nil
}

func (p *ptyProc) Pid() int { return p.pid }

func (p *ptyProc) Read(buf []byte) (int, error) {
	n, err := p.out.Read(buf)
	if err != nil {
		return n, io.EOF
	}
	return n, nil
}

func (p *ptyProc) Write(data []byte) error {
	_, err := p.in.Write(data)
	return err
}

func (p *ptyProc) Alive() bool {
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}

func (p *ptyProc) Wait() { <-p.done }

func (p *ptyProc) ExitStatus() *int {
	if p.Alive() {
		return nil
	}
	return p.code
}

// Terminates the console's process, true once it is gone
func (p *ptyProc) Terminate(bool) bool {
	if !p.Alive() {
		return true
	}
	windows.TerminateProcess(p.process, 1)
	select {
	case <-p.done:
		return true
	case <-time.After(2 * time.Second):
		return !p.Alive()
	}
}

// Writes the console's end of file keystroke
func (p *ptyProc) SendEOF() error { return p.Write([]byte{26}) }

// Closes the console and its pipes, terminating a program still running
func (p *ptyProc) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	windows.ClosePseudoConsole(p.console)
	p.in.Close()
	p.out.Close()
	if p.Alive() && !p.Terminate(true) {
		return errNotTerminated
	}
	windows.CloseHandle(p.process)
	p.closed = true
	return nil
}
