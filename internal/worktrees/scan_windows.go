package worktrees

import (
	"errors"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Hands each running process's folder, read from its process parameters, to see until it says it found what it looked for
func scan(see func(pid int, place string) bool) {
	ids := make([]uint32, 4096)
	var returned uint32
	if err := windows.EnumProcesses(ids, &returned); err != nil {
		return
	}
	for _, id := range ids[:returned/4] {
		if id == 0 {
			continue
		}
		if cwd, err := folderOf(id); err == nil && cwd != "" {
			see(int(id), filepath.Clean(cwd))
		}
	}
}

// Reads memory of another process
func readRemote(process windows.Handle, address, size uintptr) ([]byte, error) {
	if size == 0 {
		return nil, errors.New("nothing to read")
	}
	buf := make([]byte, size)
	var read uintptr
	if err := windows.ReadProcessMemory(process, address, &buf[0], size, &read); err != nil {
		return nil, err
	}
	return buf[:read], nil
}

// Current folder of the process from its process parameters, through the WOW64 block for a 32 bit process seen from a 64 bit one
func folderOf(id uint32) (string, error) {
	process, err := windows.OpenProcess(windows.PROCESS_QUERY_INFORMATION|windows.PROCESS_VM_READ, false, id)
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(process)
	var target, self bool
	windows.IsWow64Process(process, &target)
	windows.IsWow64Process(windows.CurrentProcess(), &self)
	var length, buffer uintptr
	if target && !self {
		var peb32 uintptr
		var size uint32
		if err := windows.NtQueryInformationProcess(process, windows.ProcessWow64Information, unsafe.Pointer(&peb32), uint32(unsafe.Sizeof(peb32)), &size); err != nil {
			return "", err
		}
		raw, err := readRemote(process, peb32+0x10, 4)
		if err != nil || len(raw) < 4 {
			return "", errors.New("short WOW64 environment block read")
		}
		raw, err = readRemote(process, uintptr(*(*uint32)(unsafe.Pointer(&raw[0]))), 0x2c)
		if err != nil || len(raw) < 0x2c {
			return "", errors.New("short WOW64 process parameters read")
		}
		length = uintptr(*(*uint16)(unsafe.Pointer(&raw[0x24])))
		buffer = uintptr(*(*uint32)(unsafe.Pointer(&raw[0x28])))
	} else {
		var info windows.PROCESS_BASIC_INFORMATION
		var size uint32
		if err := windows.NtQueryInformationProcess(process, windows.ProcessBasicInformation, unsafe.Pointer(&info), uint32(unsafe.Sizeof(info)), &size); err != nil {
			return "", err
		}
		raw, err := readRemote(process, uintptr(unsafe.Pointer(info.PebBaseAddress))+unsafe.Offsetof(windows.PEB{}.ProcessParameters), unsafe.Sizeof(uintptr(0)))
		if err != nil || len(raw) < int(unsafe.Sizeof(uintptr(0))) {
			return "", errors.New("short environment block read")
		}
		var params windows.RTL_USER_PROCESS_PARAMETERS
		raw, err = readRemote(process, *(*uintptr)(unsafe.Pointer(&raw[0])), unsafe.Sizeof(params))
		if err != nil || len(raw) < int(unsafe.Sizeof(params)) {
			return "", errors.New("short process parameters read")
		}
		params = *(*windows.RTL_USER_PROCESS_PARAMETERS)(unsafe.Pointer(&raw[0]))
		length = uintptr(params.CurrentDirectory.DosPath.Length)
		buffer = uintptr(unsafe.Pointer(params.CurrentDirectory.DosPath.Buffer))
	}
	raw, err := readRemote(process, buffer, length)
	if err != nil {
		return "", err
	}
	text := make([]uint16, len(raw)/2)
	for i := range text {
		text[i] = uint16(raw[2*i]) | uint16(raw[2*i+1])<<8
	}
	return windows.UTF16ToString(text), nil
}
