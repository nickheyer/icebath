//go:build windows

package gateway

import (
	"bytes"
	"io/fs"
	"os"
	"runtime"
	"syscall"
	"time"
	"unsafe"

	"github.com/nickheyer/icebath/pkg/home"
	"golang.org/x/sys/windows"
)

const pipeClientSQOS = 0x00100000 | 0x00010000

var (
	modPipeKernel32                 = windows.NewLazySystemDLL("kernel32.dll")
	modPipeAdvapi32                 = windows.NewLazySystemDLL("advapi32.dll")
	procGetNamedPipeServerSessionID = modPipeKernel32.NewProc("GetNamedPipeServerSessionId")
	procWaitNamedPipeW              = modPipeKernel32.NewProc("WaitNamedPipeW")
	procImpersonateNamedPipeClient  = modPipeAdvapi32.NewProc("ImpersonateNamedPipeClient")
)

type win32PipeFacts struct{}

var nativePipeFacts pipePeerFacts = win32PipeFacts{}

// S-1 text of the SID at sid
func sidText(sid *windows.SID) (string, error) {
	var text *uint16
	if err := windows.ConvertSidToStringSid(sid, &text); err != nil {
		return "", pipePermissionError("cannot encode bootstrap peer identity")
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(text)))
	return windows.UTF16PtrToString(text), nil
}

// String SID of a token's user
func tokenSID(token windows.Token) (string, error) {
	user, err := token.GetTokenUser()
	if err != nil {
		return "", pipePermissionError("cannot read bootstrap peer identity")
	}
	return sidText(user.User.Sid)
}

func (win32PipeFacts) ownPID() uint32 { return uint32(os.Getpid()) }

func (win32PipeFacts) processSID(pid uint32) (string, error) {
	process, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return "", pipePermissionError("cannot verify bootstrap peer process")
	}
	defer windows.CloseHandle(process)
	var token windows.Token
	if err := windows.OpenProcessToken(process, windows.TOKEN_QUERY, &token); err != nil {
		return "", pipePermissionError("cannot verify bootstrap peer token")
	}
	defer token.Close()
	return tokenSID(token)
}

// Owner SID and DACL of the pipe object, readable without any access to the server process
func (win32PipeFacts) pipeSecurity(handle uintptr) (*pipeSecurity, error) {
	sd, err := windows.GetSecurityInfo(windows.Handle(handle), windows.SE_KERNEL_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return nil, pipePermissionError("cannot read named-pipe owner")
	}
	owner, _, err := sd.Owner()
	if err != nil || owner == nil {
		return nil, pipePermissionError("cannot read named-pipe owner")
	}
	out := &pipeSecurity{ACEs: []pipeACE{}}
	dacl, _, err := sd.DACL()
	if err != nil && err != windows.ERROR_OBJECT_NOT_FOUND {
		return nil, pipePermissionError("cannot read named-pipe DACL")
	}
	if err == nil && dacl != nil {
		for i := uint32(0); i < uint32(dacl.AceCount); i++ {
			var ace *windows.ACCESS_ALLOWED_ACE
			if err := windows.GetAce(dacl, i, &ace); err != nil {
				return nil, pipePermissionError("cannot read named-pipe DACL")
			}
			entry := pipeACE{Kind: int(ace.Header.AceType)}
			if entry.Kind == 0 || entry.Kind == 1 {
				if entry.SID, err = sidText((*windows.SID)(unsafe.Pointer(&ace.SidStart))); err != nil {
					return nil, err
				}
			}
			out.ACEs = append(out.ACEs, entry)
		}
	}
	if out.Owner, err = sidText(owner); err != nil {
		return nil, err
	}
	return out, nil
}

func (win32PipeFacts) pipeServerSession(handle uintptr) *uint32 {
	var session uint32
	if r, _, _ := procGetNamedPipeServerSessionID.Call(handle, uintptr(unsafe.Pointer(&session))); r == 0 {
		return nil
	}
	return &session
}

func (win32PipeFacts) namedPipeProcessID(handle uintptr, server bool) (uint32, bool) {
	var pid uint32
	var err error
	if server {
		err = windows.GetNamedPipeServerProcessId(windows.Handle(handle), &pid)
	} else {
		err = windows.GetNamedPipeClientProcessId(windows.Handle(handle), &pid)
	}
	return pid, err == nil
}

func (win32PipeFacts) sessionOf(pid uint32) *uint32 { return sessionOf(pid) }

func (win32PipeFacts) processElevation(pid uint32) *bool {
	if elevated, known := home.ProcessElevation(&pid); known {
		return &elevated
	}
	return nil
}

