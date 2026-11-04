//go:build windows

package tools

import (
	"errors"
	"fmt"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows half of the process identity layer, the kill on close job object and PEB reads

// Job handle kept alive for the life of the process so the kernel kills the job when it dies
var jobHandle windows.Handle

const stillActive = 259

// Places this process in a job that dies whole when it dies, idempotent
func attachSelfToKillOnCloseJob() bool {
	if jobHandle != 0 {
		return true
	}
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil || job == 0 {
		return false
	}
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE | windows.JOB_OBJECT_LIMIT_BREAKAWAY_OK | windows.JOB_OBJECT_LIMIT_SILENT_BREAKAWAY_OK
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		windows.CloseHandle(job)
		return false
	}
	if err := windows.AssignProcessToJobObject(job, windows.CurrentProcess()); err != nil {
		windows.CloseHandle(job)
		return false
	}
	jobHandle = job
	identityLogger().Debug("attached to kill-on-close job object")
	return true
}

// Windows has no process groups to probe, the pgid is read as a pid
func processGroupGone(pgid int) bool { return !(osProcessProber{}).Running(pgid) }

// Windows has no group signals, the pgid is terminated as a pid
func signalProcessGroup(pgid int, force bool) error { return (osProcessProber{}).Terminate(pgid) }

// The POSIX supervisor never runs on Windows, the job object owns the children
func startSupervisorProcess() (supervisorHandle, error) {
	return nil, errors.New("owner-death supervisor is POSIX only")
}

// Process facts read through the Win32 process API
type osProcessProber struct{}

// Opens pid for queries, errNoSuchProcess when it does not exist
func openProcess(pid int, access uint32) (windows.Handle, error) {
	if pid <= 0 {
		return 0, errNoSuchProcess
	}
	h, err := windows.OpenProcess(access, false, uint32(pid))
	if err != nil {
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			return 0, errNoSuchProcess
		}
		return 0, err
	}
	return h, nil
}

func (osProcessProber) Running(pid int) bool {
	h, err := openProcess(pid, windows.PROCESS_QUERY_LIMITED_INFORMATION)
	if err != nil {
		return errors.Is(err, windows.ERROR_ACCESS_DENIED)
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return true
	}
	return code == stillActive
}

func (osProcessProber) Birth(pid int) (float64, error) {
	h, err := openProcess(pid, windows.PROCESS_QUERY_LIMITED_INFORMATION)
	if err != nil {
		return 0, err
	}
	defer windows.CloseHandle(h)
	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &creation, &exit, &kernel, &user); err != nil {
		return 0, err
	}
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err == nil && code != stillActive {
		return 0, errNoSuchProcess
	}
	return float64(creation.Nanoseconds()) / 1e9, nil
}

func (p osProcessProber) WorkerBirth(pid int) (float64, error) { return p.Birth(pid) }

func (osProcessProber) BootStart(pid int) (float64, bool) { return 0, false }

func (osProcessProber) Zombie(pid int) bool { return false }

// Parent of every live process from a toolhelp snapshot
func toolhelpParents() (map[int]int, error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(snap)
	var entry windows.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	parents := map[int]int{}
	if err := windows.Process32First(snap, &entry); err != nil {
		return nil, err
	}
	for {
		parents[int(entry.ProcessID)] = int(entry.ParentProcessID)
		if err := windows.Process32Next(snap, &entry); err != nil {
			break
		}
	}
	return parents, nil
}

func (osProcessProber) PPID(pid int) (int, bool) {
	parents, err := toolhelpParents()
	if err != nil {
		return 0, false
	}
	ppid, ok := parents[pid]
	return ppid, ok
}

func (osProcessProber) Children(pid int, recursive bool) []int {
	parents, err := toolhelpParents()
	if err != nil {
		return nil
	}
	kids := map[int][]int{}
	for child, parent := range parents {
		kids[parent] = append(kids[parent], child)
	}
	if !recursive {
		return kids[pid]
	}
	seen := map[int]bool{pid: true}
	out := []int{}
	queue := []int{pid}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, k := range kids[cur] {
			if seen[k] {
				continue
			}
			seen[k] = true
			out = append(out, k)
			queue = append(queue, k)
		}
	}
	return out
}

func (osProcessProber) Terminate(pid int) error {
	h, err := openProcess(pid, windows.PROCESS_TERMINATE)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	return windows.TerminateProcess(h, 15)
}

func (p osProcessProber) Kill(pid int) error { return p.Terminate(pid) }

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
	h, err := openProcess(pid, windows.PROCESS_QUERY_INFORMATION|windows.PROCESS_VM_READ)
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

func bytesToUTF16(b []byte) []uint16 {
	out := make([]uint16, len(b)/2)
	for i := range out {
		out[i] = uint16(b[2*i]) | uint16(b[2*i+1])<<8
	}
	return out
}

func (osProcessProber) Environ(pid int) (map[string]string, error) {
	_, block, err := pebStrings(pid)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	start := 0
	for i, u := range block {
		if u != 0 {
			continue
		}
		if i == start {
			break
		}
		kv := windows.UTF16ToString(block[start:i])
		start = i + 1
		if k, v, ok := strings.Cut(kv, "="); ok && k != "" {
			out[k] = v
		}
	}
	return out, nil
}

func (osProcessProber) Cmdline(pid int) ([]string, error) {
	cmdline, _, err := pebStrings(pid)
	if err != nil {
		return nil, err
	}
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
