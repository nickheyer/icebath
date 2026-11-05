//go:build windows

package proc

import (
	"errors"
	"fmt"
	"time"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Exit code GetExitCodeProcess reports for a process still running
const StillActive = 259

// Filetime of the Unix epoch in 100 nanosecond units since 1601
const epochFiletime = 116444736000000000

// Opens pid for queries, ErrNoSuchProcess when it does not exist
func OpenProcess(pid int, access uint32) (windows.Handle, error) {
	if pid <= 0 {
		return 0, ErrNoSuchProcess
	}
	h, err := windows.OpenProcess(access, false, uint32(pid))
	if err != nil {
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			return 0, ErrNoSuchProcess
		}
		return 0, err
	}
	return h, nil
}

// Whether err is the access denied or privilege missing failure psutil treats as a permission error
func permissionError(err error) bool {
	return errors.Is(err, windows.ERROR_ACCESS_DENIED) || errors.Is(err, windows.ERROR_PRIVILEGE_NOT_HELD)
}

// Creation epoch of pid like psutil create_time, read from the system process list when the handle is denied
func Birth(pid int) (float64, error) {
	h, err := OpenProcess(pid, windows.PROCESS_QUERY_LIMITED_INFORMATION)
	if err != nil {
		if permissionError(err) {
			return systemProcessCreateTime(pid)
		}
		return 0, err
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err == nil && code != StillActive {
		return 0, ErrNoSuchProcess
	}
	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &creation, &exit, &kernel, &user); err != nil {
		if permissionError(err) {
			return systemProcessCreateTime(pid)
		}
		return 0, err
	}
	return float64(creation.Nanoseconds()) / 1e9, nil
}

// Creation epoch of pid from the NtQuerySystemInformation process list
func systemProcessCreateTime(pid int) (float64, error) {
	size := uint32(512 << 10)
	for range 8 {
		buf := make([]byte, size)
		var ret uint32
		err := windows.NtQuerySystemInformation(windows.SystemProcessInformation, unsafe.Pointer(&buf[0]), size, &ret)
		if errors.Is(err, windows.STATUS_INFO_LENGTH_MISMATCH) {
			size = max(ret+64<<10, size*2)
			continue
		}
		if err != nil {
			return 0, err
		}
		for off := 0; off+int(unsafe.Sizeof(windows.SYSTEM_PROCESS_INFORMATION{})) <= len(buf); {
			info := (*windows.SYSTEM_PROCESS_INFORMATION)(unsafe.Pointer(&buf[off]))
			if int(info.UniqueProcessID) == pid {
				return float64(info.CreateTime-epochFiletime) / 1e7, nil
			}
			if info.NextEntryOffset == 0 {
				break
			}
			off += int(info.NextEntryOffset)
		}
		return 0, ErrNoSuchProcess
	}
	return 0, fmt.Errorf("system process list kept outgrowing %d bytes", size)
}

// Start stamp worker records compare against, the creation epoch on Windows
func WorkerBirth(pid int) (float64, error) { return Birth(pid) }

// Boot relative start, which Windows does not expose
func BootStart(pid int) (float64, bool) { return 0, false }

// Windows reaps exited processes itself, so no pid is ever a zombie
func Zombie(pid int) bool { return false }

// Parent pid of pid from a toolhelp snapshot, false when pid is not listed
func PPID(pid int) (int, bool) {
	t, err := snapshot()
	if err != nil {
		return 0, false
	}
	for _, p := range t {
		if p.PID == pid {
			return p.PPID, true
		}
	}
	return 0, false
}

// Reads size bytes of the remote process memory at base
func readRemote(h windows.Handle, base uintptr, size uintptr) ([]byte, error) {
	if size == 0 {
		return []byte{}, nil
	}
	buf := make([]byte, size)
	var got uintptr
	if err := windows.ReadProcessMemory(h, base, &buf[0], size, &got); err != nil {
		return nil, err
	}
	return buf[:got], nil
}

// Command line and environment block of pid read from its process parameters
func pebStrings(pid int) (cmdline string, env []uint16, err error) {
	h, err := OpenProcess(pid, windows.PROCESS_QUERY_INFORMATION|windows.PROCESS_VM_READ)
	if err != nil {
		return "", nil, err
	}
	defer windows.CloseHandle(h)
	var targetWow, selfWow bool
	windows.IsWow64Process(h, &targetWow)
	windows.IsWow64Process(windows.CurrentProcess(), &selfWow)
	if targetWow && !selfWow {
		return peb32Strings(h)
	}
	var pbi windows.PROCESS_BASIC_INFORMATION
	var ret uint32
	if err := windows.NtQueryInformationProcess(h, windows.ProcessBasicInformation, unsafe.Pointer(&pbi), uint32(unsafe.Sizeof(pbi)), &ret); err != nil {
		return "", nil, err
	}
	pebBase := uintptr(unsafe.Pointer(pbi.PebBaseAddress))
	ptrSize := unsafe.Sizeof(uintptr(0))
	raw, err := readRemote(h, pebBase+unsafe.Offsetof(windows.PEB{}.ProcessParameters), ptrSize)
	if err != nil {
		return "", nil, err
	}
	paramsBase := *(*uintptr)(unsafe.Pointer(&raw[0]))
	var params windows.RTL_USER_PROCESS_PARAMETERS
	raw, err = readRemote(h, paramsBase, unsafe.Sizeof(params))
	if err != nil {
		return "", nil, err
	}
	params = *(*windows.RTL_USER_PROCESS_PARAMETERS)(unsafe.Pointer(&raw[0]))
	cmdRaw, err := readRemote(h, uintptr(unsafe.Pointer(params.CommandLine.Buffer)), uintptr(params.CommandLine.Length))
	if err != nil {
		return "", nil, err
	}
	envRaw, err := readRemote(h, uintptr(params.Environment), params.EnvironmentSize)
	if err != nil {
		return "", nil, err
	}
	return windows.UTF16ToString(bytesToUTF16(cmdRaw)), bytesToUTF16(envRaw), nil
}