func (win32PipeFacts) processIsElevated() bool { return home.ProcessIsElevated() }

// Client SID from the identification token captured at open, which outlives a short lived client
func (win32PipeFacts) pipeClientSID(handle uintptr) (string, error) {
	runtime.LockOSThread()
	if r, _, _ := procImpersonateNamedPipeClient.Call(handle); r == 0 {
		runtime.UnlockOSThread()
		return "", pipePermissionError("cannot impersonate named-pipe peer")
	}
	var token windows.Token
	openErr := windows.OpenThreadToken(windows.CurrentThread(), windows.TOKEN_QUERY, true, &token)
	if err := windows.RevertToSelf(); err != nil {
		if openErr == nil {
			token.Close()
		}
		return "", impersonationLeakError{}
	}
	runtime.UnlockOSThread()
	if openErr != nil {
		return "", pipePermissionError("cannot read named-pipe peer token")
	}
	defer token.Close()
	return tokenSID(token)
}

func pipeOSError(msg string, err error) error { return &nativePipeError{msg + ": " + err.Error(), err} }

// Milliseconds left before deadline, a deadline error once it has passed
func remainingMs(deadline time.Time) (uint32, error) {
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return 0, pipeDeadlineError()
	}
	return max(1, uint32(remaining/time.Millisecond)), nil
}

func newPipeOverlapped() (*windows.Overlapped, error) {
	event, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		return nil, err
	}
	return &windows.Overlapped{HEvent: event}, nil
}

// Waits for one overlapped transfer by the deadline, cancelling and reaping the native operation on any failure
func pipeComplete(handle windows.Handle, ov *windows.Overlapped, deadline time.Time) (uint32, error) {
	ms, err := remainingMs(deadline)
	if err == nil {
		var event uint32
		if event, err = windows.WaitForSingleObject(ov.HEvent, ms); err == nil && event != windows.WAIT_OBJECT_0 {
			err = pipeDeadlineError()
		}
		if err == nil {
			var n uint32
			if gerr := windows.GetOverlappedResult(handle, ov, &n, false); gerr == nil {
				return n, nil
			} else {
				err = pipeOSError("runtime pipe I/O failed", gerr)
			}
		}
	}
	_ = windows.CancelIoEx(handle, ov)
	var n uint32
	_ = windows.GetOverlappedResult(handle, ov, &n, true)
	return 0, err
}

// Waits up to budget for a client, true once one is connected and false only on the idle cancel
func pipeAccept(handle windows.Handle, budget time.Duration) (bool, error) {
	ov, err := newPipeOverlapped()
	if err != nil {
		return false, err
	}
	defer windows.CloseHandle(ov.HEvent)
	if err := windows.ConnectNamedPipe(handle, ov); err != nil {
		if err == windows.ERROR_PIPE_CONNECTED {
			return true, nil
		}
		if err != windows.ERROR_IO_PENDING {
			return false, err
		}
	}
	if event, _ := windows.WaitForSingleObject(ov.HEvent, max(1, uint32(budget/time.Millisecond))); event != windows.WAIT_OBJECT_0 {
		_ = windows.CancelIoEx(handle, ov)
	}
	var n uint32
	if err := windows.GetOverlappedResult(handle, ov, &n, true); err != nil {
		if err == windows.ERROR_OPERATION_ABORTED {
			return false, nil
		}
		return false, pipeOSError("runtime pipe accept failed", err)
	}
	return true, nil
}

// First line of the peer's message, refusing one longer than maximum
func pipeReadLine(handle windows.Handle, deadline time.Time, maximum int) ([]byte, error) {
	var chunks []byte
	for bytes.IndexByte(chunks, '\n') < 0 {
		buf := make([]byte, min(4096, maximum+1-len(chunks)))
		ov, err := newPipeOverlapped()
		if err != nil {
			return nil, err
		}
		var done uint32
		if err := windows.ReadFile(handle, buf, &done, ov); err != nil && err != windows.ERROR_IO_PENDING && err != windows.ERROR_MORE_DATA {
			windows.CloseHandle(ov.HEvent)
			return nil, err
		}
		count, err := pipeComplete(handle, ov, deadline)
		windows.CloseHandle(ov.HEvent)
		if err != nil {
			return nil, err
		}
		if count == 0 {
			return nil, &nativePipeError{"runtime pipe closed before response", errPipeConnection}
		}
		chunks = append(chunks, buf[:count]...)
		if len(chunks) > maximum {
			return nil, pipePermissionError("runtime control message too large")
		}
	}
	line, _, _ := bytes.Cut(chunks, []byte{'\n'})
	return line, nil
}

