package hostinfo

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const (
	displayClassKey   = `SYSTEM\CurrentControlSet\Control\Class\{4d36e968-e325-11ce-bfc1-08002be10318}`
	dxgiSoftwareFlag  = 0x2
	comRelease        = 2
	dxgiEnumAdapters1 = 12
	dxgiGetDesc1      = 10
)

var (
	dxgiDLL                = windows.NewLazySystemDLL("dxgi.dll")
	procCreateDXGIFactory1 = dxgiDLL.NewProc("CreateDXGIFactory1")
	iidDXGIFactory1        = windows.GUID{Data1: 0x770AAE78, Data2: 0xF26F, Data3: 0x4DBA,
		Data4: [8]byte{0xA8, 0x29, 0x25, 0x3C, 0x83, 0xD1, 0xB3, 0x87}}
)

// Facts kept for Windows, the display class adapters and the DXGI adapters
type nativeCache struct {
	displays memo[[]displayAdapter]
	dxgi     memo[[]DXGIAdapter]
}

// One display class adapter of the registry, vendor and device empty when unreadable
type displayAdapter struct {
	vendor      string
	device      string
	description string
}

// COM object whose first word points at its method table
type comObject struct{ vtable *[16]uintptr }

// Adapter description DXGI fills, the fields nothing reads left blank
type dxgiAdapterDesc1 struct {
	description    [128]uint16
	vendorID       uint32
	_              [3]uint32
	dedicatedVideo uintptr
	_              uintptr
	sharedSystem   uintptr
	_              [2]uint32
	flags          uint32
}

// Places nvidia-smi lives outside PATH, the DCH driver System32 copy then the legacy NVSMI folder
func smiCandidates() []string {
	root, ok := os.LookupEnv("SystemRoot")
	if !ok {
		root = `C:\Windows`
	}
	programFiles, ok := os.LookupEnv("ProgramFiles")
	if !ok {
		programFiles = `C:\Program Files`
	}
	return []string{filepath.Join(root, "System32", "nvidia-smi.exe"),
		filepath.Join(programFiles, "NVIDIA Corporation", "NVSMI", "nvidia-smi.exe")}
}

// Vendors of the display class adapters, unknown when the class key cannot be read
func gpuClass(h *Host) GPUClass {
	adapters, err := h.native.displays.get(readDisplayAdapters)
	if err != nil {
		return GPUUnknown
	}
	vendors := make([]string, len(adapters))
	for i, a := range adapters {
		vendors[i] = a.vendor
	}
	return classifyGPUVendors(vendors)
}

// NVIDIA entries of the display class adapters, none when the class key cannot be read
func nvidiaGPUs(h *Host) []nvidiaGPU {
	adapters, err := h.native.displays.get(readDisplayAdapters)
	if err != nil {
		return nil
	}
	var gpus []nvidiaGPU
	for _, a := range adapters {
		if strings.EqualFold(a.vendor, "10de") {
			gpus = append(gpus, nvidiaGPU{Name: a.description, PCIID: nvidiaPCIID(a.device)})
		}
	}
	return gpus
}

// Display class subkeys with a PCI id, plus numbered subkeys without one, virtual adapters skipped
func readDisplayAdapters() ([]displayAdapter, error) {
	root, err := registry.OpenKey(registry.LOCAL_MACHINE, displayClassKey, registry.ENUMERATE_SUB_KEYS)
	if err != nil {
		return nil, fmt.Errorf("display adapters: %w", err)
	}
	defer root.Close()
	names, err := root.ReadSubKeyNames(gpuScanLimit)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("display adapters: %w", err)
	}
	var adapters []displayAdapter
	for _, subkey := range names {
		path := displayClassKey + `\` + subkey
		matching, description := registryString(path, "MatchingDeviceId"), registryString(path, "DriverDesc")
		upper := strings.ToUpper(matching)
		switch {
		case strings.Contains(upper, "VEN_"):
			adapters = append(adapters, displayAdapter{hexAfter(upper, "VEN_"), hexAfter(upper, "DEV_"), description})
		case matching == "" && subkey != "" && strings.Trim(subkey, "0123456789") == "":
			adapters = append(adapters, displayAdapter{description: description})
		}
	}
	return adapters, nil
}

// Up to four characters after tag in a device id, empty when the tag is absent
func hexAfter(text, tag string) string {
	_, rest, found := strings.Cut(text, tag)
	if !found {
		return ""
	}
	return rest[:min(4, len(rest))]
}

// Every adapter DXGI enumerates in its order, read once
func (h *Host) DXGIAdapters() ([]DXGIAdapter, error) { return h.native.dxgi.get(readDXGIAdapters) }

// Adapters from CreateDXGIFactory1 and EnumAdapters1 until the list ends, an adapter without a description skipped
func readDXGIAdapters() ([]DXGIAdapter, error) {
	if err := procCreateDXGIFactory1.Find(); err != nil {
		return nil, fmt.Errorf("dxgi adapters: %w", err)
	}
	var factory *comObject
	if r, _, _ := procCreateDXGIFactory1.Call(uintptr(unsafe.Pointer(&iidDXGIFactory1)), uintptr(unsafe.Pointer(&factory))); int32(r) != 0 {
		return nil, fmt.Errorf("dxgi adapters: CreateDXGIFactory1 hresult %#x", uint32(r))
	}
	defer comCall(factory, comRelease)
	adapters := []DXGIAdapter{}
	for index := range gpuScanLimit {
		var adapter *comObject
		if comCall(factory, dxgiEnumAdapters1, uintptr(index), uintptr(unsafe.Pointer(&adapter))) != 0 {
			break
		}
		var desc dxgiAdapterDesc1
		hresult := comCall(adapter, dxgiGetDesc1, uintptr(unsafe.Pointer(&desc)))
		comCall(adapter, comRelease)
		if hresult != 0 {
			continue
		}
		adapters = append(adapters, DXGIAdapter{
			Description:    windows.UTF16ToString(desc.description[:]),
			VendorID:       desc.vendorID,
			DedicatedBytes: uint64(desc.dedicatedVideo),
			SharedBytes:    uint64(desc.sharedSystem),
			Software:       desc.flags&dxgiSoftwareFlag != 0,
		})
	}
	return adapters, nil
}

//go:uintptrescapes
func comCall(obj *comObject, slot int, args ...uintptr) int32 {
	r, _, _ := syscall.SyscallN(obj.vtable[slot], append([]uintptr{uintptr(unsafe.Pointer(obj))}, args...)...)
	return int32(r)
}