// Command line and environment of a 32 bit process read from its WOW64 process parameters
func peb32Strings(h windows.Handle) (string, []uint16, error) {
	var peb32 uintptr
	var ret uint32
	if err := windows.NtQueryInformationProcess(h, windows.ProcessWow64Information, unsafe.Pointer(&peb32), uint32(unsafe.Sizeof(peb32)), &ret); err != nil {
		return "", nil, err
	}
	raw, err := readRemote(h, peb32+0x10, 4)
	if err != nil {
		return "", nil, err
	}
	params := uintptr(*(*uint32)(unsafe.Pointer(&raw[0])))
	raw, err = readRemote(h, params, 0x294)
	if err != nil || len(raw) < 0x294 {
		return "", nil, fmt.Errorf("short WOW64 process parameters read: %v", err)
	}
	cmdLen := uintptr(*(*uint16)(unsafe.Pointer(&raw[0x40])))
	cmdBuf := uintptr(*(*uint32)(unsafe.Pointer(&raw[0x44])))
	envBuf := uintptr(*(*uint32)(unsafe.Pointer(&raw[0x48])))
	envSize := uintptr(*(*uint32)(unsafe.Pointer(&raw[0x290])))
	cmdRaw, err := readRemote(h, cmdBuf, cmdLen)
	if err != nil {
		return "", nil, err
	}
	envRaw, err := readRemote(h, envBuf, envSize)
	if err != nil {
		return "", nil, err
	}
	return windows.UTF16ToString(bytesToUTF16(cmdRaw)), bytesToUTF16(envRaw), nil
}

// Little endian bytes as UTF-16 code units
func bytesToUTF16(b []byte) []uint16 {
	out := make([]uint16, len(b)/2)
	for i := range out {
		out[i] = uint16(b[2*i]) | uint16(b[2*i+1])<<8
	}
	return out
}

// Retries a remote memory read on ERROR_PARTIAL_COPY for about a second like psutil, then reports access denied
func retryPartialCopy[T any](pid int, read func() (T, error)) (T, error) {
	delay := 100 * time.Microsecond
	for range 33 {
		v, err := read()
		if !errors.Is(err, windows.ERROR_PARTIAL_COPY) {
			return v, err
		}
		time.Sleep(delay)
		delay = min(delay*2, 40*time.Millisecond)
	}
	var zero T
	return zero, fmt.Errorf("process %d memory stayed partially copied: %w", pid, windows.ERROR_ACCESS_DENIED)
}

// Environment of pid read from its process parameters like psutil environ
func Environ(pid int) (map[string]string, error) {
	return retryPartialCopy(pid, func() (map[string]string, error) {
		_, block, err := pebStrings(pid)
		if err != nil {
			return nil, err
		}
		return parseEnvironBlock(string(utf16.Decode(block))), nil
	})
}

// Argument vector of pid like psutil cmdline, the PEB first and the limited query when the PEB is denied
func Cmdline(pid int) ([]string, error) {
	return retryPartialCopy(pid, func() ([]string, error) {
		cmdline, _, err := pebStrings(pid)
		if err != nil && permissionError(err) {
			cmdline, err = queriedCommandLine(pid)
		}
		if err != nil {
			return nil, err
		}
		return splitCommandLine(cmdline)
	})
}

// Command line of pid from NtQueryInformationProcess, which needs only limited query access
func queriedCommandLine(pid int) (string, error) {
	h, err := OpenProcess(pid, windows.PROCESS_QUERY_LIMITED_INFORMATION)
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(h)
	size := uint32(4096)
	for range 8 {
		buf := make([]byte, size)
		var ret uint32
		err := windows.NtQueryInformationProcess(h, windows.ProcessCommandLineInformation, unsafe.Pointer(&buf[0]), size, &ret)
		if errors.Is(err, windows.STATUS_INFO_LENGTH_MISMATCH) || errors.Is(err, windows.STATUS_BUFFER_TOO_SMALL) || errors.Is(err, windows.STATUS_BUFFER_OVERFLOW) {
			size = max(ret, size*2)
			continue
		}
		if err != nil {
			return "", err
		}
		return (*windows.NTUnicodeString)(unsafe.Pointer(&buf[0])).String(), nil
	}
	return "", fmt.Errorf("command line of process %d kept outgrowing %d bytes", pid, size)
}

// Splits a command line the way CommandLineToArgvW does
func splitCommandLine(cmdline string) ([]string, error) {
	if cmdline == "" {
		return []string{}, nil
	}
	ptr, err := windows.UTF16PtrFromString(cmdline)
	if err != nil {
		return nil, err
	}
	var argc int32
	argv, err := windows.CommandLineToArgv(ptr, &argc)
	if err != nil {
		return nil, err
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(argv)))
	out := make([]string, 0, argc)
	for i := 0; i < int(argc); i++ {
		out = append(out, windows.UTF16PtrToString(&argv[i][0]))
	}
	return out, nil
}

// Every pid in a toolhelp snapshot like psutil pids
func listPIDs() ([]int, error) {
	t, err := snapshot()
	if err != nil {
		return nil, err
	}
	pids := make([]int, 0, len(t))
	for _, p := range t {
		pids = append(pids, p.PID)
	}
	return pids, nil
}
