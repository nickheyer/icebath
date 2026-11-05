package hostinfo

import (
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const (
	cpuKey          = `HARDWARE\DESCRIPTION\System\CentralProcessor\0`
	displayClassKey = `SYSTEM\CurrentControlSet\Control\Class\{4d36e968-e325-11ce-bfc1-08002be10318}`
)

var (
	modKernel32          = windows.NewLazySystemDLL("kernel32.dll")
	procIsWow64Process2  = modKernel32.NewProc("IsWow64Process2")
	procProcessIDToSess  = modKernel32.NewProc("ProcessIdToSessionId")
	procActiveConsoleSID = modKernel32.NewProc("WTSGetActiveConsoleSessionId")
	procGetWindowStation = windows.NewLazySystemDLL("user32.dll").NewProc("GetProcessWindowStation")
)

// IsWow64Process2 native machine, zero when unavailable
func wow64NativeMachine() int {
	if err := procIsWow64Process2.Find(); err != nil {
		return 0
	}
	var processMachine, nativeMachine uint16
	r, _, _ := procIsWow64Process2.Call(uintptr(windows.CurrentProcess()), uintptr(unsafe.Pointer(&processMachine)), uintptr(unsafe.Pointer(&nativeMachine)))
	if r == 0 {
		return 0
	}
	return int(nativeMachine)
}

// Machine name the way platform.machine reports it, from the environment the OS sets for every process
func windowsMachineName() string {
	if arch, ok := syscall.Getenv("PROCESSOR_ARCHITEW6432"); ok && arch != "" {
		return arch
	}
	arch, _ := syscall.Getenv("PROCESSOR_ARCHITECTURE")
	return arch
}

func nativeArchImpl() string { return WindowsNativeArch(wow64NativeMachine(), windowsMachineName()) }

// String registry value under HKLM, empty when missing
func winregStr(subkey, name string) string {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, subkey, registry.QUERY_VALUE)
	if err != nil {
		return ""
	}
	defer k.Close()
	v, _, err := k.GetStringValue(name)
	if err != nil {
		return ""
	}
	return v
}

func cpuModelImpl() string  { return winregStr(cpuKey, "ProcessorNameString") }
func cpuVendorImpl() string { return winregStr(cpuKey, "VendorIdentifier") }

func ramTotalImpl() int64 {
	var status struct {
		Length, MemoryLoad                                                                                   uint32
		TotalPhys, AvailPhys, TotalPageFile, AvailPageFile, TotalVirtual, AvailVirtual, AvailExtendedVirtual uint64
	}
	status.Length = uint32(unsafe.Sizeof(status))
	r, _, _ := modKernel32.NewProc("GlobalMemoryStatusEx").Call(uintptr(unsafe.Pointer(&status)))
	if r == 0 {
		return 0
	}
	return int64(status.TotalPhys)
}

// displayAdapterScan is the cached display class scan with whether the key was readable
type displayAdapterScan struct {
	adapters []DisplayAdapter
	ok       bool
}

var displayAdaptersOnce = cached(func() displayAdapterScan {
	adapters, ok := windowsDisplayAdaptersImpl()
	return displayAdapterScan{adapters, ok}
})

// Display class registry rows parsed into adapters, false when the class key cannot be opened
func windowsDisplayAdaptersImpl() ([]DisplayAdapter, bool) {
	root, err := registry.OpenKey(registry.LOCAL_MACHINE, displayClassKey, registry.ENUMERATE_SUB_KEYS)
	if err != nil {
		return nil, false
	}
	defer root.Close()
	names, err := root.ReadSubKeyNames(gpuScanLimit)
	if err != nil && len(names) == 0 {
		return nil, false
	}
	var rows [][3]string
	for _, subkey := range names {
		path := displayClassKey + `\` + subkey
		rows = append(rows, [3]string{subkey, winregStr(path, "MatchingDeviceId"), winregStr(path, "DriverDesc")})
	}
	return ParseWindowsDisplayAdapters(rows), true
}

func gpuClassImpl() string {
	scan := displayAdaptersOnce()
	if !scan.ok {
		return "unknown"
	}
	vendors := make([]string, len(scan.adapters))
	for i, a := range scan.adapters {
		vendors[i] = a.VendorID
	}
	return ClassifyGPUVendors(vendors)
}

func nvidiaGPUsImpl() []NvidiaGPU {
	return NvidiaGPUsFromAdapters(displayAdaptersOnce().adapters)
}

func interactiveSessionImpl() bool {
	var sessionID uint32
	r, _, _ := procProcessIDToSess.Call(uintptr(windows.GetCurrentProcessId()), uintptr(unsafe.Pointer(&sessionID)))
	if r == 0 || sessionID == 0 {
		return false
	}
	active, _, _ := procActiveConsoleSID.Call()
	if uint32(active) != 0xFFFFFFFF && uint32(active) == sessionID {
		return true
	}
	station, _, _ := procGetWindowStation.Call()
	return station != 0
}