// Writes all of data by the deadline
func pipeWrite(handle windows.Handle, data []byte, deadline time.Time) error {
	for len(data) > 0 {
		ov, err := newPipeOverlapped()
		if err != nil {
			return err
		}
		var done uint32
		if err := windows.WriteFile(handle, data, &done, ov); err != nil && err != windows.ERROR_IO_PENDING {
			windows.CloseHandle(ov.HEvent)
			return err
		}
		sent, err := pipeComplete(handle, ov, deadline)
		windows.CloseHandle(ov.HEvent)
		if err != nil {
			return err
		}
		if sent == 0 {
			return &nativePipeError{"runtime pipe closed during write", errPipeConnection}
		}
		data = data[sent:]
	}
	return nil
}

// One request line and its response line over home's control pipe after the server identity check
func queryRuntimeControl(home string, request []byte, timeout time.Duration) ([]byte, error) {
	deadline := time.Now().Add(timeout)
	name, err := WindowsPipeName(home)
	if err != nil {
		return nil, err
	}
	if _, err := remainingMs(deadline); err != nil {
		return nil, err
	}
	namePtr, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, err
	}
	var handle windows.Handle
	for {
		handle, err = windows.CreateFile(namePtr, 0xC0000000, 0, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OVERLAPPED|pipeClientSQOS, 0)
		if err == nil {
			break
		}
		switch err {
		case windows.ERROR_FILE_NOT_FOUND:
			return nil, &nativePipeError{"runtime control pipe absent", fs.ErrNotExist}
		case windows.ERROR_ACCESS_DENIED:
			return nil, pipePermissionError("runtime control pipe inaccessible")
		case windows.ERROR_PIPE_BUSY:
		default:
			return nil, err
		}
		ms, err := remainingMs(deadline)
		if err != nil {
			return nil, err
		}
		if r, _, werr := procWaitNamedPipeW.Call(uintptr(unsafe.Pointer(namePtr)), uintptr(ms)); r == 0 {
			if werr == syscall.Errno(121) {
				return nil, pipeDeadlineError()
			}
			return nil, werr
		}
	}
	defer windows.CloseHandle(handle)
	if _, err := peerSubject(nativePipeFacts, uintptr(handle), true); err != nil {
		return nil, err
	}
	line := append(bytes.TrimRight(append([]byte(nil), request...), "\n"), '\n')
	if err := pipeWrite(handle, line, deadline); err != nil {
		return nil, err
	}
	response, err := pipeReadLine(handle, deadline, 512*1024)
	if err != nil {
		return nil, err
	}
	return append(response, '\n'), nil
}

type win32PipeInstance struct{ handle windows.Handle }

// Listening instance of home's control pipe whose protected DACL admits exactly the current user
func createNativePipe(home string, first bool) (nativePipeInstance, error) {
	sid, err := nativePipeFacts.processSID(uint32(os.Getpid()))
	if err != nil {
		return nil, err
	}
	sd, err := windows.SecurityDescriptorFromString("D:P(A;;GA;;;" + sid + ")")
	if err != nil {
		return nil, pipePermissionError("cannot create private pipe DACL")
	}
	attrs := &windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	name, err := WindowsPipeName(home)
	if err != nil {
		return nil, err
	}
	namePtr, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, err
	}
	flags := uint32(3) | windows.FILE_FLAG_OVERLAPPED
	if first {
		flags |= 0x00080000
	}
	handle, err := windows.CreateNamedPipe(namePtr, flags, 8, nativePipeInstances, 65536, 65536, 2000, attrs)
	if err != nil {
		return nil, err
	}
	return &win32PipeInstance{handle}, nil
}

func (p *win32PipeInstance) accept(budget time.Duration) (bool, error) {
	return pipeAccept(p.handle, budget)
}
func (p *win32PipeInstance) readLine(deadline time.Time, maximum int) ([]byte, error) {
	return pipeReadLine(p.handle, deadline, maximum)
}
func (p *win32PipeInstance) write(data []byte, deadline time.Time) error {
	return pipeWrite(p.handle, data, deadline)
}
func (p *win32PipeInstance) peerSubject() (string, error) {
	return peerSubject(nativePipeFacts, uintptr(p.handle), false)
}

// Recycles the instance for the next client, a client that already left is no failure
func (p *win32PipeInstance) disconnect() error {
	if err := windows.DisconnectNamedPipe(p.handle); err != nil && err != windows.ERROR_PIPE_NOT_CONNECTED {
		return pipeOSError("DisconnectNamedPipe failed", err)
	}
	return nil
}

func (p *win32PipeInstance) close() { _ = windows.CloseHandle(p.handle) }
