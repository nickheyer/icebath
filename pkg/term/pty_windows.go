package term

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/nickheyer/icebath/pkg/internal/proc"
)

// Program running on a pseudo console
type Process struct {
	console windows.Handle
	process windows.Handle
	pid     int
	in      *os.File
	out     *os.File
	done    chan struct{}
	exit    Exit
}

// Starts the command on a fresh pseudo console of the given size
func startProcess(c Command, cols, rows int) (*Process, error) {
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
	p := &Process{console: console, in: os.NewFile(uintptr(inWrite), "conpty-in"), out: os.NewFile(uintptr(outRead), "conpty-out"), done: make(chan struct{})}
	if err := p.spawn(c); err != nil {
		windows.ClosePseudoConsole(console)
		p.in.Close()
		p.out.Close()
		return nil, err
	}
	go p.wait()
	return p, nil
}

// Creates the process attached to the pseudo console
func (p *Process) spawn(c Command) error {
	attrs, err := windows.NewProcThreadAttributeList(1)
	if err != nil {
		return err
	}
	defer attrs.Delete()
	if err := attrs.Update(windows.PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE, unsafe.Pointer(p.console), unsafe.Sizeof(p.console)); err != nil {
		return err
	}
	si := &windows.StartupInfoEx{ProcThreadAttributeList: attrs.List()}
	si.Cb = uint32(unsafe.Sizeof(*si))
	line := windows.ComposeCommandLine(append([]string{c.Path}, c.Args...))
	cmdline, err := windows.UTF16PtrFromString(line)
	if err != nil {
		return err
	}
	var dir *uint16
	if c.Dir != "" {
		if dir, err = windows.UTF16PtrFromString(c.Dir); err != nil {
			return err
		}
	}
	env, err := environBlock(c.Env)
	if err != nil {
		return err
	}
	var pi windows.ProcessInformation
	flags := uint32(windows.EXTENDED_STARTUPINFO_PRESENT | windows.CREATE_UNICODE_ENVIRONMENT)
	if err := windows.CreateProcess(nil, cmdline, nil, nil, false, flags, env, dir, &si.StartupInfo, &pi); err != nil {
		return err
	}
	windows.CloseHandle(pi.Thread)
	p.process, p.pid = pi.Process, int(pi.ProcessId)
	return nil
}

// Environment as one block of NUL separated UTF-16 strings, nil for an empty environment to inherit
func environBlock(env []string) (*uint16, error) {
	if len(env) == 0 {
		return nil, nil
	}
	var block []uint16
	for _, kv := range env {
		u, err := windows.UTF16FromString(kv)
		if err != nil {
			return nil, err
		}
		block = append(block, u...)
	}
	block = append(block, 0)
	return &block[0], nil
}

// Waits for the program and records how it ended
func (p *Process) wait() {
	windows.WaitForSingleObject(p.process, windows.INFINITE)
	var code uint32
	if err := windows.GetExitCodeProcess(p.process, &code); err != nil {
		p.exit = Exit{Code: 1, Err: err}
	} else {
		p.exit = Exit{Code: int(code)}
	}
	windows.CloseHandle(p.process)
	close(p.done)
}

// Output of the program
func (p *Process) Read(b []byte) (int, error) {
	n, err := p.out.Read(b)
	if errors.Is(err, windows.ERROR_BROKEN_PIPE) || errors.Is(err, syscall.ERROR_BROKEN_PIPE) {
		return n, os.ErrClosed
	}
	return n, err
}

// Input to the program
func (p *Process) Write(b []byte) (int, error) { return p.in.Write(b) }

// Tells the program the console is cols by rows
func (p *Process) Resize(cols, rows int) error {
	return windows.ResizePseudoConsole(p.console, windows.Coord{X: int16(cols), Y: int16(rows)})
}

// Process id of the program
func (p *Process) Pid() int { return p.pid }

// Working directory of the program read from its process parameters, empty where that cannot be read
func (p *Process) Cwd() string {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_INFORMATION|windows.PROCESS_VM_READ, false, uint32(p.pid))
	if err != nil {
		return ""
	}
	defer windows.CloseHandle(h)
	var info windows.PROCESS_BASIC_INFORMATION
	var got uint32
	if err := windows.NtQueryInformationProcess(h, windows.ProcessBasicInformation, unsafe.Pointer(&info), uint32(unsafe.Sizeof(info)), &got); err != nil {
		return ""
	}
	var peb windows.PEB
	if err := windows.ReadProcessMemory(h, uintptr(unsafe.Pointer(info.PebBaseAddress)), (*byte)(unsafe.Pointer(&peb)), unsafe.Sizeof(peb), nil); err != nil {
		return ""
	}
	var params windows.RTL_USER_PROCESS_PARAMETERS
	if err := windows.ReadProcessMemory(h, uintptr(unsafe.Pointer(peb.ProcessParameters)), (*byte)(unsafe.Pointer(&params)), unsafe.Sizeof(params), nil); err != nil {
		return ""
	}
	dir := params.CurrentDirectory.DosPath
	if dir.Length == 0 || dir.Buffer == nil {
		return ""
	}
	buf := make([]uint16, dir.Length/2)
	if err := windows.ReadProcessMemory(h, uintptr(unsafe.Pointer(dir.Buffer)), (*byte)(unsafe.Pointer(&buf[0])), uintptr(dir.Length), nil); err != nil {
		return ""
	}
	return strings.TrimSuffix(windows.UTF16ToString(buf), `\`)
}

// Closes the console, then kills the program and everything under it when it lingers
func (p *Process) Close() {
	p.in.Close()
	windows.ClosePseudoConsole(p.console)
	select {
	case <-p.done:
	case <-time.After(closeGrace):
		proc.KillTree(p.Pid())
		<-p.done
	}
	p.out.Close()
}

// Default shells on Windows, the first one found on the path
var windowsShells = []string{"pwsh.exe", "powershell.exe", "cmd.exe"}

// First default shell found on the path, the command shell of the system when none is
func defaultShell() string {
	for _, name := range windowsShells {
		if path, err := exec.LookPath(name); err == nil {
			return path
		}
	}
	return "cmd.exe"
}
