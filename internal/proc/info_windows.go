package proc

import (
	"errors"
	"fmt"
	"path/filepath"
	"time"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Strings a process keeps in its RTL_USER_PROCESS_PARAMETERS block
type processParams struct {
	cmdline string
	cwd     string
	env     []uint16
}

// PROCESS_BASIC_INFORMATION with every address held as an integer, never as a Go pointer
type basicInformation struct {
	ExitStatus                   int32
	PebBaseAddress               uintptr
	AffinityMask                 uintptr
	BasePriority                 int32
	UniqueProcessID              uintptr
	InheritedFromUniqueProcessID uintptr
}

// Native word of raw at off, an address in the target process
func wordAt(raw []byte, off uintptr) uintptr { return *(*uintptr)(unsafe.Pointer(&raw[off])) }

// Size bytes of remote memory at base, ERROR_PARTIAL_COPY when the read comes back short
func readRemote(h windows.Handle, base, size uintptr) ([]byte, error) {
	if size == 0 {
		return []byte{}, nil
	}
	buf := make([]byte, size)
	var got uintptr
	if err := windows.ReadProcessMemory(h, base, &buf[0], size, &got); err != nil {
		return nil, err
	}
	if got < size {
		return nil, windows.ERROR_PARTIAL_COPY
	}
	return buf, nil
}

// Little endian bytes as UTF-16 code units
func utf16Units(b []byte) []uint16 {
	out := make([]uint16, len(b)/2)
	for i := range out {
		out[i] = uint16(b[2*i]) | uint16(b[2*i+1])<<8
	}
	return out
}

// Remote UTF-16 string of length bytes at base
func remoteString(h windows.Handle, base, length uintptr) (string, error) {
	raw, err := readRemote(h, base, length)
	if err != nil {
		return "", err
	}
	return string(utf16.Decode(utf16Units(raw))), nil
}

// Process parameters of pid, read through the WOW64 layout for a 32 bit target
func readParams(pid int) (processParams, error) {
	h, err := openProcess(pid, windows.PROCESS_QUERY_INFORMATION|windows.PROCESS_VM_READ)
	if err != nil {
		return processParams{}, err
	}
	defer windows.CloseHandle(h)
	var targetWow, selfWow bool
	if err := windows.IsWow64Process(h, &targetWow); err != nil {
		return processParams{}, err
	}
	if err := windows.IsWow64Process(windows.CurrentProcess(), &selfWow); err != nil {
		return processParams{}, err
	}
	if targetWow && !selfWow {
		return readParams32(h)
	}
	var pbi basicInformation
	var ret uint32
	if err := windows.NtQueryInformationProcess(h, windows.ProcessBasicInformation, unsafe.Pointer(&pbi), uint32(unsafe.Sizeof(pbi)), &ret); err != nil {
		return processParams{}, err
	}
	raw, err := readRemote(h, pbi.PebBaseAddress+unsafe.Offsetof(windows.PEB{}.ProcessParameters), unsafe.Sizeof(uintptr(0)))
	if err != nil {
		return processParams{}, err
	}
	base := wordAt(raw, 0)
	var layout windows.RTL_USER_PROCESS_PARAMETERS
	offCwd := unsafe.Offsetof(layout.CurrentDirectory) + unsafe.Offsetof(layout.CurrentDirectory.DosPath)
	offCmd := unsafe.Offsetof(layout.CommandLine)
	offEnv, offEnvSize := unsafe.Offsetof(layout.Environment), unsafe.Offsetof(layout.EnvironmentSize)
	offBuf := unsafe.Offsetof(layout.CommandLine.Buffer)
	if raw, err = readRemote(h, base, offEnvSize+unsafe.Sizeof(uintptr(0))); err != nil {
		return processParams{}, err
	}
	length := func(off uintptr) uintptr { return uintptr(*(*uint16)(unsafe.Pointer(&raw[off]))) }
	var p processParams
	if p.cmdline, err = remoteString(h, wordAt(raw, offCmd+offBuf), length(offCmd)); err != nil {
		return processParams{}, err
	}
	if p.cwd, err = remoteString(h, wordAt(raw, offCwd+offBuf), length(offCwd)); err != nil {
		return processParams{}, err
	}
	envRaw, err := readRemote(h, wordAt(raw, offEnv), wordAt(raw, offEnvSize))
	if err != nil {
		return processParams{}, err
	}
	p.env = utf16Units(envRaw)
	return p, nil
}

// Process parameters of a 32 bit target read at the fixed WOW64 offsets
func readParams32(h windows.Handle) (processParams, error) {
	var peb32 uintptr
	var ret uint32
	if err := windows.NtQueryInformationProcess(h, windows.ProcessWow64Information, unsafe.Pointer(&peb32), uint32(unsafe.Sizeof(peb32)), &ret); err != nil {
		return processParams{}, err
	}
	raw, err := readRemote(h, peb32+0x10, 4)
	if err != nil {
		return processParams{}, err
	}
	base := uintptr(*(*uint32)(unsafe.Pointer(&raw[0])))
	raw, err = readRemote(h, base, 0x294)
	if err != nil {
		return processParams{}, err
	}
	u16 := func(off int) uintptr { return uintptr(*(*uint16)(unsafe.Pointer(&raw[off]))) }
	u32 := func(off int) uintptr { return uintptr(*(*uint32)(unsafe.Pointer(&raw[off]))) }
	var p processParams
	if p.cwd, err = remoteString(h, u32(0x28), u16(0x24)); err != nil {
		return processParams{}, err
	}
	if p.cmdline, err = remoteString(h, u32(0x44), u16(0x40)); err != nil {
		return processParams{}, err
	}
	envRaw, err := readRemote(h, u32(0x48), u32(0x290))
	if err != nil {
		return processParams{}, err
	}
	p.env = utf16Units(envRaw)
	return p, nil
}

// Retries a remote read that raced the target writing its parameters for about a second, then reports access denied
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
	return zero, denied(pid, windows.ERROR_PARTIAL_COPY)
}

