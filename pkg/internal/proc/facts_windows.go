package proc

import (
	"errors"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows reads every process cwd from its process parameters
const CwdAvailable = true

// Working directory of pid from the PEB process parameters, normalized like psutil
func Cwd(pid int) (string, error) {
	return retryPartialCopy(pid, func() (string, error) {
		raw, err := pebCwd(pid)
		if err != nil {
			return "", err
		}
		return filepath.Clean(raw), nil
	})
}

// Current directory DosPath of pid read through the native or WOW64 parameter block
func pebCwd(pid int) (string, error) {
	h, err := OpenProcess(pid, windows.PROCESS_QUERY_INFORMATION|windows.PROCESS_VM_READ)
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(h)
	var targetWow, selfWow bool
	windows.IsWow64Process(h, &targetWow)
	windows.IsWow64Process(windows.CurrentProcess(), &selfWow)
	var length, buffer uintptr
	if targetWow && !selfWow {
		var peb32 uintptr
		var ret uint32
		if err := windows.NtQueryInformationProcess(h, windows.ProcessWow64Information, unsafe.Pointer(&peb32), uint32(unsafe.Sizeof(peb32)), &ret); err != nil {
			return "", err
		}
		raw, err := readRemote(h, peb32+0x10, 4)
		if err != nil {
			return "", err
		}
		raw, err = readRemote(h, uintptr(*(*uint32)(unsafe.Pointer(&raw[0]))), 0x2c)
		if err != nil || len(raw) < 0x2c {
			return "", errors.New("short WOW64 process parameters read")
		}
		length = uintptr(*(*uint16)(unsafe.Pointer(&raw[0x24])))
		buffer = uintptr(*(*uint32)(unsafe.Pointer(&raw[0x28])))
	} else {
		var pbi windows.PROCESS_BASIC_INFORMATION
		var ret uint32
		if err := windows.NtQueryInformationProcess(h, windows.ProcessBasicInformation, unsafe.Pointer(&pbi), uint32(unsafe.Sizeof(pbi)), &ret); err != nil {
			return "", err
		}
		raw, err := readRemote(h, uintptr(unsafe.Pointer(pbi.PebBaseAddress))+unsafe.Offsetof(windows.PEB{}.ProcessParameters), unsafe.Sizeof(uintptr(0)))
		if err != nil {
			return "", err
		}
		var params windows.RTL_USER_PROCESS_PARAMETERS
		raw, err = readRemote(h, *(*uintptr)(unsafe.Pointer(&raw[0])), unsafe.Sizeof(params))
		if err != nil {
			return "", err
		}
		params = *(*windows.RTL_USER_PROCESS_PARAMETERS)(unsafe.Pointer(&raw[0]))
		length = uintptr(params.CurrentDirectory.DosPath.Length)
		buffer = uintptr(unsafe.Pointer(params.CurrentDirectory.DosPath.Buffer))
	}
	raw, err := readRemote(h, buffer, length)
	if err != nil {
		return "", err
	}
	return windows.UTF16ToString(bytesToUTF16(raw)), nil
}

// Windows processes carry no POSIX uid
func RealUID(pid int) (int, error) {
	return 0, errors.New("real uid is a POSIX process attribute")
}

// Image basename of pid like psutil name, the full image path read with limited query rights
func Name(pid int) (string, error) {
	h, err := OpenProcess(pid, windows.PROCESS_QUERY_LIMITED_INFORMATION)
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(h)
	buf := make([]uint16, windows.MAX_LONG_PATH)
	size := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &size); err != nil {
		return "", err
	}
	return filepath.Base(windows.UTF16ToString(buf[:size])), nil
}

// Ends pid with TerminateProcess, psutil terminate on Windows
func Terminate(pid int) error { return killPID(pid) }