// Command line of pid through the limited query right alone, for targets that refuse memory reads
func queriedCommandLine(pid int) (string, error) {
	h, err := openProcess(pid, windows.PROCESS_QUERY_LIMITED_INFORMATION)
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
			return "", fmt.Errorf("pid %d: %w", pid, err)
		}
		return (*windows.NTUnicodeString)(unsafe.Pointer(&buf[0])).String(), nil
	}
	return "", fmt.Errorf("pid %d: command line kept outgrowing %d bytes", pid, size)
}

// Words of a command line split the way CommandLineToArgvW splits them
func splitCommandLine(line string) ([]string, error) {
	if line == "" {
		return []string{}, nil
	}
	ptr, err := windows.UTF16PtrFromString(line)
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
	for i := range int(argc) {
		out = append(out, windows.UTF16PtrToString(&argv[i][0]))
	}
	return out, nil
}

// Argument vector of pid, the process parameters first and the limited query when memory reads are refused
func Cmdline(pid int) ([]string, error) {
	return retryPartialCopy(pid, func() ([]string, error) {
		p, err := readParams(pid)
		line := p.cmdline
		if errors.Is(err, ErrAccessDenied) {
			line, err = queriedCommandLine(pid)
		}
		if err != nil {
			return nil, err
		}
		return splitCommandLine(line)
	})
}

// Working directory of pid, cleaned of a trailing separator
func Cwd(pid int) (string, error) {
	return retryPartialCopy(pid, func() (string, error) {
		p, err := readParams(pid)
		if err != nil {
			return "", err
		}
		return filepath.Clean(p.cwd), nil
	})
}

// Environment of pid with names upper cased, Windows treating them without regard to case
func Environ(pid int) (map[string]string, error) {
	return retryPartialCopy(pid, func() (map[string]string, error) {
		p, err := readParams(pid)
		if err != nil {
			return nil, err
		}
		return parseEnviron(string(utf16.Decode(p.env)), true), nil
	})
}

// Executable image path of pid
func Exe(pid int) (string, error) {
	h, err := openProcess(pid, windows.PROCESS_QUERY_LIMITED_INFORMATION)
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(h)
	buf := make([]uint16, windows.MAX_LONG_PATH)
	size := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &size); err != nil {
		return "", fmt.Errorf("pid %d: %w", pid, err)
	}
	return windows.UTF16ToString(buf[:size]), nil
}

// Image name of pid as Task Manager shows it, read from the native list when the image path is refused
func Name(pid int) (string, error) {
	switch pid {
	case 0:
		return "System Idle Process", nil
	case 4:
		return "System", nil
	}
	exe, err := Exe(pid)
	if errors.Is(err, ErrAccessDenied) {
		p, lerr := systemProcess(pid)
		if lerr != nil {
			return "", lerr
		}
		return p.name, nil
	}
	if err != nil {
		return "", err
	}
	return filepath.Base(exe), nil
}

// Account owning pid as DOMAIN\user from its process token
func Owner(pid int) (string, error) {
	if pid == 0 || pid == 4 {
		return `NT AUTHORITY\SYSTEM`, nil
	}
	h, err := openProcess(pid, windows.PROCESS_QUERY_LIMITED_INFORMATION)
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(h)
	var tok windows.Token
	if err := windows.OpenProcessToken(h, windows.TOKEN_QUERY, &tok); err != nil {
		if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			return "", denied(pid, err)
		}
		return "", fmt.Errorf("pid %d: %w", pid, err)
	}
	defer tok.Close()
	tu, err := tok.GetTokenUser()
	if err != nil {
		return "", fmt.Errorf("pid %d: %w", pid, err)
	}
	account, domain, _, err := tu.User.Sid.LookupAccount("")
	if err != nil {
		return "", fmt.Errorf("pid %d: %w", pid, err)
	}
	return domain + `\` + account, nil
}

// Memory, thread, CPU and handle use of pid from the native system process list
func StatsOf(pid int) (Stats, error) {
	p, err := systemProcess(pid)
	if err != nil {
		return Stats{}, err
	}
	return Stats{
		RSS:         p.rss,
		VMS:         p.vms,
		Threads:     p.threads,
		User:        time.Duration(p.user) * 100,
		System:      time.Duration(p.kernel) * 100,
		Descriptors: p.handles,
	}, nil
}
